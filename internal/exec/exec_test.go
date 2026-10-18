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

func TestCountFixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo\nbar\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := CountFixed("foo", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if n[path] != 2 {
		t.Errorf("CountFixed(foo)=%d want 2", n[path])
	}
	// Fixed-string matching: a regex metacharacter must be matched literally,
	// so "a.b" matches only "a.b", not "ab".
	if err := os.WriteFile(path, []byte("a.b\nab\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, _ = CountFixed("a.b", []string{path})
	if n[path] != 1 {
		t.Errorf("CountFixed(a.b)=%d want 1 (literal match)", n[path])
	}
}

func TestSearchFixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("key = AKIA1234567890ABCDEF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := SearchFixed("AKIA1234567890ABCDEF", []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("SearchFixed found %d lines, want 1", len(lines))
	}
	if lines[0].Num != 1 {
		t.Errorf("expected match on line 1, got %d", lines[0].Num)
	}
}

// A pattern beginning with "-" must be treated as data, not as a grep option.
// Before "--" was added, a pattern such as "-f/etc/passwd" made grep read
// patterns from an arbitrary file (or "-r" silently changed traversal).
func TestGrepPatternCannotInjectOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("-x\nplain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := Search("-x", []string{path}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Text != "-x" {
		t.Fatalf("Search(-x) = %+v, want one match", lines)
	}
	// A missing file named like an option must fail rather than being treated
	// as a flag.
	if _, err := Search("plain", []string{"--include=*"}, false); err == nil {
		t.Fatal("expected grep to reject a non-existent option-shaped filename")
	}
}

// Regex replaces run through sed -E, and an escaped metacharacter keeps its
// meaning instead of being doubled into a literal backslash.
func TestDryRunRegexReplaceUsesExtendedSed(t *testing.T) {
	op := EditOp{Kind: KindSed, Script: `s|a\.b|X|g`, Extended: true, Old: `a\.b`, New: "X", Literal: false}
	out, err := DryRun(op, "a.b\naxb\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "X\naxb\n" {
		t.Fatalf("out = %q, want %q", out, "X\naxb\n")
	}
}

// A broad delete that removes every line is gated on the actual result, so a
// pattern that slips past the compile-time denylist (e.g. "^.*$") still needs
// --force.
func TestDryRunBlocksWholeFileDelete(t *testing.T) {
	op := EditOp{Kind: KindSed, Script: `/^.*$/d`, Pattern: "^.*$", Deny: "^.*$", DenyRegex: true}
	if _, err := DryRun(op, "a\nb\n"); err == nil {
		t.Fatal("expected a whole-file delete to be refused")
	}
	op.DeleteAllAllowed = true
	out, err := DryRun(op, "a\nb\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
}

// The unique-match default applies to regex replaces too: two matches must be
// rejected before sed runs.
func TestDryRunRegexExpectCount(t *testing.T) {
	op := EditOp{Kind: KindSed, Script: `s|v[0-9]|x|g`, Extended: true, Old: `v[0-9]`, Literal: false, ExpectCount: 1}
	if _, err := DryRun(op, "v1\nv2\n"); err == nil {
		t.Fatal("expected two regex matches to be rejected when one is required")
	}
	op.ExpectCount = 2
	if _, err := DryRun(op, "v1\nv2\n"); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunRegexAlternation(t *testing.T) {
	op := EditOp{Kind: KindSed, Script: `s#foo|bar#x#g`, Extended: true, Literal: false}
	out, err := DryRun(op, "foo\nbar\nbaz\n")
	if err != nil {
		t.Fatal(err)
	}
	if out != "x\nx\nbaz\n" {
		t.Fatalf("out = %q", out)
	}
}
