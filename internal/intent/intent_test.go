package intent

import "testing"

func TestParseReplace(t *testing.T) {
	ops, err := Parse(`replace "v1" with "v3"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("want 1 op, got %d", len(ops))
	}
	op := ops[0]
	if op.Kind != KindSed || op.X != "v1" || op.Y != "v3" || !op.Global {
		t.Errorf("unexpected op: %+v", op)
	}
}

func TestParseRename(t *testing.T) {
	ops, err := Parse("rename old to new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].X != "old" || ops[0].Y != "new" {
		t.Errorf("want x=old y=new, got %+v", ops[0])
	}
}

func TestParseInsert(t *testing.T) {
	ops, err := Parse(`insert "NEW" before line matching config`, nil)
	if err != nil {
		t.Fatal(err)
	}
	op := ops[0]
	if op.Kind != KindSed || op.Anchor != "config" || op.Text != "NEW" || !op.InsertBefore {
		t.Errorf("unexpected insert op: %+v", op)
	}
}

func TestParseDelete(t *testing.T) {
	ops, err := Parse(`delete lines matching TODO`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].Kind != KindSed || ops[0].Pattern != "TODO" {
		t.Errorf("unexpected delete op: %+v", ops[0])
	}
}

func TestParseUppercase(t *testing.T) {
	ops, err := Parse("uppercase every line", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].Kind != KindAwk || ops[0].Transform != "upper" {
		t.Errorf("unexpected op: %+v", ops[0])
	}
}

func TestParseVagueFallsToGrep(t *testing.T) {
	ops, err := Parse("find the config value", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].Kind != KindGrep || ops[0].Pattern != "config" {
		t.Errorf("expected grep for 'config', got %+v", ops[0])
	}
}

func TestParseVagueTooVague(t *testing.T) {
	if _, err := Parse("", nil); err == nil {
		t.Fatal("expected error for empty instruction")
	}
}

func TestParseRejectsMultiline(t *testing.T) {
	ops, err := Parse("replace \"a\nb\" with \"c\"", nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if err := ops[0].Validate(); err == nil {
		t.Fatal("expected multi-line replace value to fail validation")
	}
}

func TestParseOpsValid(t *testing.T) {
	txt := `[{"kind":"sed","x":"v1","y":"v3"},{"kind":"grep","pattern":"TODO"}]`
	ops, ok := ParseOps(txt)
	if !ok || len(ops) != 2 {
		t.Fatalf("ParseOps(%q) ok=%v len=%d", txt, ok, len(ops))
	}
	if ops[1].Kind != KindGrep {
		t.Errorf("unexpected op: %+v", ops[1])
	}
}

func TestParseOpsRejectsBadScript(t *testing.T) {
	// A raw shell command must not be accepted as a typed op.
	for _, txt := range []string{
		`[{"kind":"sed","script":"rm -rf /"}]`,
		`not json`,
		`[]`,
	} {
		if _, ok := ParseOps(txt); ok {
			t.Errorf("ParseOps(%q) should be rejected", txt)
		}
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
