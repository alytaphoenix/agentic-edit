package native

import (
	"fmt"
	"strings"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/ir"
)

// ApplyIR applies a text-level IR operation to content. File-level operations
// (create/delete/rename/chmod) are handled by the plan builder and return an
// error here.
func ApplyIR(op ir.Op, content string) (string, error) {
	switch op.Kind {
	case ir.ReplaceLiteral:
		return Apply(exec.EditOp{
			Kind: exec.KindSed, Action: "replace", Old: op.Old, New: op.New,
			Literal: true, Global: op.Global, ExpectCount: op.ExpectCount,
		}, content)
	case ir.ReplaceRegex:
		return Apply(exec.EditOp{
			Kind: exec.KindSed, Action: "replace", Old: op.Old, New: op.New,
			Extended: true, Global: op.Global, ExpectCount: op.ExpectCount,
		}, content)
	case ir.Insert:
		return Apply(exec.EditOp{
			Kind: exec.KindSed, Action: "insert", Anchor: op.Anchor, Text: op.Text,
			InsertBefore: op.InsertBefore, FirstOnly: !op.All,
		}, content)
	case ir.ReplaceRange:
		return applyRange(content, op.Start, op.End, op.Text, false)
	case ir.DeleteRange:
		return applyRange(content, op.Start, op.End, "", true)
	case ir.ApplyPatch:
		return ApplyPatch(content, op.Patch)
	default:
		return "", fmt.Errorf("native engine: %q is a file-level operation", op.Kind)
	}
}

func applyRange(content string, start, end int, text string, del bool) (string, error) {
	doc := Parse(content)
	if start < 1 || end < start {
		return "", fmt.Errorf("range requires 1 <= start <= end")
	}
	if end > len(doc.Lines) {
		return "", fmt.Errorf("range %d-%d is beyond the file (%d line(s))", start, end, len(doc.Lines))
	}
	repl := []string(nil)
	if !del && text != "" {
		text = strings.ReplaceAll(text, "\r\n", "\n")
		text = strings.TrimSuffix(text, "\n")
		repl = strings.Split(text, "\n")
	}
	out := make([]string, 0, len(doc.Lines)-(end-start+1)+len(repl))
	out = append(out, doc.Lines[:start-1]...)
	out = append(out, repl...)
	out = append(out, doc.Lines[end:]...)
	doc.Lines = out
	if len(out) == 0 {
		doc.FinalNewline = false
	}
	return doc.Render(), nil
}
