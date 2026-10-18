package tx

import (
	"os"
	"path/filepath"
	"testing"
)

func newTx(t *testing.T, dir string) *Tx {
	t.Helper()
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Cleanup)
	return tr
}

func TestCreateAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")
	tr := newTx(t, dir)
	if err := tr.StageCreate(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello\n" {
		t.Fatalf("content = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rollback did not remove the created file")
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newTx(t, dir)
	if err := tr.StageCreate(path, []byte("y"), 0o644); err == nil {
		t.Fatal("expected an existing file to be refused")
	}
}

func TestDeleteAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	before := []byte("keep me\n")
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	tr := newTx(t, dir)
	if err := tr.StageDelete(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("commit did not delete the file")
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatalf("rollback content = %q", got)
	}
}

func TestRenameAndRollback(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "a.txt")
	to := filepath.Join(dir, "b.txt")
	before := []byte("data\n")
	if err := os.WriteFile(from, before, 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newTx(t, dir)
	if err := tr.StageRename(from, to, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatal("source still exists")
	}
	if got, _ := os.ReadFile(to); string(got) != string(before) {
		t.Fatalf("destination content = %q", got)
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(to); !os.IsNotExist(err) {
		t.Fatal("rollback left the destination")
	}
	if got, _ := os.ReadFile(from); string(got) != string(before) {
		t.Fatalf("rollback source content = %q", got)
	}
}

func TestChmodAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := newTx(t, dir)
	if err := tr.StageChmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := tr.Rollback(); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("rollback mode = %o", info.Mode().Perm())
	}
}

func TestCommitLeavesNoHiddenFilesAfterCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	before := []byte("x\n")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.StageDelete(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	tr.Cleanup()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) >= 4 && e.Name()[:4] == ".aed" {
			t.Fatalf("leftover staging/trash file %q", e.Name())
		}
	}
}
