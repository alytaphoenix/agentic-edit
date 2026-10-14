package lang

import (
	"regexp"
	"strconv"
	"strings"
)

// bashErrorLine matches bash's "<file>: line N: <message>" diagnostics.
var bashErrorLine = regexp.MustCompile(`line\s+(\d+):\s*(.+)$`)

func init() {
	Register(&CommandChecker{
		Language:   "bash",
		Extensions: []string{".sh", ".bash"},
		Shebangs:   []string{"bash", "sh", "dash"},
		Bin:        "bash",
		Env:        "AED_BASH",
		Args:       []string{"-n", StdinPlaceholder},
		Parse:      parseBashOutput,
	})
}

// parseBashOutput turns `bash -n` diagnostics into structured errors. bash
// prints one line per problem: "<path>: line N: <msg>".
func parseBashOutput(output string) []Diagnostic {
	var out []Diagnostic
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := bashErrorLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			if msg := m[2]; msg != "" {
				out = append(out, Diagnostic{Line: n, Message: "bash: " + msg})
			}
			continue
		}
	}
	if len(out) == 0 {
		out = append(out, Diagnostic{Message: "bash: " + output})
	}
	return out
}
