package lang

import (
	"regexp"
	"strings"
)

func init() {
	Register(&CommandChecker{
		Language:   "yaml",
		Extensions: []string{".yaml", ".yml"},
		Bin:        "python3",
		Env:        "AED_PYTHON",
		Args:       []string{"-c", yamlScript},
		Timeout:    DefaultTimeout,
		Parse:      parseYamlOutput,
		Unavailable: func(out string) bool {
			l := strings.ToLower(out)
			return strings.Contains(l, "no module named") || strings.Contains(l, "modulenotfounderror") || strings.Contains(l, "aed-unavailable:")
		},
	})
}

const yamlScript = `
import sys
src = sys.stdin.read()
try:
    import yaml
except Exception as e:
    print("aed-unavailable: yaml " + str(e))
    sys.exit(2)
try:
    yaml.safe_load(src)
except Exception as e:
    print(e)
    sys.exit(1)
`

var yamlLineCol = regexp.MustCompile(`(?i)(?:line\s+)?(\d+):\s*(.+)`)

func parseYamlOutput(output string) []Diagnostic {
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
		d := Diagnostic{Message: "yaml: " + line}
		// PyYAML errors usually look like: "found duplicate key ... (line: 2, col: 3)"
		if m := regexp.MustCompile(`line\s+(\d+)`).FindStringSubmatch(line); m != nil {
			d.Line = atoiOr0(m[1])
		}
		if m := regexp.MustCompile(`col(?:umn)?\s+(\d+)`).FindStringSubmatch(line); m != nil {
			d.Col = atoiOr0(m[1])
		}
		diags = append(diags, d)
	}
	return diags
}
