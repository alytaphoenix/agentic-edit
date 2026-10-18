package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runEditCmd drives Run and captures stdout+stderr.
func runEditCmd(dir string, args ...string) (int, string) {
	return runEditStdin(dir, "n", args...)
}

// runEditStdin drives Run with explicit stdin and captures stdout+stderr.
func runEditStdin(dir, stdin string, args ...string) (int, string) {
	var out, errb bytes.Buffer
	all := append([]string{"edit", "--root", dir}, args...)
	code := Run(all, strings.NewReader(stdin), &out, &errb)
	return code, out.String() + errb.String()
}

// The verify/no-verify flags must have opposite semantics; sharing one bool
// variable made --no-verify a no-op. This test guards that regression.
func TestVerifyFlagDefaultRunsGuard(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, "verify[ok]") {
		t.Errorf("expected default guard to run, out=%q", out)
	}
}

func TestNoVerifyFlagDisablesGuard(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditStdin(dir, "n", "--action", `replace "v1" with "v3"`, "-f", path, "-y", "--no-verify")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if strings.Contains(out, "verify[") {
		t.Errorf("--no-verify should disable the guard, out=%q", out)
	}
}

func TestShowSecretsDisablesScrub(t *testing.T) {
	dir := t.TempDir()
	// The file has a changeable line (name = "v1") plus a context line carrying
	// a secret, so the preview diff includes the secret-bearing context line.
	content := `name = "v1"\nconst API = "AKIA1234567890ABCDEF"\n`
	path := filepath.Join(dir, "demo.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default: secrets are redacted from the preview.
	_, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path)
	if !strings.Contains(out, "[REDACTED") {
		t.Errorf("expected secret to be redacted by default, out=%q", out)
	}
	if strings.Contains(out, "AKIA1234567890ABCDEF") {
		t.Errorf("secret leaked into output, out=%q", out)
	}

	// --show-secrets: the raw value is emitted and nothing is redacted.
	_, out = runEditStdin(dir, "n", "--action", `replace "v1" with "v3"`, "-f", path, "--show-secrets")
	if !strings.Contains(out, "AKIA1234567890ABCDEF") {
		t.Errorf("expected raw secret with --show-secrets, out=%q", out)
	}
	if strings.Contains(out, "[REDACTED") {
		t.Errorf("did not expect redaction with --show-secrets, out=%q", out)
	}
}

// Reaching the guard-failure path: replacing "b" with "ab" leaves the old value
// "b" present as a substring of "ab", so the conservative Deny check fails. This
// is intentional (deny-absence is conservative) and reliably exercises the
// report-and-restore guard.
func guardFailureArgs(dir string, path string) []string {
	return []string{"--action", `replace "b" with "ab"`, "-f", path, "-y"}
}

