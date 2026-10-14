package lang

import (
	"regexp"
	"strings"
)

func init() {
	Register(&CommandChecker{
		Language:   "toml",
		Extensions: []string{".toml"},
		Bin:        "python3",
		Env:        "AED_PYTHON",
		Args:       []string{"-c", tomlScript},
		Timeout:    DefaultTimeout,
		Parse:      parseTomlOutput,
		Unavailable: func(out string) bool {
			l := strings.ToLower(out)
			return strings.Contains(l, "no module named") || strings.Contains(l, "modulenotfounderror") || strings.Contains(l, "aed-unavailable:")
		},
	})
}

const tomlScript = `
import sys
src = sys.stdin.read()
try:
    import tomllib
except Exception as e:
    print("aed-unavailable: " + str(e))
    sys.exit(2)
try:
    tomllib.loads(src)
except Exception as e:
    print(e)
    sys.exit(1)
`

var tomlLineCol = regexp.MustCompile(`(?i)line\s+(\d+),?\s*column\s+(\d+)`)

func parseTomlOutput(output string) []Diagnostic {
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
		d := Diagnostic{Message: "toml: " + line}
		if m := tomlLineCol.FindStringSubmatch(line); m != nil {
			d.Line = atoiOr0(m[1])
			d.Col = atoiOr0(m[2])
		}
		diags = append(diags, d)
	}
	return diags
}
