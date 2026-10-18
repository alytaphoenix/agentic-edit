package native

import (
	"testing"

	"agentic-edit/internal/ir"
)

func TestApplyIRReplace(t *testing.T) {
	out, err := ApplyIR(ir.Op{Kind: ir.ReplaceLiteral, Old: "v1", New: "v2", ExpectCount: 1}, "v1\n")
	if err != nil || out != "v2\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	out, err = ApplyIR(ir.Op{Kind: ir.ReplaceRegex, Old: `v[0-9]`, New: "x", Global: true}, "v1 v2\n")
	if err != nil || out != "x x\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestApplyIRInsertFirstVsAll(t *testing.T) {
	in := "a\nk\nb\nk\n"
	out, err := ApplyIR(ir.Op{Kind: ir.Insert, Anchor: "^k$", Text: "x"}, in)
	if err != nil || out != "a\nk\nx\nb\nk\n" {
		t.Fatalf("first out=%q err=%v", out, err)
	}
	out, err = ApplyIR(ir.Op{Kind: ir.Insert, Anchor: "^k$", Text: "x", All: true, InsertBefore: true}, in)
	if err != nil || out != "a\nx\nk\nb\nx\nk\n" {
		t.Fatalf("all out=%q err=%v", out, err)
	}
}

func TestApplyIRRange(t *testing.T) {
	out, err := ApplyIR(ir.Op{Kind: ir.ReplaceRange, Start: 2, End: 3, Text: "X"}, "a\nb\nc\nd\n")
	if err != nil || out != "a\nX\nd\n" {
		t.Fatalf("replace out=%q err=%v", out, err)
	}
	out, err = ApplyIR(ir.Op{Kind: ir.DeleteRange, Start: 1, End: 2}, "a\nb\nc\n")
	if err != nil || out != "c\n" {
		t.Fatalf("delete out=%q err=%v", out, err)
	}
	if _, err := ApplyIR(ir.Op{Kind: ir.DeleteRange, Start: 1, End: 9}, "a\n"); err == nil {
		t.Fatal("expected an out-of-range error")
	}
}

func TestApplyIRFileOpsRejected(t *testing.T) {
	if _, err := ApplyIR(ir.Op{Kind: ir.CreateFile, Path: "x"}, ""); err == nil {
		t.Fatal("file-level op should be rejected by the text engine")
	}
}
