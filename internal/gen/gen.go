// Package gen turns a parsed intent into concrete awk/sed/grep operations. It
// tries an LLM (Laya -> Ollama -> OpenAI-compatible) first, then falls back to a
// rule-based generator so the tool is fully usable with no external services.
package gen

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/laya"
	"agentic-edit/internal/llm"
	"agentic-edit/internal/plan"
)

// Input carries everything the generator needs.
type Input struct {
	Intent      plan.Intent
	Contents    map[string]string
	LayaContext string
	UseLLM      bool
	Model       string
	OllamaURL   string
	APIKey      string
	BaseURL     string
	Client      *laya.Client
}

// Result is the set of operations produced for an intent.
type Result struct {
	Ops    []exec.EditOp
	Reason string
}

type editCmd struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Script  string `json:"script"`
	Assert  string `json:"assert"`
	Deny    string `json:"deny"`
	Purpose string `json:"purpose"`
}

// Generate produces operations for the intent.
func Generate(in Input) (*Result, error) {
	if len(in.Contents) == 0 {
		return nil, fmt.Errorf("no file contents available")
	}
	if in.UseLLM {
		if txt, err := callLLM(in); err == nil && strings.TrimSpace(txt) != "" {
			if res, ok := parseResult(txt, in); ok {
				return res, nil
			}
		}
	}
	return ruleBased(in)
}

func callLLM(in Input) (string, error) {
	ctx := context.Background()
	// 1) Laya pipeline (uses the same local model Laya is configured with).
	if in.Client.Enabled() && in.Client.Ping(ctx) {
		if s, err := in.Client.Chat(ctx, genPrompt(in)); err == nil && s != "" {
			return s, nil
		}
	}
	// 2) OpenAI-compatible endpoint (API key).
	if in.APIKey != "" {
		if s, err := llm.OpenAICompatible(ctx, in.BaseURL, in.APIKey, in.Model, genPrompt(in)); err == nil && s != "" {
			return s, nil
		}
	}
	// 3) Local Ollama.
	if s, err := llm.Ollama(ctx, in.OllamaURL, in.Model, genPrompt(in)); err == nil && s != "" {
		return s, nil
	}
	return "", fmt.Errorf("all LLM backends failed")
}

func genPrompt(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Instruction: %s\n", in.Intent.Description)
	if in.LayaContext != "" {
		fmt.Fprintf(&b, "\nBackground context from Laya:\n%s\n", in.LayaContext)
	}
	b.WriteString("\nTarget file contents (first 4000 chars each):\n")
	for path, content := range in.Contents {
		if len(content) > 4000 {
			content = content[:4000] + "\n...[truncated]..."
		}
		fmt.Fprintf(&b, "\n### %s\n%s\n", path, content)
	}
	b.WriteString(`Respond with ONLY a JSON array (no markdown fences) of edit objects:
[{"kind":"sed","path":"<file>","script":"<sed script>","assert":"<grep pattern required after>","deny":"<grep pattern forbidden after>","purpose":"<why>"}]
Rules: use | as the sed delimiter when the data contains /; escape | inside patterns; keep commands targeted; never emit rm, whole-file deletes, or system() calls. If no edit is needed, return [].`)
	return b.String()
}

func parseResult(txt string, in Input) (*Result, bool) {
	trimmed := strings.TrimSpace(txt)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")

	var cmds []editCmd
	if err := json.Unmarshal([]byte(trimmed), &cmds); err != nil || len(cmds) == 0 {
		return nil, false
	}
	var ops []exec.EditOp
	for _, c := range cmds {
		kind := c.Kind
		if kind == "" {
			kind = exec.KindSed
		}
		purpose := c.Purpose
		if purpose == "" {
			if c.Path != "" {
				purpose = fmt.Sprintf("%s on %s", kind, c.Path)
			} else {
				purpose = kind
			}
		}
		ops = append(ops, exec.EditOp{
			Kind:    kind,
			Path:    c.Path,
			Script:  c.Script,
			Pattern: c.Script,
			Assert:  c.Assert,
			Deny:    c.Deny,
			Purpose: purpose,
		})
	}
	return &Result{Ops: ops, Reason: "llm"}, true
}

