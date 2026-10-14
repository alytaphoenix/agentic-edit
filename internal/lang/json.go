package lang

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

func init() {
	Register(&jsonChecker{})
}

type jsonChecker struct{}

func (jsonChecker) Name() string { return "json" }

func (jsonChecker) Applies(path, content string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".json" || ext == ".jsonl"
}

func (jsonChecker) Check(ctx context.Context, path, content string) ([]Diagnostic, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := strings.TrimSpace(content)
	if s == "" {
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".jsonl" {
		diags := checkJSONLines(content)
		return diags, nil
	}

	// A single JSON document: decode then demand EOF.
	dec := json.NewDecoder(bytes.NewReader([]byte(content)))
	var v any
	if err := dec.Decode(&v); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		line, col := jsonErrLoc(content, err)
		return []Diagnostic{{Line: line, Col: col, Message: fmt.Sprintf("json: %v", err)}}, nil
	}
	// trailing content check
	var extra any
	if err := dec.Decode(&extra); err == nil {
		line, col := jsonErrLoc(content, &json.SyntaxError{Offset: dec.InputOffset()})
		_ = line
		_ = col
		return []Diagnostic{{Line: 1, Col: 1, Message: "json: invalid top-level value: multiple JSON documents"}}, nil
	} else if err != io.EOF {
		line, col := jsonErrLoc(content, err)
		return []Diagnostic{{Line: line, Col: col, Message: fmt.Sprintf("json: %v", err)}}, nil
	}
	return nil, nil
}

func checkJSONLines(content string) []Diagnostic {
	lines := strings.Split(content, "\n")
	var diags []Diagnostic
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if err := validateJSONValue(line); err != nil {
			line, col := jsonErrLoc(line, err)
			diags = append(diags, Diagnostic{
				Line:    i + 1,
				Col:     col,
				Message: fmt.Sprintf("json: %v", err),
			})
			_ = line
		}
	}
	return diags
}

func validateJSONValue(src string) error {
	var v any
	return json.Unmarshal([]byte(src), &v)
}

func jsonErrLoc(content string, err error) (int, int) {
	off := 0
	if se, ok := err.(*json.SyntaxError); ok {
		off = int(se.Offset)
	}
	if off <= 0 {
		return 1, 1
	}
	return offsetLineCol(content, off, true)
}

func offsetLineCol(content string, off int, oneBased bool) (int, int) {
	idx := off
	if oneBased {
		idx--
	}
	if idx < 0 {
		idx = 0
	}
	if idx > len(content) {
		idx = len(content)
	}
	line, col := 1, 1
	for i := 0; i < idx; {
		r := content[i]
		if r == '\n' {
			line++
			col = 1
		} else {
			col++
		}
		i++
	}
	return line, col
}
