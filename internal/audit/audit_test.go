package audit

import (
	"os"
	"path/filepath"
	"testing"

	"agentic-edit/internal/tx"
)

func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestAppendVerifyAndTamper(t *testing.T) {
	setupHome(t)
	if _, err := Append(Record{Kind: "apply", Root: "/r", TXID: "t1"}); err != nil {
		t.Fatal(err)
	}
	rec, err := Append(Record{Kind: "undo", Root: "/r", TXID: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Seq != 2 || rec.PrevHash == "" {
		t.Fatalf("record = %+v", rec)
	}
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
	// Tamper with the first record.
	path, _ := LogPath()
	data, _ := os.ReadFile(path)
	data[0] = 'X'
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(); err == nil {
		t.Fatal("expected tampering to be detected")
	}
}

func journalEntry(path string, before, after []byte) FileEntry {
	return FileEntry{
		Path: path, Action: "update",
		BeforeSHA: tx.Hash(before), AfterSHA: tx.Hash(after),
		Mode: 0o644,
	}
}

func TestRecoverPendingJournal(t *testing.T) {
	setupHome(t)
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	before := []byte("before\n")
	after := []byte("after\n")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Begin("tx-pending", root, "", []FileEntry{journalEntry("f.txt", before, after)},
		map[string][]byte{"f.txt": before})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the rename but before the journal was finalized.
	if err := os.WriteFile(path, after, 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := Recover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatalf("recovered content = %q", got)
	}
	if j.Meta.Status != "pending" {
		t.Fatalf("in-memory meta mutated: %s", j.Meta.Status)
	}
}

func TestUndoCommittedJournal(t *testing.T) {
	setupHome(t)
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	before := []byte("before\n")
	after := []byte("after\n")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Begin("tx-committed", root, "", []FileEntry{journalEntry("f.txt", before, after)},
		map[string][]byte{"f.txt": before})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, after, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Undo(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("res = %+v", res)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatalf("undone content = %q", got)
	}
}

func TestUndoRefusesChangedFile(t *testing.T) {
	setupHome(t)
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	before := []byte("before\n")
	after := []byte("after\n")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Begin("tx-conflict", root, "", []FileEntry{journalEntry("f.txt", before, after)},
		map[string][]byte{"f.txt": before})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(root, ""); err == nil {
		t.Fatal("expected undo to refuse a changed file")
	}
}
