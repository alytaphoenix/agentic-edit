package lang

import (
	"context"
	"encoding/xml"
	"io"
	"path/filepath"
	"strings"
)

func init() {
	Register(&xmlChecker{})
}

type xmlChecker struct{}

func (xmlChecker) Name() string { return "xml" }
func (xmlChecker) Applies(path, content string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".xml"
}

func (xmlChecker) Check(ctx context.Context, path, content string) ([]Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}
	dec := xml.NewDecoder(strings.NewReader(content))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			if se, ok := err.(*xml.SyntaxError); ok {
				return []Diagnostic{{Line: se.Line, Message: "xml: " + se.Msg}}, nil
			}
			return []Diagnostic{{Message: "xml: " + err.Error()}}, nil
		}
	}
}
