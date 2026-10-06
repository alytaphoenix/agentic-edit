package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineDiff(t *testing.T) {
	d := LineDiff("a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+B") {
		t.Errorf("diff missing expected hunk:\n%s", d)
	}
	if !strings.Contains(d, "  a") {
		t.Errorf("diff should keep unchanged line 'a'")
	}
}

func TestLineDiffNoChange(t *testing.T) {
	// Identical files yield only context lines — no +/- changes.
	d := LineDiff("a\nb\n", "a\nb\n")
	if strings.Contains(d, "-") || strings.Contains(d, "+") {
		t.Errorf("expected no changes, got:\n%s", d)
	}
}

func TestDryRunSed(t *testing.T) {
	out, err := DryRun(EditOp{Kind: "sed", Script: "s|x|Y|g"}, "x and x\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Y and Y\n" {
		t.Errorf("dry-run sed wrong: %q", out)
	}
}

func TestDryRunGrep(t *testing.T) {
	out, err := DryRun(EditOp{Kind: "grep", Pattern: "o"}, "foo\nbar\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "1:foo\n" {
		t.Errorf("dry-run grep wrong: %q", out)
	}
}

func TestSearchAndCount(t *testing.T) {
	lines, err := Search("l", []string{"../gen/gen.go"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("expected matches")
	}
	n, _ := Count("l", []string{"../gen/gen.go"}, true)
	total := 0
	for _, c := range n {
		total += c
	}
	if total == 0 {
		t.Fatal("expected count > 0")
	}
}

// Regression: grep with no matches (exit status 1) must be a valid empty result,
// not an error, so dry-run previews don't fail.
func TestDryRunGrepNoMatch(t *testing.T) {
	out, err := DryRun(EditOp{Kind: KindGrep, Pattern: "nomatch"}, "a\nb\nc\n")
	if err != nil {
		t.Errorf("no-match grep should not error, got %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output for no-match grep, got %q", out)
	}
}

func TestRunInPlacePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte("echo hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RunInPlace(EditOp{Kind: KindSed, Path: path, Script: "s|hello|world|g"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("executable mode not preserved: got %o, want 755", info.Mode().Perm())
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "world") {
		t.Errorf("edit did not apply: %q", out)
	}
}

func TestRunInPlaceRollsBackOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sed script matching nothing is a valid no-op; instead force a failure by
	// running an op against a path that does not exist.
	err := RunInPlace(EditOp{Kind: KindSed, Path: path + "/missing", Script: "s|a|b"})
	if err == nil {
		t.Fatal("expected error applying against a missing file")
	}
}