func TestGuardFailureRollsBackAndExits3(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `x = "b"`)
	orig := `x = "b"`

	// -y: report only, automatic rollback, exit 3.
	code, out := runEditStdin(dir, "n", guardFailureArgs(dir, path)...)
	if code != 3 {
		t.Fatalf("exit=%d, want 3; out=%q", code, out)
	}
	if !strings.Contains(out, "verify[FAILED]") {
		t.Errorf("expected FAILED status, out=%q", out)
	}
	if !strings.Contains(out, "restored original files") {
		t.Errorf("expected automatic rollback, out=%q", out)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimRight(string(got), "\n") != orig {
		t.Errorf("expected file restored to %q, got %q", orig, string(got))
	}

	// --keep-on-failure preserves the old "leave the edit in place" behavior.
	code, out = runEditStdin(dir, "y\n", "--action", `replace "b" with "ab"`, "-f", path, "-y", "--keep-on-failure")
	if code != 3 {
		t.Fatalf("keep-on-failure exit=%d, want 3; out=%q", code, out)
	}
	got, _ = os.ReadFile(path)
	if strings.TrimRight(string(got), "\n") != `x = "ab"` {
		t.Errorf("--keep-on-failure should keep the edit, got %q", string(got))
	}

	// Reset and retry applying without -y but with piped (non-TTY) stdin:
	// --dry-run=false forces the apply, the confirm reads "y", and the guard
	// still fails. It rolls back automatically.
	if err := os.WriteFile(path, []byte(orig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runEditStdin(dir, "y\n", "--action", `replace "b" with "ab"`, "-f", path, "--dry-run=false")
	if code != 3 {
		t.Fatalf("no-y non-interactive exit=%d, want 3; out=%q", code, out)
	}
	if !strings.Contains(out, "restored original files") {
		t.Errorf("expected automatic rollback, out=%q", out)
	}
}

// confirmRestore accepts y/yes (case-insensitive) and rejects everything else.
func TestConfirmRestore(t *testing.T) {
	cases := map[string]bool{
		"y\n":      true,
		"yes\n":    true,
		"Y\n":      true,
		"  yes \n": true,
		"n\n":      false,
		"no\n":     false,
		"\n":       false,
		"trash\n":  false,
	}
	for in, want := range cases {
		if got := confirmRestore(strings.NewReader(in), io.Discard); got != want {
			t.Errorf("confirmRestore(%q)=%v, want %v", in, got, want)
		}
	}
}

// isTerminal is false for non-file readers and for regular files, and true for
// character devices such as /dev/null.
func TestIsTerminal(t *testing.T) {
	if isTerminal(strings.NewReader("x")) {
		t.Error("strings.Reader should not be a terminal")
	}
	dir := t.TempDir()
	fpath := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(fpath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(fpath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file should not be a terminal")
	}
	// /dev/null is a character device on macOS; treat its absence gracefully.
	nd, err := os.Open("/dev/null")
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	defer nd.Close()
	if !isTerminal(nd) {
		t.Error("/dev/null should be detected as a terminal (char device)")
	}
}

// writeLine writes a single line to a fresh file in dir and returns its path.
func writeLine(t *testing.T, dir, line string) string {
	t.Helper()
	path := filepath.Join(dir, "demo.go")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The old implementation wrote to predictable "file.aed.tmp" and "file.bak"
// siblings. Pre-creating those as symlinks must not let an edit write outside
// the workspace.
func TestSymlinkSiblingPathsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".aed.tmp", ".bak"} {
		if err := os.Symlink(outside, path+suffix); err != nil {
			t.Fatal(err)
		}
	}
	code, out := runEditCmd(dir, "--action", `replace "x=1" with "x=2"`, "-f", path, "-y")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "safe\n" {
		t.Fatalf("outside file modified: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("target became a symlink")
	}
	if got, _ := os.ReadFile(path); string(got) != "x=2\n" {
		t.Fatalf("target content = %q", got)
	}
}

func TestSymlinkTargetRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("x=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "x=1" with "x=2"`, "-f", link, "-y")
	if code == 0 {
		t.Fatalf("expected symlink target to be refused, out=%q", out)
	}
	if got, _ := os.ReadFile(target); string(got) != "x=1\n" {
		t.Fatalf("symlink target modified: %q", got)
	}
}

func TestLiteralReplaceDoesNotActAsRegex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a.b\naxb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "a.b" with "X"`, "-f", path, "-y")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "X\naxb\n" {
		t.Fatalf("literal replace changed the wrong text: %q", got)
	}
}

func TestReplaceRequiresUniqueByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("v1\nv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y")
	if code == 0 {
		t.Fatalf("expected ambiguous replace to fail, out=%q", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\nv1\n" {
		t.Fatalf("file was modified despite ambiguity: %q", got)
	}
	code, out = runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y", "--all")
	if code != 0 {
		t.Fatalf("--all exit=%d out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2\nv2\n" {
		t.Fatalf("--all result = %q", got)
	}
}

func TestProtectedPathRequiresOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("TOKEN=v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y")
	if code != 5 {
		t.Fatalf("exit=%d, want 5; out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "TOKEN=v1\n" {
		t.Fatalf("protected file modified: %q", got)
	}
	code, out = runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y", "--allow-protected")
	if code != 0 {
		t.Fatalf("override exit=%d out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "TOKEN=v2\n" {
		t.Fatalf("override result = %q", got)
	}
}

// A grep-only plan is a search. It must not build a fake "after" that looks
// like the whole file was deleted.
func TestGrepPlanIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("axb\nzzz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `find zzz`, "-f", path, "-y")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if strings.Contains(out, "-axb") || strings.Contains(out, "applied:") {
		t.Fatalf("grep was treated as an edit: %q", out)
	}
	if !strings.Contains(out, "zzz") {
		t.Fatalf("expected the matching line, got %q", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "axb\nzzz\n" {
		t.Fatalf("grep-only edit changed the file: %q", got)
	}
}

func TestJSONResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run([]string{"edit", "--root", dir, "--action", `replace "v1" with "v2"`, "-f", path, "--json"}, strings.NewReader("n"), &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", out.String(), err)
	}
	if got["ok"] != true || got["dry_run"] != true {
		t.Fatalf("unexpected result: %v", got)
	}
	files, _ := got["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("expected one file, got %v", got["files"])
	}
}
