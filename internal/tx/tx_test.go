package tx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitWritesExactBytesAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.sh")
	before := []byte("#!/bin/sh\necho hi\n")
	if err := os.WriteFile(path, before, 0o755); err != nil {
		t.Fatal(err)
	}
	data, mode, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after := []byte("#!/bin/sh\necho bye\n")

	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cleanup()
	if err := tr.Stage(path, data, after, mode); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(after) {
		t.Fatalf("content = %q, want %q", got, after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".aed-") {
			t.Errorf("leftover staging file %q", e.Name())
		}
	}
}

func TestCommitConflictDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, mode, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cleanup()
	if err := tr.Stage(path, data, []byte("edited\n"), mode); err != nil {
		t.Fatal(err)
	}
	// Simulate a concurrent writer after we read the file.
	if err := os.WriteFile(path, []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err == nil {
		t.Fatal("expected a conflict")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "someone else\n" {
		t.Fatalf("concurrent content was overwritten: %q", got)
	}
}

func TestStageRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cleanup()
	if err := tr.Stage(link, []byte("x"), []byte("y"), 0o644); err == nil {
		t.Fatal("expected symlink to be rejected")
	}
}

func TestRollbackRestoresEveryFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("a-before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b-before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	da, ma, _ := ReadFile(a)
	db, mb, _ := ReadFile(b)
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cleanup()
	if err := tr.Stage(a, da, []byte("a-after\n"), ma); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stage(b, db, []byte("b-after\n"), mb); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{a: "a-before\n", b: "b-before\n"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestNoPredictableSiblingPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-create the old predictable paths as symlinks. They must not be
	// followed or used.
	for _, name := range []string{".aed.tmp", ".bak"} {
		if err := os.Symlink(outside, path+name); err != nil {
			t.Fatal(err)
		}
	}
	data, mode, _ := ReadFile(path)
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Cleanup()
	if err := tr.Stage(path, data, []byte("y\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "safe\n" {
		t.Fatalf("outside file was modified: %q", got)
	}
}
