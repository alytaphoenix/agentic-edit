// Package intent parses a natural-language instruction into a small, typed set
// of edit operations. The resulting Ops are validated and then rendered into
// executable awk/sed/grep commands by the compile package, so the only way to
// produce an operation is through a fixed, safe template.
package intent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Kind identifies the underlying utility an Op drives.
type Kind string

const (
	KindSed  Kind = "sed"
	KindAwk  Kind = "awk"
	KindGrep Kind = "grep"
)

// Op is a single, typed edit operation. The fields that are set depend on Kind:
//
//	sed  "replace": X, Y, Scope ("/addr/", optional), Global
//	sed  "delete":  Pattern
//	sed  "insert":  Anchor, Text, InsertBefore
//	awk  "transform": Transform (one of the known transform names)
//	grep "find":    Pattern
type Op struct {
	Kind Kind `json:"kind"`

	// sed replace.
	X      string `json:"x,omitempty"`
	Y      string `json:"y,omitempty"`
	Scope  string `json:"scope,omitempty"` // "" or "/addr/"
	Global bool   `json:"global,omitempty"`

	// Replace semantics. Literal is the safe default: X is matched as a plain
	// string. Set Regex to opt into sed regular-expression matching. ExpectCount
	// > 0 requires exactly that many matches before the edit is allowed.
	Regex       bool `json:"regex,omitempty"`
	ExpectCount int  `json:"expect_count,omitempty"`

	// sed delete / grep find.
	Pattern string `json:"pattern,omitempty"`

	// sed insert.
	Anchor       string `json:"anchor,omitempty"`
	Text         string `json:"text,omitempty"`
	InsertBefore bool   `json:"insert_before,omitempty"`

	// awk transform ("upper" | "lower").
	Transform string `json:"transform,omitempty"`
}

// knownTransforms is the fixed set of awk transforms the compiler supports.
var knownTransforms = map[string]bool{"upper": true, "lower": true}

// sedAction reports whether a sed Op is a replace, insert, or delete.
func (o Op) sedAction() string {
	if o.X != "" || o.Y != "" {
		return "replace"
	}
	if o.Anchor != "" && o.Text != "" {
		return "insert"
	}
	return "delete"
}

// Validate enforces the structural constraints that make an Op safe to compile:
// a known kind, a known awk transform, single-line values, and required fields.
func (o Op) Validate() error {
	switch o.Kind {
	case KindSed:
		if strings.ContainsAny(o.X, "\r\n") || strings.ContainsAny(o.Y, "\r\n") {
			return fmt.Errorf("sed replace values must be single-line")
		}
		if o.ExpectCount < 0 {
			return fmt.Errorf("expect_count must not be negative")
		}
		if len(o.X) > 64*1024 || len(o.Y) > 64*1024 || len(o.Pattern) > 64*1024 {
			return fmt.Errorf("sed value exceeds the 64 KiB limit")
		}
		if o.SedAction() == "replace" {
			if o.X == "" {
				return fmt.Errorf("sed replace requires a non-empty value to replace")
			}
			if o.Scope != "" && !isAddr(o.Scope) {
				return fmt.Errorf("invalid sed address %q", o.Scope)
			}
			if o.Regex {
				if err := validateRegex(o.X); err != nil {
					return err
				}
			}
		}
		if o.SedAction() == "delete" && o.Pattern == "" {
			return fmt.Errorf("sed delete requires a pattern")
		}
		if strings.ContainsAny(o.Pattern, "\r\n") {
			return fmt.Errorf("sed pattern must be single-line")
		}
		if o.Anchor != "" && strings.ContainsAny(o.Anchor, "\r\n") {
			return fmt.Errorf("sed anchor must be single-line")
		}
		if o.Text != "" && strings.ContainsAny(o.Text, "\r\n") {
			return fmt.Errorf("inserted text must be single-line")
		}
	case KindAwk:
		if !knownTransforms[o.Transform] {
			return fmt.Errorf("unknown awk transform %q", o.Transform)
		}
	case KindGrep:
		if o.Pattern == "" {
			return fmt.Errorf("grep requires a pattern")
		}
		if strings.ContainsAny(o.Pattern, "\r\n") {
			return fmt.Errorf("grep pattern must be single-line")
		}
	default:
		return fmt.Errorf("unknown op kind %q", o.Kind)
	}
	return nil
}

// SedAction is a small accessor so callers/tests can branch on the sed action.
func (o Op) SedAction() string { return o.sedAction() }

func isAddr(s string) bool {
	if len(s) < 2 || len(s) > 1024 || s[0] != '/' || s[len(s)-1] != '/' {
		return false
	}
	// A sed address is a regex delimited by slashes. Semicolon, braces, bang,
	// and hash are command separators/control characters in sed; allowing them
	// inside the "address" let an LLM emit something like "/x/;e id;/y/" and
	// execute a shell command on GNU sed. Reject them outright.
	if strings.ContainsAny(s, "\r\n;{}!#\\") {
		return false
	}
	return true
}

// nonPortableRegex matches constructs that Go's regexp accepts but POSIX ERE
// (sed -E) does not. The count guard is evaluated with Go's regexp, so allowing
// these would let the guard and sed disagree about what matches — exactly the
// kind of divergence that turns a "unique replace" into a broad one.
var nonPortableRegex = regexp.MustCompile(`\\[dDwWsSbBAzZpPxQE]|\(\?`)

