package lang

import (
	"regexp"
	"strings"
)

// pyErrorLine matches the "line:col: message" line printed by pySyntaxScript.
var pyErrorLine = regexp.MustCompile(`^(\d+):(\d+):\s*(.+)$`)

// pySyntaxScript compiles stdin as Python source and prints a single
// "line:col: message" line when compilation fails, exiting non-zero.
// Using compile() parses the file without executing it, and reports the same
// SyntaxError/IndentationError the interpreter would.
const pySyntaxScript = `
import sys
try:
    compile(sys.stdin.read(), "<aed>", "exec")
except SyntaxError as e:
    lineno = e.lineno or 0
    col = e.offset or 0
    print("%d:%d: %s" % (lineno, col, e.msg))
    sys.exit(1)
`

func init() {
	Register(&CommandChecker{
		Language:   "python",
		Extensions: []string{".py", ".pyw"},
		Shebangs:   []string{"python"},
		Bin:        "python3",
		Env:        "AED_PYTHON",
		Args:       []string{"-c", pySyntaxScript},
		Parse:      parsePythonOutput,
	})
}

// parsePythonOutput turns the python wrapper's single diagnostic line into a
// structured error, falling back to the raw output when no structured line
// matched (e.g. an unusual interpreter failure).
func parsePythonOutput(output string) []Diagnostic {
	var out []Diagnostic
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := pyErrorLine.FindStringSubmatch(line); m != nil {
			out = append(out, Diagnostic{
				Line:    atoiOr0(m[1]),
				Col:     atoiOr0(m[2]),
				Message: "python: " + m[3],
			})
			continue
		}
	}
	if len(out) == 0 {
		out = append(out, Diagnostic{Message: "python: " + strings.TrimSpace(output)})
	}
	return out
}
