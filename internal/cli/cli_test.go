package cli

import (
	"bytes"
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

func TestGuardFailureExits3WithoutRestore(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `x = "b"`)
	orig := `x = "b"`

	// -y: report only, no prompt, file left edited, exit 3.
	code, out := runEditStdin(dir, "n", guardFailureArgs(dir, path)...)
	if code != 3 {
		t.Fatalf("exit=%d, want 3; out=%q", code, out)
	}
	if !strings.Contains(out, "verify[FAILED]") {
		t.Errorf("expected FAILED status, out=%q", out)
	}
	if strings.Contains(out, "restored") {
		t.Errorf("-y must not prompt to restore, out=%q", out)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimRight(string(got), "\n") != `x = "ab"` {
		t.Errorf("expected file edited to x = \"ab\" (no restore), got %q", string(got))
	}

	// Reset the file and retry applying without -y but with piped (non-TTY)
	// stdin: --dry-run=false forces the apply, the confirm reads "y", and the
	// guard still fails. With no TTY the restore prompt is skipped (report only).
	if err := os.WriteFile(path, []byte(orig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runEditStdin(dir, "y\n", "--action", `replace "b" with "ab"`, "-f", path, "--dry-run=false")
	if code != 3 {
		t.Fatalf("no-y non-interactive exit=%d, want 3; out=%q", code, out)
	}
	if strings.Contains(out, "restored") {
		t.Errorf("piped stdin must not prompt to restore, out=%q", out)
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
