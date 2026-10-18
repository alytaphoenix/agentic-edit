package lang

import (
	"context"
	"os/exec"
	"testing"
)

func TestShebangInterpreterParsing(t *testing.T) {
	cases := []struct{ content, want string }{
		{"#!/bin/bash\necho hi", "bash"},
		{"#!/usr/bin/env python3\nprint(1)", "python3"},
		{"#!/usr/bin/env -i bash\nx", "bash"},
		{"#! /bin/sh -e\necho hi", "sh"},
		{"#!/bin/zsh", "zsh"},
		{"echo hi", ""},
	}
	for _, tc := range cases {
		got := shebangInterpreter(tc.content)
		if got != tc.want {
			t.Errorf("shebangInterpreter(%q) = %q; want %q", tc.content, got, tc.want)
		}
	}
}

func TestCheckerDispatchByExtension(t *testing.T) {
	if CheckerFor("script.sh", "") == nil || CheckerFor("script.sh", "").Name() != "bash" {
		t.Fatal("expected bash checker for .sh")
	}
	if CheckerFor("mod.py", "") == nil || CheckerFor("mod.py", "").Name() != "python" {
		t.Fatal("expected python checker for .py")
	}
	if CheckerFor("Makefile", "#!/bin/bash\n") == nil || CheckerFor("Makefile", "#!/bin/bash\n").Name() != "bash" {
		t.Fatal("expected bash checker for extensionless bash shebang")
	}
	if CheckerFor("x.go", "") == nil || CheckerFor("x.go", "").Name() != "go" {
		t.Fatal("expected go checker for .go")
	}
	if CheckerFor("x.json", "") == nil || CheckerFor("x.json", "").Name() != "json" {
		t.Fatal("expected json checker for .json")
	}
	if CheckerFor("x.toml", "") == nil || CheckerFor("x.toml", "").Name() != "toml" {
		t.Fatal("expected toml checker for .toml")
	}
	if CheckerFor("x.ini", "") == nil || CheckerFor("x.ini", "").Name() != "ini" {
		t.Fatal("expected ini checker for .ini")
	}
	if CheckerFor("x.xml", "") == nil || CheckerFor("x.xml", "").Name() != "xml" {
		t.Fatal("expected xml checker for .xml")
	}
	if CheckerFor("x.properties", "") == nil || CheckerFor("x.properties", "").Name() != "properties" {
		t.Fatal("expected properties checker for .properties")
	}
	if CheckerFor("README.md", "# hi") != nil {
		t.Fatal("expected no checker for .md")
	}
}

func TestUnknownLanguageIsSkippedNotPassed(t *testing.T) {
	rep := Check(context.Background(), "notes.txt", "hello")
	if rep.Status != Skipped {
		t.Fatalf("notes.txt: status = %v, want skipped", rep.Status)
	}
	if rep.Status == Passed {
		t.Fatal("unknown type must never be reported as passed")
	}
}

func TestBashCheckerReal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	pass := Check(context.Background(), "ok.sh", "echo 'hi'\nif true; then\n  echo ok\nfi\n")
	if pass.Status != Passed {
		t.Fatalf("good script: %+v", pass)
	}
	bad := Check(context.Background(), "bad.sh", "echo \"hi\nif true; then\n")
	if bad.Status != Failed || len(bad.Diags) == 0 {
		t.Fatalf("bad script: %+v", bad)
	}
	if bad.Diags[0].Line == 0 {
		t.Fatalf("expected a line number, got %+v", bad.Diags[0])
	}
	// env override points at a missing binary => ErrUnavailable path.
	t.Setenv("AED_BASH", "/definitely/not/bash")
	rep := Check(context.Background(), "bad.sh", "echo \"hi\n")
	if rep.Status != Skipped || rep.Reason == "" {
		t.Fatalf("missing tool should be skipped, got %+v", rep)
	}
}

