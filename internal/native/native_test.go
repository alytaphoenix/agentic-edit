package native

import (
	"strings"
	"testing"

	"agentic-edit/internal/exec"
)

func TestReplaceLiteralPreservesShape(t *testing.T) {
	in := "\ufeffa v1\r\nb v1\r\n"
	out, err := Apply(exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: "v1", New: "v3", Literal: true, Global: true}, in)
	if err != nil {
		t.Fatal(err)
	}
	want := "\ufeffa v3\r\nb v3\r\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestReplaceLiteralFirstPerLine(t *testing.T) {
	in := "x x\nx\n"
	out, err := Apply(exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: "x", New: "y", Literal: true}, in)
	if err != nil {
		t.Fatal(err)
	}
	if out != "y x\ny\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestReplaceNoFinalNewline(t *testing.T) {
	in := "v1"
	out, err := Apply(exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: "v1", New: "v2", Literal: true}, in)
	if err != nil {
		t.Fatal(err)
	}
	if out != "v2" {
		t.Fatalf("out = %q", out)
	}
}

func TestReplaceExpectCount(t *testing.T) {
	op := exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: "v1", New: "v2", Literal: true, ExpectCount: 1}
	if _, err := Apply(op, "v1\nv1\n"); err == nil {
		t.Fatal("expected a count mismatch")
	}
	if out, err := Apply(op, "v1\n"); err != nil || out != "v2\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestReplaceRegexFirstAndGlobal(t *testing.T) {
	op := exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: `v[0-9]`, New: "X", Extended: true}
	out, err := Apply(op, "v1 v2\nv3\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "X v2\nX\n" {
		t.Fatalf("first-per-line out = %q", out)
	}

	op.Global = true
	out, err = Apply(op, "v1 v2\nv3\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "X X\nX\n" {
		t.Fatalf("global out = %q", out)
	}
}

// New is a value, not a sed replacement expression: "$1" must stay literal.
func TestReplaceRegexReplacementIsLiteral(t *testing.T) {
	op := exec.EditOp{Kind: exec.KindSed, Action: "replace", Old: `v1`, New: "$1&", Extended: true, Global: true}
	out, err := Apply(op, "v1\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "$1&\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestInsertBeforeAndAfter(t *testing.T) {
	in := "a\nanchor\nb\nanchor\nc\n"
	op := exec.EditOp{Kind: exec.KindSed, Action: "insert", Anchor: "^anchor$", Text: "new"}
	out, err := Apply(op, in)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nanchor\nnew\nb\nanchor\nnew\nc\n" {
		t.Fatalf("after out = %q", out)
	}

	op.InsertBefore = true
	out, err = Apply(op, in)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nnew\nanchor\nb\nnew\nanchor\nc\n" {
		t.Fatalf("before out = %q", out)
	}
}

func TestDeleteAndWholeFileGate(t *testing.T) {
	op := exec.EditOp{Kind: exec.KindSed, Action: "delete", Pattern: "^a"}
	out, err := Apply(op, "a1\nb\na2\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "b\n" {
		t.Fatalf("out = %q", out)
	}

	if _, err := Apply(exec.EditOp{Kind: exec.KindSed, Action: "delete", Pattern: ".*"}, "a\nb\n"); err == nil {
		t.Fatal("expected the whole-file delete gate")
	}
	out, err = Apply(exec.EditOp{Kind: exec.KindSed, Action: "delete", Pattern: ".*", DeleteAllAllowed: true}, "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("out = %q", out)
	}
}

func TestTransformPreservesEOL(t *testing.T) {
	out, err := Apply(exec.EditOp{Kind: exec.KindAwk, Action: "transform", Transform: "upper"}, "ab\r\ncd")
	if err != nil {
		t.Fatal(err)
	}
	if out != "AB\r\nCD" {
		t.Fatalf("out = %q", out)
	}
}

func TestInsertTextWithNewlines(t *testing.T) {
	out, err := Apply(exec.EditOp{Kind: exec.KindSed, Action: "insert", Anchor: "b", Text: "x\ny"}, "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "a\nb\nx\ny\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestDocumentRoundTrip(t *testing.T) {
	cases := []string{"", "\n", "a", "a\n", "a\n\n", "\ufeffa\r\nb\r\n"}
	for _, in := range cases {
		if got := Parse(in).Render(); got != in {
			t.Errorf("round trip %q -> %q", in, got)
		}
	}
	if strings.Join(Parse("a\r\nb").Lines, "|") != "a|b" {
		t.Error("CRLF was not normalized into lines")
	}
}
