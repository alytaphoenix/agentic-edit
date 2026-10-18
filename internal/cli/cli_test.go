package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aed-home-*")
	if err == nil {
		_ = os.Setenv("HOME", dir)
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

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

// fakeLaya starts an MCP server that answers laya_decide with the given risk
// and confidence, so the safety gate can be exercised end to end.
func fakeLaya(t *testing.T, risk string, confidence float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			decision := fmt.Sprintf(`{"values":{"risk":%q,"intent_match":3,"destructive":false,"scope_creep":false,"secret_exposure":false},"answer_confidence":{"risk":%v}}`, risk, confidence)
			result, _ := json.Marshal(map[string]any{
				"content": []map[string]any{{"type": "text", "text": decision}},
			})
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
		}
	}))
}

func TestLayaBlockModeDeniesReview(t *testing.T) {
	srv := fakeLaya(t, "review", 0.9)
	defer srv.Close()
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y",
		"--laya-safety=block", "--laya-url", srv.URL)
	if code != 5 {
		t.Fatalf("exit=%d, want 5; out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != `name = "v1"` {
		t.Fatalf("blocked edit changed the file: %q", got)
	}
}

func TestLayaWarnModeAppliesOnBlock(t *testing.T) {
	srv := fakeLaya(t, "block", 0.99)
	defer srv.Close()
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y",
		"--laya-safety=warn", "--laya-url", srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != `name = "v3"` {
		t.Fatalf("warn mode did not apply: %q", got)
	}
}

func TestLayaBlockModeAppliesOnConfidentAllow(t *testing.T) {
	srv := fakeLaya(t, "allow", 0.99)
	defer srv.Close()
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y",
		"--laya-safety=block", "--laya-url", srv.URL)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; out=%q", code, out)
	}
}

func TestInvalidLayaSafetyIsUsageError(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y", "--laya-safety=blok")
	if code != 2 {
		t.Fatalf("exit=%d, want 2; out=%q", code, out)
	}
}

// A symlinked --root must not hide a protected path: the relative path used for
// the policy check has to be computed against the resolved root.
func TestProtectedPathWithSymlinkedRoot(t *testing.T) {
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, ".env"), []byte("TOKEN=v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realDir, linkRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkRoot, ".env")
	var out, errb bytes.Buffer
	code := Run([]string{"edit", "--root", linkRoot, "--action", `replace "v1" with "v2"`, "-f", path, "-y"},
		strings.NewReader("n"), &out, &errb)
	if code != 5 {
		t.Fatalf("exit=%d, want 5; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}

// grep output is file content, so secrets must be redacted by default just as
// they are in edit diffs. --show-secrets opts out.
func TestGrepRedactsSecretsByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	secret := "AKIA1234567890ABCDEF"
	if err := os.WriteFile(path, []byte("key = "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"grep", "key", path}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("grep leaked a secret: %q", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED") {
		t.Fatalf("expected a redaction marker: %q", out.String())
	}

	out.Reset()
	if code := Run([]string{"grep", "--show-secrets", "key", path}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), secret) {
		t.Fatalf("--show-secrets should emit the raw value: %q", out.String())
	}
}

// The native engine must preserve BOM, CRLF, and a missing final newline.
func TestNativeEnginePreservesEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	orig := "\ufeffone v1\r\ntwo v1"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y", "--all")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	got, _ := os.ReadFile(path)
	want := "\ufeffone v3\r\ntwo v3"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSedEngineStillAvailable(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y", "--engine=sed")
	if code != 0 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); strings.TrimSpace(string(got)) != `name = "v3"` {
		t.Fatalf("result = %q", got)
	}
}

func TestInvalidEngineIsUsageError(t *testing.T) {
	dir := t.TempDir()
	path := writeLine(t, dir, `name = "v1"`)
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v3"`, "-f", path, "-y", "--engine=nope")
	if code != 2 {
		t.Fatalf("exit=%d out=%q", code, out)
	}
}

func TestAedignoreBlocksByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".aedignore"), []byte("generated/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "generated", "f.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y")
	if code != 5 {
		t.Fatalf("exit=%d, want 5; out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("ignored file was modified: %q", got)
	}
	code, out = runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y", "--allow-ignored")
	if code != 0 {
		t.Fatalf("override exit=%d out=%q", code, out)
	}
}

func runAed(args ...string) (int, string) {
	var out, errb bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &errb)
	return code, out.String() + errb.String()
}

func TestPlanApplyRoundTripAndStaleConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opsPath := filepath.Join(dir, "ops.json")
	planPath := filepath.Join(dir, "plan.json")
	ops := `[{"kind":"replace_literal","path":"a.txt","old":"v1","new":"v2","expect_count":1}]`
	if err := os.WriteFile(opsPath, []byte(ops), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runAed("plan", "--root", dir, "--ops", opsPath, "--out", planPath)
	if code != 0 {
		t.Fatalf("plan exit=%d out=%q", code, out)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan map[string]any
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	digest, _ := plan["digest"].(string)
	code, out = runAed("apply", "--root", dir, "--plan", planPath, "--expect-digest", digest, "-y")
	if code != 0 {
		t.Fatalf("apply exit=%d out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v2\n" {
		t.Fatalf("content = %q", got)
	}

	// Rebuild, then change the file: apply must report a conflict.
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runAed("plan", "--root", dir, "--ops", opsPath, "--out", planPath)
	if code != 0 {
		t.Fatalf("plan2 exit=%d out=%q", code, out)
	}
	data, _ = os.ReadFile(planPath)
	_ = json.Unmarshal(data, &plan)
	digest, _ = plan["digest"].(string)
	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = runAed("apply", "--root", dir, "--plan", planPath, "--expect-digest", digest, "-y")
	if code != 6 {
		t.Fatalf("stale apply exit=%d, want 6; out=%q", code, out)
	}
}

func TestPlanRejectsWrongDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opsPath := filepath.Join(dir, "ops.json")
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(opsPath, []byte(`[{"kind":"replace_literal","path":"a.txt","old":"v1","new":"v2","expect_count":1}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := runAed("plan", "--root", dir, "--ops", opsPath, "--out", planPath); code != 0 {
		t.Fatalf("plan exit=%d out=%q", code, out)
	}
	code, out := runAed("apply", "--root", dir, "--plan", planPath, "--expect-digest", "sha256:deadbeef", "-y")
	if code != 6 {
		t.Fatalf("exit=%d, want 6; out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestPlanProtectedPathDenied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opsPath := filepath.Join(dir, "ops.json")
	if err := os.WriteFile(opsPath, []byte(`[{"kind":"replace_literal","path":".env","old":"v1","new":"v2","expect_count":1}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runAed("plan", "--root", dir, "--ops", opsPath)
	if code != 5 {
		t.Fatalf("exit=%d, want 5; out=%q", code, out)
	}
}

func TestUndoRestoresEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runEditCmd(dir, "--action", `replace "v1" with "v2"`, "-f", path, "-y")
	if code != 0 {
		t.Fatalf("edit exit=%d out=%q", code, out)
	}
	code, out = runAed("undo", "--root", dir)
	if code != 0 {
		t.Fatalf("undo exit=%d out=%q", code, out)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("undo content = %q", got)
	}
	if code, out := runAed("log", "--verify", "-n", "10"); code != 0 {
		t.Fatalf("log verify exit=%d out=%q", code, out)
	}
}
