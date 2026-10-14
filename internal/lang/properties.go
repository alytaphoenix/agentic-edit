package lang

import (
	"context"
	"path/filepath"
	"strings"
)

func init() {
	Register(&propertiesChecker{})
}

type propertiesChecker struct{}

func (propertiesChecker) Name() string { return "properties" }
func (propertiesChecker) Applies(path, content string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".properties"
}

func (propertiesChecker) Check(ctx context.Context, path, content string) ([]Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lines := strings.Split(content, "\n")
	var diags []Diagnostic
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		// Continuation lines end in backslash; the preceding line joined.
		if strings.HasSuffix(line, "\\") {
			continue
		}
		if !(strings.ContainsAny(line, "=:")) {
			diags = append(diags, Diagnostic{
				Line:    i + 1,
				Col:     1,
				Message: "properties: line has no key/value separator ('=', or ':')",
			})
		}
	}
	return diags, nil
}
