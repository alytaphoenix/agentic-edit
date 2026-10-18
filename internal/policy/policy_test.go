package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsProtected(t *testing.T) {
	yes := []string{".git/config", ".github/workflows/ci.yml", ".env", ".env.local", "Makefile", "makefile", "Dockerfile.dev", "id_rsa", "sub/.npmrc", "a.pem", ".envrc"}
	for _, p := range yes {
		if !IsProtected(p) {
			t.Errorf("%q should be protected", p)
		}
	}
	no := []string{"main.go", "docs/readme.md", "src/Makefile.go", "env.txt"}
	for _, p := range no {
		if IsProtected(p) {
			t.Errorf("%q should not be protected", p)
		}
	}
}

func TestIgnoreMatching(t *testing.T) {
	dir := t.TempDir()
	content := "# comment\n\ngenerated/\n*.min.js\n"
	if err := os.WriteFile(filepath.Join(dir, ".aedignore"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ig, err := LoadIgnore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"generated/a.txt", "assets/app.min.js"} {
		if !ig.Match(p) {
			t.Errorf("%q should be ignored", p)
		}
	}
	if ig.Match("src/main.go") {
		t.Error("src/main.go should not be ignored")
	}
	if err := CheckPath("generated/a.txt", false, false, ig); err == nil || !IsDenied(err) {
		t.Fatalf("expected an ignored-path denial, got %v", err)
	}
	if err := CheckPath("generated/a.txt", false, true, ig); err != nil {
		t.Fatalf("--allow-ignored should override: %v", err)
	}
}

func TestCheckTextRejectsBinaryAndSize(t *testing.T) {
	if err := CheckText("x", []byte("a\x00b"), 100); err == nil {
		t.Fatal("expected binary rejection")
	}
	if err := CheckText("x", []byte("abcdef"), 3); err == nil {
		t.Fatal("expected size rejection")
	}
	if err := CheckText("x", []byte("abc"), 3); err != nil {
		t.Fatal(err)
	}
}

func TestConfineRejectsEscapeAndSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Confine(root, filepath.Join(outside, "x")); err == nil {
		t.Fatal("expected an outside path to be rejected")
	}
	if _, _, err := Confine(root, filepath.Join(root, "link", "new.txt")); err == nil {
		t.Fatal("expected a symlinked parent to be rejected for a new file")
	}
	real, rel, err := Confine(root, filepath.Join(root, "ok.txt"))
	if err != nil || rel != "ok.txt" || real == "" {
		t.Fatalf("real=%q rel=%q err=%v", real, rel, err)
	}
}

func TestLimits(t *testing.T) {
	l := DefaultLimits()
	if err := l.CheckLimits(1, 10, 1, 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckLimits(l.MaxFiles+1, 0, 0, 0, 0); err == nil {
		t.Fatal("expected the file limit to trip")
	}
	if err := l.CheckLimits(1, 0, l.MaxChangedLines+1, 0, 0); err == nil {
		t.Fatal("expected the changed-line limit to trip")
	}
}
