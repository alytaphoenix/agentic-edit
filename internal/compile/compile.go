// Package compile renders validated, typed intent.Ops into executable
// exec.EditOp values. Every command is produced from a fixed template with
// inputs escaped, so an operation can only ever be one of a known-safe shape.
// This is the primary safety guarantee; the safety package remains as a
// defense-in-depth denylist on the rendered output.
package compile

import (
	"fmt"
	"regexp"
	"strings"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/intent"
)

// awkPrograms maps a transform name to its fixed program text.
var awkPrograms = map[string]string{
	"upper": `{ print toupper($0) }`,
	"lower": `{ print tolower($0) }`,
}

// matchAllRe matches sed patterns that would delete every line.
var matchAllRe = regexp.MustCompile(`^(?:\.\*|\.|\^|\$)$`)

// Render validates and renders a typed Op into an executable EditOp. force
// permits otherwise-rejected whole-file destructive deletes.
func Render(op intent.Op, force bool) (exec.EditOp, error) {
	if err := op.Validate(); err != nil {
		return exec.EditOp{}, err
	}
	var (
		eo  exec.EditOp
		err error
	)
	switch op.Kind {
	case intent.KindSed:
		eo, err = renderSed(op, force)
	case intent.KindAwk:
		eo, err = renderAwk(op)
	case intent.KindGrep:
		eo = exec.EditOp{
			Kind:    exec.KindGrep,
			Action:  "grep",
			Pattern: op.Pattern,
			Purpose: "find lines matching " + op.Pattern,
		}
	default:
		return exec.EditOp{}, fmt.Errorf("unknown op kind %q", op.Kind)
	}
	if err != nil {
		return exec.EditOp{}, err
	}
	eo.FromTemplate = true
	return eo, nil
}

func renderSed(op intent.Op, force bool) (exec.EditOp, error) {
	switch op.SedAction() {
	case "replace":
		literal := !op.Regex
		global := op.Global || op.ExpectCount > 1
		script, err := replaceScript(op.Scope, op.X, op.Y, global, literal)
		if err != nil {
			return exec.EditOp{}, err
		}
		eo := exec.EditOp{
			Kind:        exec.KindSed,
			Action:      "replace",
			Script:      script,
			Assert:      op.Y,
			Old:         op.X,
			New:         op.Y,
			Literal:     literal,
			Global:      global,
			Extended:    !literal, // regex replaces are POSIX ERE via sed -E
			ExpectCount: op.ExpectCount,
			Purpose:     fmt.Sprintf("replace %q with %q", op.X, op.Y),
		}
		if op.Global || op.ExpectCount > 1 {
			// Only assert the old value is absent for a global replace; a
			// partial replace can legitimately leave the old value elsewhere.
			eo.Deny = op.X
			eo.DenyRegex = !literal
		}
		return eo, nil
	case "insert":
		script, err := insertScript(op.Anchor, op.Text, op.InsertBefore)
		if err != nil {
			return exec.EditOp{}, err
		}
		pos := "after"
		if op.InsertBefore {
			pos = "before"
		}
		return exec.EditOp{
			Kind:         exec.KindSed,
			Action:       "insert",
			Anchor:       op.Anchor,
			Text:         op.Text,
			InsertBefore: op.InsertBefore,
			Script:       script,
			Assert:       op.Text,
			Purpose:      fmt.Sprintf("insert %q %s anchor %q", op.Text, pos, op.Anchor),
		}, nil
	case "delete":
		if matchAllRe.MatchString(op.Pattern) && !force {
			return exec.EditOp{}, fmt.Errorf("delete pattern %q matches every line; re-run with --force to allow whole-file deletion", op.Pattern)
		}
		script, err := deleteScript(op.Pattern)
		if err != nil {
			return exec.EditOp{}, err
		}
		return exec.EditOp{
			Kind:             exec.KindSed,
			Action:           "delete",
			Script:           script,
			Pattern:          op.Pattern, // marks this as a delete op
			Deny:             op.Pattern, // the deleted pattern must be absent
			DenyRegex:        true,
			DeleteAllAllowed: force,
			Purpose:          "delete lines matching " + op.Pattern,
		}, nil
	}
	return exec.EditOp{}, fmt.Errorf("cannot render sed op")
}

func renderAwk(op intent.Op) (exec.EditOp, error) {
	prog, ok := awkPrograms[op.Transform]
	if !ok {
		return exec.EditOp{}, fmt.Errorf("unknown awk transform %q", op.Transform)
	}
	return exec.EditOp{
		Kind:      exec.KindAwk,
		Action:    "transform",
		Transform: op.Transform,
		Script:    prog,
		Purpose:   "awk " + op.Transform,
	}, nil
}

