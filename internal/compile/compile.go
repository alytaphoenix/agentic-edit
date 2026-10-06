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
	switch op.Kind {
	case intent.KindSed:
		return renderSed(op, force)
	case intent.KindAwk:
		return renderAwk(op)
	case intent.KindGrep:
		return exec.EditOp{
			Kind:    exec.KindGrep,
			Pattern: op.Pattern,
			Purpose: "find lines matching " + op.Pattern,
		}, nil
	default:
		return exec.EditOp{}, fmt.Errorf("unknown op kind %q", op.Kind)
	}
}

func renderSed(op intent.Op, force bool) (exec.EditOp, error) {
	switch op.SedAction() {
	case "replace":
		script, err := replaceScript(op.Scope, op.X, op.Y, op.Global)
		if err != nil {
			return exec.EditOp{}, err
		}
		return exec.EditOp{
			Kind:    exec.KindSed,
			Script:  script,
			Assert:  op.Y,
			Deny:    op.X,
			Purpose: fmt.Sprintf("replace %q with %q", op.X, op.Y),
		}, nil
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
			Kind:    exec.KindSed,
			Script:  script,
			Purpose: fmt.Sprintf("insert %q %s anchor %q", op.Text, pos, op.Anchor),
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
			Kind:    exec.KindSed,
			Script:  script,
			Purpose: "delete lines matching " + op.Pattern,
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
		Kind:    exec.KindAwk,
		Script:  prog,
		Purpose: "awk " + op.Transform,
	}, nil
}

// isAddr reports whether s is a well-formed /regex/ sed address.
func isAddr(s string) bool {
	if len(s) < 2 || s[0] != '/' || s[len(s)-1] != '/' {
		return false
	}
	return !strings.ContainsAny(s, "\r\n")
}

// --- sed template builders ---

func replaceScript(scope, x, y string, global bool) (string, error) {
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
	return fmt.Sprintf("%ss|%s|%s|%s", addr, sedReplaceEscape(x), sedReplaceEscape(y), flag), nil
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
	return addr + cmd + "\n" + text, nil
}

// sedReplaceEscape escapes a value for use inside the s|...|...| command, using
// '|' as the delimiter. Backslash is escaped first so later escapes survive.
func sedReplaceEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, `&`, `\&`)
	return s
}

// sedAddressEscape escapes a value for use inside a /.../ address.
func sedAddressEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `/`, `\/`)
	return s
}
