package lang

import (
	"regexp"
	"strings"
)

func init() {
	Register(&CommandChecker{
		Language:   "ini",
		Extensions: []string{".ini", ".cfg"},
		Bin:        "python3",
		Env:        "AED_PYTHON",
		Args:       []string{"-c", iniScript},
		Timeout:    DefaultTimeout,
		Parse:      parseIniOutput,
		Unavailable: func(out string) bool {
			l := strings.ToLower(out)
			return strings.Contains(l, "aed-unavailable:") || strings.Contains(l, "no module named")
		},
	})
}

const iniScript = `
import sys, configparser, re
src = sys.stdin.read()
try:
    import configparser as _c
except Exception as e:
    print("aed-unavailable: configparser " + str(e))
    sys.exit(2)
cp = configparser.ConfigParser()
# Permit option-only files by injecting a synthetic section when no header present.
if not re.search(r"(?m)^\s*\[", src):
    src = "[DEFAULT]\n" + src
try:
    cp.read_string(src)
except Exception as e:
    print(e)
    sys.exit(1)
`

var iniLineCol = regexp.MustCompile(`(?i)line\s+(\d+)`)

func parseIniOutput(output string) []Diagnostic {
	out := strings.TrimSpace(output)
	if out == "" {
		return nil
	}
	var diags []Diagnostic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		d := Diagnostic{Message: "ini: " + line}
		if m := iniLineCol.FindStringSubmatch(line); m != nil {
			d.Line = atoiOr0(m[1])
		}
		diags = append(diags, d)
	}
	return diags
}
