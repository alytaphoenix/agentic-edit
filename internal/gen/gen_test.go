package gen

import (
	"testing"

	"agentic-edit/internal/plan"
)

func genFor(t *testing.T, desc string, force bool) *Result {
	t.Helper()
	res, err := Generate(Input{
		Intent:   plan.Parse(desc, nil),
		Contents: map[string]string{"a.go": "const A = \"v1\"\nTODO fix\nconfig = 1\n"},
		Force:    force,
	})
	if err != nil {
		t.Fatalf("unexpected error for %q: %v", desc, err)
	}
	return res
}

func TestGenerateReplace(t *testing.T) {
	res := genFor(t, `replace "v1" with "v3"`, false)
	if len(res.Ops) != 1 {
		t.Fatalf("want 1 op, got %d", len(res.Ops))
	}
	op := res.Ops[0]
	if op.Kind != "sed" || op.Script != "s|v1|v3|g" {
		t.Errorf("unexpected op: %+v", op)
	}
	if op.Assert != "v3" || op.Deny != "v1" {
		t.Errorf("unexpected asserts: assert=%q deny=%q", op.Assert, op.Deny)
	}
}

func TestGenerateReplacePipeDelimiter(t *testing.T) {
	res, err := Generate(Input{
		Intent:   plan.Parse(`replace a/b with c/d`, nil),
		Contents: map[string]string{"a.go": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// '|' is the sed delimiter, so '/' needs no escaping.
	if got := res.Ops[0].Script; got != "s|a/b|c/d|g" {
		t.Errorf("pipe escaping wrong: %q", got)
	}
}

func TestGenerateReplaceEscapesDelimiter(t *testing.T) {
	res, err := Generate(Input{
		Intent:   plan.Parse(`replace a|b with c|d`, nil),
		Contents: map[string]string{"a.go": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The delimiter is chosen to avoid "|", so the literals keep their pipes.
	if got := res.Ops[0].Script; got != "s#a|b#c|d#g" {
		t.Errorf("pipe escaping wrong: %q", got)
	}
}

func TestGenerateDeletePreservesCase(t *testing.T) {
	res := genFor(t, `delete lines matching TODO`, false)
	if res.Ops[0].Script != "/TODO/d" {
		t.Errorf("unexpected delete script: %q", res.Ops[0].Script)
	}
}

func TestGenerateInsertAfter(t *testing.T) {
	res := genFor(t, `insert "NEW" after line matching config`, false)
	if res.Ops[0].Script != "/config/a\\\nNEW" {
		t.Errorf("unexpected insert script: %q", res.Ops[0].Script)
	}
}

func TestGenerateInsertBefore(t *testing.T) {
	res := genFor(t, `insert "NEW" before line matching config`, false)
	if res.Ops[0].Script != "/config/i\\\nNEW" {
		t.Errorf("unexpected insert script: %q", res.Ops[0].Script)
	}
}

func TestGenerateVagueFallsToGrep(t *testing.T) {
	res := genFor(t, `find the config value`, false)
	if res.Ops[0].Kind != "grep" || res.Ops[0].Pattern != "config" {
		t.Errorf("expected grep for 'config', got %+v", res.Ops[0])
	}
}

func TestGenerateUppercase(t *testing.T) {
	res := genFor(t, "uppercase every line", false)
	op := res.Ops[0]
	if op.Kind != "awk" || op.Script != "{ print toupper($0) }" {
		t.Errorf("unexpected uppercase op: %+v", op)
	}
}

func TestGenerateLowercase(t *testing.T) {
	res := genFor(t, "lowercase every line", false)
	op := res.Ops[0]
	if op.Kind != "awk" || op.Script != "{ print tolower($0) }" {
		t.Errorf("unexpected lowercase op: %+v", op)
	}
}

// Regression: "delete all lines matching X" must produce a sed delete, not a grep.
func TestGenerateDeleteAllMatching(t *testing.T) {
	res := genFor(t, "delete all lines matching BETA", false)
	if res.Ops[0].Kind != "sed" {
		t.Errorf("expected sed delete, got kind=%q (%+v)", res.Ops[0].Kind, res.Ops[0])
	}
	if res.Ops[0].Script != "/BETA/d" {
		t.Errorf("unexpected delete script: %q", res.Ops[0].Script)
	}
}

// Whole-file destructive deletes (pattern matches every line) must be refused
// without --force and allowed with it.
func TestGenerateDeleteAllRequiresForce(t *testing.T) {
	_, err := Generate(Input{
		Intent:   plan.Parse("delete all lines matching .", nil),
		Contents: map[string]string{"a.go": "BETA"},
		Force:    false,
	})
	if err == nil {
		t.Fatal("expected refusal to delete every line without --force")
	}

	res, err := Generate(Input{
		Intent:   plan.Parse("delete all lines matching .", nil),
		Contents: map[string]string{"a.go": "BETA"},
		Force:    true,
	})
	if err != nil {
		t.Fatalf("unexpected error with --force: %v", err)
	}
	if res.Ops[0].Kind != "sed" || res.Ops[0].Script != "/./d" {
		t.Errorf("unexpected op: %+v", res.Ops[0])
	}
}

func TestGenerateNoContents(t *testing.T) {
	if _, err := Generate(Input{Intent: plan.Parse("replace a with b", nil)}); err == nil {
		t.Fatal("expected error when no contents provided")
	}
}