// isAddr reports whether s is a well-formed, restricted /regex/ sed address.
func isAddr(s string) bool {
	if len(s) < 2 || len(s) > 1024 || s[0] != '/' || s[len(s)-1] != '/' {
		return false
	}
	// Semicolon, braces, bang, and hash are sed command separators/control
	// characters. Rejecting them prevents address-scope command injection such
	// as "/x/;e id;/y/".
	if strings.ContainsAny(s, "\r\n;{}!#\\") {
		return false
	}
	return true
}

// --- sed template builders ---

func replaceScript(scope, x, y string, global, literal bool) (string, error) {
	if strings.ContainsAny(x, "\r\n") || strings.ContainsAny(y, "\r\n") {
		return "", fmt.Errorf("replace values must be single-line")
	}
	addr := scope
	if addr != "" && !isAddr(addr) {
		return "", fmt.Errorf("invalid sed address %q", addr)
	}
	flag := "g"
	if !global {
		flag = ""
	}
	delim := pickDelimiter(x, y)
	pattern := x
	if literal {
		pattern = sedLiteralEscape(x, delim)
	} else {
		// A regex is passed through with only the delimiter escaped. Escaping
		// backslashes here (as the replacement escaper does) would turn a regex
		// dot into a literal backslash plus any character.
		pattern = sedRegexEscape(x, delim)
	}
	replacement := sedReplacementEscape(y, delim)
	return fmt.Sprintf("%ss%c%s%c%s%c%s", addr, delim, pattern, delim, replacement, delim, flag), nil
}

func deleteScript(pattern string) (string, error) {
	if strings.ContainsAny(pattern, "\r\n") {
		return "", fmt.Errorf("delete pattern must be single-line")
	}
	return "/" + sedAddressEscape(pattern) + "/d", nil
}

func insertScript(anchor, text string, before bool) (string, error) {
	if strings.ContainsAny(anchor, "\r\n") {
		return "", fmt.Errorf("anchor must be single-line")
	}
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("inserted text must be single-line")
	}
	addr := "/" + sedAddressEscape(anchor) + "/"
	cmd := "a\\"
	if before {
		cmd = "i\\"
	}
	// POSIX multiline form so `sed -f` works on both GNU (Linux) and BSD (macOS).
	// Backslashes are escaped so a trailing one cannot continue the sed command
	// onto the following line.
	return addr + cmd + "\n" + strings.ReplaceAll(text, `\`, `\\`), nil
}

// pickDelimiter chooses a sed s-command delimiter that occurs in neither the
// pattern nor the replacement. This keeps a regex alternation such as "a|b"
// meaningful instead of escaping it into a literal pipe.
func pickDelimiter(x, y string) byte {
	for _, d := range []byte("|#%@~") {
		if !strings.ContainsRune(x, rune(d)) && !strings.ContainsRune(y, rune(d)) {
			return d
		}
	}
	return '|'
}

// sedLiteralEscape escapes a literal value so it matches only itself inside the
// s-command pattern. Backslash is escaped first so later escapes survive.
func sedLiteralEscape(s string, delim byte) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, string(delim), `\`+string(delim))
	for _, ch := range []string{".", "[", "]", "^", "$", "*", "&"} {
		s = strings.ReplaceAll(s, ch, `\`+ch)
	}
	return s
}

// sedRegexEscape escapes only the s-command delimiter for a regex pattern. A
// delimiter preceded by an odd number of backslashes is already escaped and is
// left alone.
func sedRegexEscape(s string, delim byte) string {
	var b strings.Builder
	backslashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' {
			backslashes++
			b.WriteByte(c)
			continue
		}
		if c == delim && backslashes%2 == 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
		backslashes = 0
	}
	return b.String()
}

// sedReplacementEscape escapes a replacement value: backslash, "&", and the
// chosen delimiter.
func sedReplacementEscape(s string, delim byte) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	// The delimiter is now unescaped in the escaped string (the backslash
	// doubling above makes every preceding run even), so escape it directly.
	s = strings.ReplaceAll(s, string(delim), `\`+string(delim))
	s = strings.ReplaceAll(s, `&`, `\&`)
	return s
}

// sedAddressEscape escapes a value for use inside a /.../ address.
func sedAddressEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `/`, `\/`)
	return s
}
