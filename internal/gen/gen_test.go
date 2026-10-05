package gen

import (
	"testing"

	"agentic-edit/internal/plan"
)

func ruleFor(t *testing.T, desc string) *Result {
	t.Helper()
	res, err := ruleBased(Input{
		Intent:   plan.Parse(desc, nil),
		Contents: map[string]string{"a.go": "const A = \"v1\"\nTODO fix\nconfig = 1\n"},
	})
	if err != nil {
		t.Fatalf("unexpected error for %q: %v", desc, err)
	}
	return res
}

func TestRuleBasedReplace(t *testing.T) {
	res := ruleFor(t, `replace "v1" with "v3"`)
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

func TestRuleBasedReplacePipeDelimiter(t *testing.T) {
	res, err := ruleBased(Input{
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

func TestRuleBasedDeletePreservesCase(t *testing.T) {
	res := ruleFor(t, `delete lines matching TODO`)
	want := "/" + "TODO" + "/d"
	if res.Ops[0].Script != want {
		t.Errorf("unexpected delete script: %q, want %q", res.Ops[0].Script, want)
	}
}

func TestRuleBasedInsertAfterPreservesCase(t *testing.T) {
	res := ruleFor(t, `insert "NEW" after line matching config`)
	want := "/config/a\\\n" + "NEW"
	if res.Ops[0].Script != want {
		t.Errorf("unexpected insert script: %q, want %q", res.Ops[0].Script, want)
	}
}

func TestRuleBasedVagueFallsToGrep(t *testing.T) {
	res := ruleFor(t, `find the config value`)
	if res.Ops[0].Kind != "grep" || res.Ops[0].Pattern != "config" {
		t.Errorf("expected grep for 'config', got %+v", res.Ops[0])
	}
}

func TestExtractPair(t *testing.T) {
	cases := []struct {
		in, x, y string
	}{
		{`replace "v1" with "v3"`, "v1", "v3"},
		{`replace foo with bar`, "foo", "bar"},
		{`rename old to new`, "old", "new"},
		{`replace a with b then c`, "a", "b then c"},
	}
	for _, c := range cases {
		gx, gy := extractPair(c.in)
		if gx != c.x || gy != c.y {
			t.Errorf("extractPair(%q) = (%q,%q), want (%q,%q)", c.in, gx, gy, c.x, c.y)
		}
	}
}

func TestRuleBasedUppercase(t *testing.T) {
	res, err := ruleBased(Input{
		Intent:   plan.Parse("uppercase every line", nil),
		Contents: map[string]string{"a.go": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := res.Ops[0]
	if op.Kind != "awk" || op.Script != `{ print toupper($0) }` {
		t.Errorf("unexpected uppercase op: %+v", op)
	}
}

func TestRuleBasedLowercase(t *testing.T) {
	res, err := ruleBased(Input{
		Intent:   plan.Parse("lowercase every line", nil),
		Contents: map[string]string{"a.go": "X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := res.Ops[0]
	if op.Kind != "awk" || op.Script != `{ print tolower($0) }` {
		t.Errorf("unexpected lowercase op: %+v", op)
	}
}

// Regression: "delete all lines matching X" must produce a sed delete, not a grep.
func TestRuleBasedDeleteAllMatching(t *testing.T) {
	res, err := ruleBased(Input{
		Intent:   plan.Parse("delete all lines matching BETA", nil),
		Contents: map[string]string{"a.go": "BETA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := res.Ops[0]
	if op.Kind != "sed" {
		t.Errorf("expected sed delete, got kind=%q (%+v)", op.Kind, op)
	}
	if op.Script != "/BETA/d" {
		t.Errorf("unexpected delete script: %q", op.Script)
	}
}