func TestPythonCheckerReal(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	pass := Check(context.Background(), "ok.py", "def g():\n    return 1\n")
	if pass.Status != Passed {
		t.Fatalf("good code: %+v", pass)
	}
	bad := Check(context.Background(), "bad.py", "def f(\n    pass\n")
	if bad.Status != Failed || len(bad.Diags) == 0 {
		t.Fatalf("bad code: %+v", bad)
	}
	if bad.Diags[0].Line == 0 {
		t.Fatalf("expected line number, got %+v", bad.Diags[0])
	}
	t.Setenv("AED_PYTHON", "/definitely/not/python3")
	rep := Check(context.Background(), "bad.py", "def f(\n    pass\n")
	if rep.Status != Skipped {
		t.Fatalf("missing tool should be skipped, got %+v", rep)
	}
}

func TestNativeSyntaxCheckers(t *testing.T) {
	if got := Check(context.Background(), "x.go", "package main\nfunc main() {\n").Status; got != Failed {
		t.Fatalf("bad go: want failed, got %v", got)
	}
	if got := Check(context.Background(), "x.go", "package main\nfunc main() {}\n").Status; got != Passed {
		t.Fatalf("good go: want passed, got %v", got)
	}
	if got := Check(context.Background(), "x.json", `{"a":1,}`).Status; got != Failed {
		t.Fatalf("bad json: want failed, got %v", got)
	}
	if got := Check(context.Background(), "x.json", `{"a":1}`).Status; got != Passed {
		t.Fatalf("good json: want passed, got %v", got)
	}
	if got := Check(context.Background(), "x.properties", "key=value\noops\n").Status; got != Failed {
		t.Fatalf("bad properties: want failed, got %v", got)
	}
	if got := Check(context.Background(), "x.properties", "key=value\n").Status; got != Passed {
		t.Fatalf("good properties: want passed, got %v", got)
	}
	if got := Check(context.Background(), "x.toml", "x = 1\nx = 2\n").Status; got != Failed {
		t.Fatalf("bad toml: want failed, got %v", got)
	}
	if got := Check(context.Background(), "x.toml", "x = 1\n").Status; got != Passed {
		t.Fatalf("good toml: want passed, got %v", got)
	}
	if got := Check(context.Background(), "x.xml", "<a><b/></a").Status; got != Failed {
		t.Fatalf("bad xml: want failed, got %v", got)
	}
	if got := Check(context.Background(), "x.xml", "<a><b/></a>").Status; got != Passed {
		t.Fatalf("good xml: want passed, got %v", got)
	}
}

func TestCheckEditsOnlyReportsRegressions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	before := map[string]string{"a.sh": "echo ok\n"}
	after := map[string]string{
		"a.sh":    "echo \"\n",
		"b.sh":    "echo \"\n",
		"kept.sh": "echo ok\n",
		"c.sh":    "broken-before-and-new",
	}
	before["b.sh"] = "echo \"\n"
	before["kept.sh"] = "echo ok\n"

	reports := CheckEdits(context.Background(), before, after)
	by := map[string]Report{}
	for _, r := range reports {
		by[r.Path] = r
	}
	if by["a.sh"].Status != Failed {
		t.Fatalf("a.sh should be a new regression, got %v", by["a.sh"].Status)
	}
	if by["b.sh"].Status != Skipped {
		t.Fatalf("b.sh was already broken, should be skipped, got %v", by["b.sh"].Status)
	}
	if _, ok := by["kept.sh"]; ok {
		t.Fatal("unchanged file must not be reported")
	}
}

func TestRegistryOverride(t *testing.T) {
	const name = "stublang"
	c1 := &CommandChecker{Language: name, Extensions: []string{".stub"}, Bin: "true"}
	c2 := &CommandChecker{Language: name, Extensions: []string{".stub2"}, Bin: "true"}
	Register(c1)
	if CheckerFor("x.stub", "") == nil || CheckerFor("x.stub", "").Name() != name {
		t.Fatal("first registration not found")
	}
	Register(c2) // replaces
	if CheckerFor("x.stub", "") != nil {
		t.Fatal("old extension should no longer match after override")
	}
	if CheckerFor("x.stub2", "") == nil || CheckerFor("x.stub2", "").Name() != name {
		t.Fatal("new registration not effective")
	}
}
