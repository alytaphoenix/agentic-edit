package lang

import (
	"context"
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
	"strings"
)

func init() {
	Register(&goChecker{})
}

type goChecker struct{}

func (goChecker) Name() string { return "go" }

func (goChecker) Applies(path, content string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func (goChecker) Check(ctx context.Context, path, content string) ([]Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, path, content, parser.AllErrors)
	if err == nil {
		return nil, nil
	}

	switch e := err.(type) {
	case scanner.ErrorList:
		out := make([]Diagnostic, 0, len(e))
		for _, se := range e {
			out = append(out, Diagnostic{
				Line:    se.Pos.Line,
				Col:     se.Pos.Column,
				Message: "go: " + se.Msg,
			})
		}
		if len(out) == 0 {
			out = append(out, Diagnostic{Message: "go: " + err.Error()})
		}
		return out, nil
	default:
		return []Diagnostic{{Message: "go: " + err.Error()}}, nil
	}
}