// ruleBased implements keyword heuristics for common edits.
func ruleBased(in Input) (*Result, error) {
	desc := strings.TrimSpace(in.Intent.Description)
	ldesc := strings.ToLower(desc)
	var ops []exec.EditOp

	switch {
	case strings.Contains(ldesc, "replace") || strings.Contains(ldesc, "rename"):
		x, y := extractPair(desc)
		if x == "" || y == "" {
			return nil, fmt.Errorf("could not parse 'replace X with Y' from %q", in.Intent.Description)
		}
		ops = append(ops, exec.EditOp{
			Kind:    exec.KindSed,
			Script:  sedReplace(x, y),
			Assert:  escGrep(y),
			Deny:    escGrep(x),
			Purpose: fmt.Sprintf("replace %q with %q", x, y),
		})
	case strings.Contains(ldesc, "uppercase") || strings.Contains(ldesc, "upper"):
		ops = append(ops, exec.EditOp{
			Kind:    exec.KindAwk,
			Script:  `{ print toupper($0) }`,
			Purpose: "uppercase every line",
		})
	case strings.Contains(ldesc, "lowercase") || strings.Contains(ldesc, "lower"):
		ops = append(ops, exec.EditOp{
			Kind:    exec.KindAwk,
			Script:  `{ print tolower($0) }`,
			Purpose: "lowercase every line",
		})
	case strings.Contains(ldesc, "delete") || strings.Contains(ldesc, "remove"):
		pat := extractKeyword(desc, "matching")
		if pat == "" {
			pat = extractKeyword(desc, "with")
		}
		if pat == "" {
			return nil, fmt.Errorf("could not find what to delete in %q", in.Intent.Description)
		}
		ops = append(ops, exec.EditOp{
			Kind:    exec.KindSed,
			Script:  "/" + escapeSed(pat) + "/d",
			Purpose: "delete lines matching " + pat,
		})
	case strings.Contains(ldesc, "insert") && strings.Contains(ldesc, "after"):
		pat := extractKeyword(desc, "matching")
		ins := extractQuoted(desc)
		if pat == "" || ins == "" {
			return nil, fmt.Errorf("could not parse insert-after in %q", in.Intent.Description)
		}
		ops = append(ops, exec.EditOp{
			Kind: exec.KindSed,
			// Emit the POSIX `a\` multiline form so `sed -f` works on both
			// GNU (Linux) and BSD (macOS) sed.
			Script:  "/" + escapeSed(pat) + "/a\\\n" + ins,
			Purpose: fmt.Sprintf("insert %q after line matching %q", ins, pat),
		})
	default:
		pat := extractQuoted(desc)
		if pat == "" {
			pat = firstWord(desc)
		}
		if pat == "" {
			return nil, fmt.Errorf("instruction too vague for the rule-based fallback; rephrase or enable the LLM (drop --no-llm)")
		}
		ops = append(ops, exec.EditOp{
			Kind:    exec.KindGrep,
			Pattern: pat,
			Purpose: "find lines matching " + pat,
		})
	}
	return &Result{Ops: ops, Reason: "rule"}, nil
}

// --- heuristic helpers ---

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// extractPair pulls "replace X with Y" or "rename X to Y" (quotes optional).
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

// firstWord returns the first substantive word, skipping common discovery verbs.
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

func sedReplace(x, y string) string {
	// Prefer | delimiter; escape | in both sides.
	xe := strings.ReplaceAll(x, "|", `\|`)
	ye := strings.ReplaceAll(y, "|", `\|`)
	return "s|" + xe + "|" + ye + "|g"
}

func escapeSed(s string) string {
	// Escape sed-special chars that would break a /.../ address.
	for _, r := range []string{"/", "`", "&"} {
		s = strings.ReplaceAll(s, r, `\`+r)
	}
	return s
}

func escGrep(s string) string { return s }
