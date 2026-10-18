package applyplan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentic-edit/internal/ir"
	"agentic-edit/internal/policy"
)

func ops(t *testing.T, body string) []ir.Op {
	t.Helper()
	o, err := ir.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestBuildApplyRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := ops(t, `[{"kind":"replace_literal","path":"a.txt","old":"v1","new":"v2","expect_count":1}]`)
	p, _, err := Build(root, o, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest == "" || len(p.Files) != 1 || p.Files[0].Action != ActionUpdate {
		t.Fatalf("plan = %+v", p)
	}
	if _, _, _, err := Apply(root, p, p.Digest, Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestApplyRefusesTamperedPlanAndStaleFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := ops(t, `[{"kind":"replace_literal","path":"a.txt","old":"v1","new":"v2","expect_count":1}]`)
	p, _, err := Build(root, o, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tampered := *p
	tampered.Files = append([]FilePlan(nil), p.Files...)
	tampered.Files[0].AfterSHA = "sha256:deadbeef"
	if _, _, _, err := Apply(root, &tampered, tampered.Digest, Options{}); err == nil {
		t.Fatal("expected a tampered plan to be refused")
	}

	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Apply(root, p, p.Digest, Options{}); err == nil {
		t.Fatal("expected a stale file conflict")
	} else if _, ok := err.(*ConflictError); !ok {
		t.Fatalf("error = %T %v, want ConflictError", err, err)
	}
}

func TestSyntaxGateRefusesRegression(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := ops(t, `[{"kind":"replace_literal","path":"main.go","old":"package main","new":"package","expect_count":1}]`)
	p, _, err := Build(root, o, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Apply(root, p, p.Digest, Options{}); err == nil {
		t.Fatal("expected the syntax gate to refuse the edit")
	} else if _, ok := err.(*SyntaxError); !ok {
		t.Fatalf("error = %T %v, want SyntaxError", err, err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "package main") {
		t.Fatalf("file was modified: %q", got)
	}
}

func TestLimitsDenyLargeChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	o := ops(t, `[{"kind":"delete_range","path":"a.txt","start":1,"end":10}]`)
	_, _, err := Build(root, o, Options{Limits: policy.Limits{MaxChangedLines: 3}})
	if err == nil || !policy.IsDenied(err) {
		t.Fatalf("err = %v, want policy denial", err)
	}
}

func TestFileLevelOpsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "del.txt"), []byte("gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "from.txt"), []byte("move\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := ops(t, `[
		{"kind":"create_file","path":"new.txt","content":"hello\n","mode":384},
		{"kind":"delete_file","path":"del.txt"},
		{"kind":"rename_file","from":"from.txt","to":"to.txt"},
		{"kind":"chmod","path":"run.sh","mode":493}
	]`)
	p, _, err := Build(root, o, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Apply(root, p, p.Digest, Options{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(got) != "hello\n" {
		t.Fatalf("create = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("delete did not remove the file")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "to.txt")); string(got) != "move\n" {
		t.Fatalf("rename = %q", got)
	}
	if info, _ := os.Stat(filepath.Join(root, "run.sh")); info.Mode().Perm() != 0o755 {
		t.Fatalf("chmod = %o", info.Mode().Perm())
	}
}

func TestBuildRejectsEscapeAndProtected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(root, []ir.Op{{Kind: ir.DeleteFile, Path: "../x"}}, Options{}); err == nil {
		t.Fatal("expected an escape to be rejected")
	}
	if _, _, err := Build(root, []ir.Op{{Kind: ir.DeleteFile, Path: ".env"}}, Options{}); err == nil || !policy.IsDenied(err) {
		t.Fatalf("err = %v, want protected denial", err)
	}
}