// validateRegex rejects a regex that sed -E and the Go count guard would
// interpret differently.
func validateRegex(p string) error {
	if nonPortableRegex.MatchString(p) {
		return fmt.Errorf("regex %q uses a construct that is not portable to sed -E", p)
	}
	if _, err := regexp.Compile(p); err != nil {
		return fmt.Errorf("invalid regex %q: %v", p, err)
	}
	return nil
}

// Parse turns a natural-language instruction into a list of typed Ops using a
// small keyword grammar. It never reaches for an LLM; the compiler renders the
// Ops into safe commands.
func Parse(description string, files []string) ([]Op, error) {
	d := strings.TrimSpace(description)
	low := strings.ToLower(d)
	var ops []Op

	switch {
	case strings.Contains(low, "replace") || strings.Contains(low, "rename"):
		x, y := extractPair(d)
		if x == "" || y == "" {
			return nil, fmt.Errorf("could not parse 'replace X with Y' from %q", description)
		}
		ops = append(ops, Op{Kind: KindSed, X: x, Y: y, Global: true})

	case strings.Contains(low, "uppercase") || strings.Contains(low, "upper"):
		ops = append(ops, Op{Kind: KindAwk, Transform: "upper"})

	case strings.Contains(low, "lowercase") || strings.Contains(low, "lower"):
		ops = append(ops, Op{Kind: KindAwk, Transform: "lower"})

	case strings.Contains(low, "insert"):
		before := strings.Contains(low, "before")
		pat := extractKeyword(d, "matching")
		ins := extractQuoted(d)
		if pat == "" || ins == "" {
			return nil, fmt.Errorf("could not parse insert in %q", description)
		}
		ops = append(ops, Op{Kind: KindSed, Anchor: pat, Text: ins, InsertBefore: before})

	case strings.Contains(low, "delete") || strings.Contains(low, "remove"):
		pat := extractKeyword(d, "matching")
		if pat == "" {
			pat = extractKeyword(d, "with")
		}
		if pat == "" {
			return nil, fmt.Errorf("could not find what to delete in %q", description)
		}
		ops = append(ops, Op{Kind: KindSed, Pattern: pat})

	default:
		pat := extractQuoted(d)
		if pat == "" {
			pat = firstWord(d)
		}
		if pat == "" {
			return nil, fmt.Errorf("instruction too vague for the deterministic generator; rephrase or enable the LLM with --llm")
		}
		ops = append(ops, Op{Kind: KindGrep, Pattern: pat})
	}
	return ops, nil
}

// ParseOps decodes a validated list of typed Ops from JSON. It is used by the
// optional LLM path so the model can only ever emit structured ops, never a raw
// script.
func ParseOps(txt string) ([]Op, bool) {
	trimmed := strings.TrimSpace(txt)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")

	var ops []Op
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ops); err != nil || len(ops) == 0 || len(ops) > 100 {
		return nil, false
	}
	for _, o := range ops {
		if err := o.Validate(); err != nil {
			return nil, false
		}
	}
	return ops, true
}

// --- grammar helpers ---

func extractPair(desc string) (string, string) {
	var rest, sep string
	if i := strings.Index(desc, "replace"); i >= 0 {
		rest, sep = desc[i+len("replace"):], " with "
	} else if i := strings.Index(desc, "rename"); i >= 0 {
		rest, sep = desc[i+len("rename"):], " to "
	} else {
		return "", ""
	}
	k := strings.Index(rest, sep)
	if k < 0 {
		return "", ""
	}
	x := unquote(strings.TrimSpace(rest[:k]))
	y := unquote(strings.TrimSpace(rest[k+len(sep):]))
	return x, y
}

func extractKeyword(desc, key string) string {
	if key == "" {
		return extractQuoted(desc)
	}
	i := strings.Index(desc, key)
	if i < 0 {
		return ""
	}
	return unquote(trimSpaceAfter(desc[i+len(key):]))
}

func extractQuoted(s string) string {
	if i := strings.IndexByte(s, '"'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '"'); j >= 0 {
			return s[i+1 : i+1+j]
		}
	}
	if i := strings.IndexByte(s, '`'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
			return s[i+1 : i+1+j]
		}
	}
	return ""
}

func trimSpaceAfter(s string) string {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, `"`)
}

func firstWord(s string) string {
	stop := map[string]bool{
		"find": true, "search": true, "look": true, "list": true,
		"show": true, "print": true, "get": true, "display": true,
		"grep": true, "match": true, "matches": true,
		"the": true, "a": true, "an": true, "of": true, "to": true,
		"and": true, "or": true, "with": true, "for": true, "in": true,
		"on": true, "that": true, "this": true, "value": true, "values": true,
		"setting": true, "settings": true, "item": true, "items": true,
		"line": true, "lines": true, "keyword": true, "pattern": true,
		"text": true, "string": true,
	}
	for _, w := range strings.Fields(s) {
		lw := strings.ToLower(strings.Trim(w, `"'`))
		if stop[lw] || len(lw) <= 2 {
			continue
		}
		return w
	}
	return ""
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
