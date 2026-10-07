package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/gen"
)

// setup writes content to a fresh temp file and returns its path.
func setup(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(res *gen.Result, path, beforeContent, afterContent string) Result {
	before := map[string]string{path: beforeContent}
	after := map[string]string{path: afterContent}
	return Run(res, before, after, nil, false, nil)
}

func TestVerifyAssertPassesAfterApply(t *testing.T) {
	path := setup(t, "const A = \"v1\"\n")
	// Apply the replace so the new value is actually on disk.
	if err := exec.RunInPlace(exec.EditOp{Kind: exec.KindSed, Path: path, Script: "s|v1|v3|g"}); err != nil {
		t.Fatal(err)
	}
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "s|v1|v3|g", Assert: "v3", Deny: "v1"}}}
	r := run(res, path, "const A = \"v1\"\n", "const A = \"v3\"\n")
	if !r.OK {
		t.Fatalf("expected pass, got notes: %v", r.Notes)
	}
}

func TestVerifyAssertFailsWhenEditDidNotLand(t *testing.T) {
	path := setup(t, "const A = \"v1\"\n")
	// Do NOT apply: the guard must catch that v3 is missing on disk.
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "s|v1|v3|g", Assert: "v3", Deny: "v1"}}}
	r := run(res, path, "const A = \"v1\"\n", "const A = \"v3\"\n")
	if r.OK {
		t.Fatalf("expected failure when edit did not land; notes=%v", r.Notes)
	}
	joined := strings.Join(r.Notes, "\n")
	if !strings.Contains(joined, "grep found") || !strings.Contains(joined, "diff (before -> after)") {
		t.Errorf("expected error diff in notes, got: %v", r.Notes)
	}
}

// A no-op op (nothing changed) must never trigger a false assertion failure.
func TestVerifyNoOpIsSkipped(t *testing.T) {
	path := setup(t, "const A = \"v1\"\n")
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "s|doesnotexist|xyz|g", Assert: "xyz", Deny: "doesnotexist"}}}
	r := run(res, path, "const A = \"v1\"\n", "const A = \"v1\"\n")
	if !r.OK {
		t.Fatalf("no-op op should not fail; notes=%v", r.Notes)
	}
}

func TestVerifyDenyDeletePassesAfterApply(t *testing.T) {
	path := setup(t, "TODO fix\nok line\n")
	if err := exec.RunInPlace(exec.EditOp{Kind: exec.KindSed, Path: path, Script: "/TODO/d"}); err != nil {
		t.Fatal(err)
	}
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Pattern: "TODO", Script: "/TODO/d", Deny: "TODO"}}}
	r := run(res, path, "TODO fix\nok line\n", "ok line\n")
	if !r.OK {
		t.Fatalf("expected delete to pass, got notes: %v", r.Notes)
	}
}

func TestVerifyDenyDeleteFailsWhenNotApplied(t *testing.T) {
	path := setup(t, "TODO fix\nok line\n")
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Pattern: "TODO", Script: "/TODO/d", Deny: "TODO"}}}
	r := run(res, path, "TODO fix\nok line\n", "ok line\n")
	if r.OK {
		t.Fatalf("expected DENY failure when TODO still present; notes=%v", r.Notes)
	}
}

func TestVerifyInsertAssertPassesAfterApply(t *testing.T) {
	path := setup(t, "config = 1\n")
	if err := exec.RunInPlace(exec.EditOp{Kind: exec.KindSed, Path: path, Script: "/config/a\\\nNEW"}); err != nil {
		t.Fatal(err)
	}
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "/config/a\\\nNEW", Assert: "NEW"}}}
	r := run(res, path, "config = 1\n", "config = 1\nNEW\n")
	if !r.OK {
		t.Fatalf("expected insert assert to pass, got notes: %v", r.Notes)
	}
}

func TestVerifyInsertFailsWhenNotApplied(t *testing.T) {
	path := setup(t, "config = 1\n")
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "/config/a\\\nNEW", Assert: "NEW"}}}
	r := run(res, path, "config = 1\n", "config = 1\nNEW\n")
	if r.OK {
		t.Fatalf("expected insert assert to fail when NEW missing; notes=%v", r.Notes)
	}
}

// A non-global replace legitimately leaves the old value elsewhere; with no
// Deny set, the guard must not fail on it.
func TestVerifyNonGlobalReplaceNoFalseDeny(t *testing.T) {
	path := setup(t, "a-a-a\n")
	if err := exec.RunInPlace(exec.EditOp{Kind: exec.KindSed, Path: path, Script: "s|a|b|"}); err != nil {
		t.Fatal(err)
	}
	res := &gen.Result{Ops: []exec.EditOp{{Kind: exec.KindSed, Script: "s|a|b|", Assert: "b"}}}
	r := run(res, path, "a-a-a\n", "b-a-a\n")
	if !r.OK {
		t.Fatalf("non-global replace should pass (no Deny); notes=%v", r.Notes)
	}
}

// The on-disk dry-run must agree with RunInPlace so no-op detection is reliable.
func TestVerifyDryRunAgreesWithApply(t *testing.T) {
	path := setup(t, "hello world\n")
	op := exec.EditOp{Kind: exec.KindSed, Path: path, Script: "s|hello|goodbye|g", Assert: "goodbye"}
	if err := exec.RunInPlace(op); err != nil {
		t.Fatal(err)
	}
	// DryRun on the original content should match the applied result.
	dry, err := exec.DryRun(exec.EditOp{Kind: exec.KindSed, Script: "s|hello|goodbye|g"}, "hello world\n")
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if dry != string(disk) {
		t.Fatalf("dry-run (%q) disagrees with apply (%q)", dry, string(disk))
	}
}
