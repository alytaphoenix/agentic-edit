package ir

import (
	"strings"
	"testing"
)

func TestParseArrayAndDocument(t *testing.T) {
	ops, err := Parse([]byte(`[{"kind":"replace_literal","path":"a.txt","old":"v1","new":"v2","expect_count":1}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Kind != ReplaceLiteral {
		t.Fatalf("ops = %+v", ops)
	}
	ops, err = Parse([]byte(`{"version":1,"ops":[{"kind":"delete_file","path":"x"}]}`))
	if err != nil || len(ops) != 1 {
		t.Fatalf("document parse: %+v %v", ops, err)
	}
}

func TestParseRejectsUnknownAndTrailing(t *testing.T) {
	cases := []string{
		`[{"kind":"delete_file","path":"x","scope":"/y/"}]`,
		`[{"kind":"delete_file","path":"x"}] []`,
		`[{"kind":"delete_file","path":"x","mode":420}]`,
		`[{"kind":"rename_file","from":"a","to":"b","path":"c"}]`,
	}
	for _, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("expected rejection: %s", in)
		}
	}
}

func TestValidateRejectsUnsafePaths(t *testing.T) {
	bad := []string{"../x", "/etc/passwd", "a/../../b", ".", "./a", "a/./b", "a\x00b", `a\..\b`, `C:evil`}
	for _, p := range bad {
		if _, err := Parse([]byte(`[{"kind":"delete_file","path":"` + strings.ReplaceAll(p, "\x00", "\\u0000") + `"}]`)); err == nil {
			t.Errorf("path %q should be rejected", p)
		}
	}
	for _, p := range []string{"a.txt", "dir/a.txt", ".env"} {
		if _, err := Parse([]byte(`[{"kind":"delete_file","path":"` + p + `"}]`)); err != nil {
			t.Errorf("path %q should be accepted: %v", p, err)
		}
	}
}

func TestValidateKindRequirements(t *testing.T) {
	bad := []string{
		`[{"kind":"replace_literal","path":"a"}]`,
		`[{"kind":"replace_regex","path":"a","old":"["}]`,
		`[{"kind":"insert","path":"a","anchor":"x"}]`,
		`[{"kind":"replace_range","path":"a","start":2,"end":1,"text":"x"}]`,
		`[{"kind":"chmod","path":"a"}]`,
		`[{"kind":"rename_file","path":"a","from":"a","to":"a"}]`,
	}
	for _, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("expected rejection: %s", in)
		}
	}
}

func TestParseLimitsOps(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < MaxOps+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"kind":"delete_file","path":"x"}`)
	}
	b.WriteString("]")
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("expected the ops limit to be enforced")
	}
}
