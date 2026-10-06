package compile

import (
	"strings"
	"testing"

	"agentic-edit/internal/intent"
)

func TestRenderReplace(t *testing.T) {
	op := intent.Op{Kind: intent.KindSed, X: "v1", Y: "v3", Global: true}
	eo, err := Render(op, false)
	if err != nil {
		t.Fatal(err)
	}
	if eo.Script != "s|v1|v3|g" || eo.Assert != "v3" || eo.Deny != "v1" {
		t.Errorf("unexpected: %+v", eo)
	}
}

func TestRenderReplaceEscapes(t *testing.T) {
	op := intent.Op{Kind: intent.KindSed, X: `a\b|c`, Y: "&x", Global: true}
	eo, err := Render(op, false)
	if err != nil {
		t.Fatal(err)
	}
	// backslash, pipe, and & are all escaped for the s|...|...| form.
	if eo.Script != `s|a\\b\|c|\&x|g` {
		t.Errorf("unexpected script: %q", eo.Script)
	}
}

func TestRenderDelete(t *testing.T) {
	op := intent.Op{Kind: intent.KindSed, Pattern: "TODO"}
	eo, err := Render(op, false)
	if err != nil {
		t.Fatal(err)
	}
	if eo.Script != "/TODO/d" {
		t.Errorf("unexpected script: %q", eo.Script)
	}
}

func TestRenderDeleteAllRefusedWithoutForce(t *testing.T) {
	for _, pat := range []string{".", ".*", "^", "$"} {
		op := intent.Op{Kind: intent.KindSed, Pattern: pat}
		if _, err := Render(op, false); err == nil {
			t.Errorf("pattern %q should be refused without force", pat)
		}
		// With force it succeeds.
		eo, err := Render(op, true)
		if err != nil {
			t.Errorf("pattern %q unexpected error with force: %v", pat, err)
		}
		if !strings.HasSuffix(eo.Script, "/d") {
			t.Errorf("pattern %q unexpected script: %q", pat, eo.Script)
		}
	}
}

func TestRenderInsert(t *testing.T) {
	op := intent.Op{Kind: intent.KindSed, Anchor: "config", Text: "NEW", InsertBefore: true}
	eo, err := Render(op, false)
	if err != nil {
		t.Fatal(err)
	}
	if eo.Script != "/config/i\\\nNEW" {
		t.Errorf("unexpected script: %q", eo.Script)
	}
}

func TestRenderAwk(t *testing.T) {
	eo, err := Render(intent.Op{Kind: intent.KindAwk, Transform: "upper"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if eo.Script != "{ print toupper($0) }" {
		t.Errorf("unexpected script: %q", eo.Script)
	}
}

func TestRenderGrep(t *testing.T) {
	eo, err := Render(intent.Op{Kind: intent.KindGrep, Pattern: "TODO"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if eo.Kind != "grep" || eo.Pattern != "TODO" {
		t.Errorf("unexpected: %+v", eo)
	}
}

func TestRenderUnknownTransform(t *testing.T) {
	if _, err := Render(intent.Op{Kind: intent.KindAwk, Transform: "boom"}, false); err == nil {
		t.Fatal("expected error for unknown transform")
	}
}

func TestRenderInvalidAddress(t *testing.T) {
	op := intent.Op{Kind: intent.KindSed, X: "a", Y: "b", Scope: "notaddr"}
	if _, err := Render(op, false); err == nil {
		t.Fatal("expected error for invalid address")
	}
}
