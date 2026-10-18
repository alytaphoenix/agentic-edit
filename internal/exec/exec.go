// Package exec wraps the three core text utilities (awk, sed, grep) used by the
// agent. It provides safe dry-run and in-place execution plus line search and
// diffing helpers.
package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentic-edit/internal/tx"
)

// Kind identifies the underlying utility.
const (
	KindSed  = "sed"
	KindAwk  = "awk"
	KindGrep = "grep"
)

// DefaultTimeout bounds how long any single awk/sed/grep invocation may run.
var DefaultTimeout = 30 * time.Second

// EditOp is a single planned operation against a file.
type EditOp struct {
	Kind    string // "sed" | "awk" | "grep"
	Path    string // target file
	Script  string // sed/awk script (grep uses Pattern)
	Pattern string // grep pattern when Kind == "grep"
	Assert  string // grep pattern that must be present after apply
	Deny    string // grep pattern that must be absent after apply
	Purpose string // human-readable description

	// Action is the normalized edit action: replace, insert, delete, or
	// transform. It lets a native engine apply the op without parsing Script.
	Action string
	// Insert fields.
	Anchor       string
	Text         string
	InsertBefore bool
	FirstOnly    bool // insert only before/after the first matching line
	// Transform is the awk transform name ("upper" or "lower").
	Transform string

	// Optional preflight fields. They let the dry-run reject a replace that
	// would hit an unexpected number of occurrences before any edit runs.
	Old         string // literal or regex value being replaced
	New         string // replacement value
	Literal     bool   // Old is a literal string (default for replace)
	Global      bool   // replace every match (not just the first per line)
	Extended    bool   // regex replaces are rendered for sed -E (POSIX ERE)
	ExpectCount int    // 0 = no count check; >0 = exact number of matches
	DenyRegex   bool   // Deny is a regex (delete ops) rather than a literal
	// DeleteAllAllowed permits a delete whose pattern removes every line. The
	// compile layer sets this from --force; DryRun enforces it against the
	// actual content so a broad regex cannot bypass the gate.
	DeleteAllAllowed bool

	// FromTemplate marks an operation rendered by compile.Render through the
	// fixed template whitelist. Such an op has already been structurally
	// validated; the substring denylist is applied only to hand-built ops.
	FromTemplate bool
}

// Line is a single grep match result.
type Line struct {
	File string `json:"file"`
	Num  int    `json:"line"`
	Text string `json:"text"`
}

// DryRun runs the operation against in-memory content and returns the result
// without touching disk.
func DryRun(op EditOp, content string) (string, error) {
	if op.Kind == KindSed && op.Old != "" && op.ExpectCount > 0 {
		if err := checkExpectCount(op, content); err != nil {
			return "", err
		}
	}
	switch op.Kind {
	case KindSed:
		out, err := runSedStdin(op.Script, content, op.Extended)
		if err != nil {
			return "", err
		}
		if op.Pattern != "" && !op.DeleteAllAllowed &&
			strings.TrimRight(out, "\r\n") == "" && strings.TrimRight(content, "\r\n") != "" {
			return "", fmt.Errorf("delete pattern %q removes every line; re-run with --force to allow whole-file deletion", op.Pattern)
		}
		return out, nil
	case KindAwk:
		return runStdin(op.Kind, op.Script, content)
	case KindGrep:
		return grepStdin(op.Pattern, content)
	default:
		return "", fmt.Errorf("unknown operation kind %q", op.Kind)
	}
}

// checkExpectCount rejects an edit whose target value does not occur exactly
// ExpectCount times. This is the main guard against an agent's "replace this
// one thing" instruction silently changing every occurrence in a file.
func checkExpectCount(op EditOp, content string) error {
	var got int
	if op.Literal {
		got = strings.Count(content, op.Old)
	} else {
		re, err := regexp.Compile(op.Old)
		if err != nil {
			// Refuse rather than skip the count guard: a pattern the counter
			// cannot evaluate must not become an unbounded delete/replace.
			return fmt.Errorf("cannot evaluate regex %q for the count guard: %v", op.Old, err)
		}
		got = len(re.FindAllStringIndex(content, -1))
	}
	if got != op.ExpectCount {
		return fmt.Errorf("expected %d occurrence(s) of %q but found %d; refusing to edit", op.ExpectCount, op.Old, got)
	}
	return nil
}

// RunInPlace applies the operation to op.Path through a one-file transaction.
// The new content is computed in memory, written to a random staging file, and
// renamed into place only after its hash is verified. No sibling .bak or
// .aed.tmp path is ever used.
func RunInPlace(op EditOp) error {
	if op.Kind == KindGrep || op.Path == "" {
		return nil
	}
	before, mode, err := tx.ReadFile(op.Path)
	if err != nil {
		return err
	}
	out, err := DryRun(op, string(before))
	if err != nil {
		return fmt.Errorf("%s failed: %w", op.Kind, err)
	}
	if out == string(before) {
		return nil
	}
	t, err := tx.New(filepath.Dir(op.Path))
	if err != nil {
		return err
	}
	defer t.Cleanup()
	if err := t.Stage(op.Path, before, []byte(out), mode); err != nil {
		return err
	}
	return t.Commit()
}

// Search runs grep -Hn over files and returns parsed matches.
func Search(pattern string, files []string, extended bool) ([]Line, error) {
	args := []string{"-H", "-n"}
	if extended {
		args = append(args, "-E")
	}
	// "--" stops grep from interpreting a pattern or filename that begins
	// with "-" as an option (e.g. -f would read patterns from a file).
	args = append(args, "--", pattern)
	args = append(args, files...)
	data, err := grepOut(args...)
	if err != nil {
		return nil, err
	}
	return parseGrep(data), nil
}

// SearchFixed runs grep -HnF (fixed-string) over files and returns parsed
// matches. It is used by the post-apply guard to assert literal replacement
// values without interpreting regex metacharacters.
func SearchFixed(pattern string, files []string) ([]Line, error) {
	args := []string{"-H", "-n", "-F", "--", pattern}
	args = append(args, files...)
	data, err := grepOut(args...)
	if err != nil {
		return nil, err
	}
	return parseGrep(data), nil
}

// SearchRecursive runs grep -rHn from root. It is used by `aed grep` when no
// explicit files are given, instead of passing "." to non-recursive grep.
func SearchRecursive(pattern, root string, extended bool) ([]Line, error) {
	args := []string{"-r", "-H", "-n"}
	if extended {
		args = append(args, "-E")
	}
	args = append(args, "--", pattern, root)
	data, err := grepOut(args...)
	if err != nil {
		return nil, err
	}
	return parseGrep(data), nil
}

// CountRecursive returns per-file match counts under root.
func CountRecursive(pattern, root string, extended bool) (map[string]int, error) {
	args := []string{"-r", "-c"}
	if extended {
		args = append(args, "-E")
	}
	args = append(args, "--", pattern, root)
	data, err := grepOut(args...)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if raw == "" {
			continue
		}
		i := strings.LastIndexByte(raw, ':')
		if i < 0 {
			continue
		}
		n, _ := strconv.Atoi(raw[i+1:])
		out[raw[:i]] = n
	}
	return out, nil
}

func parseGrep(data []byte) []Line {
	var lines []Line
	for _, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if raw == "" {
			continue
		}
		f1 := strings.IndexByte(raw, ':')
		if f1 < 0 {
			continue
		}
		file := raw[:f1]
		rest := raw[f1+1:]
		f2 := strings.IndexByte(rest, ':')
		if f2 < 0 {
			continue
		}
		num, _ := strconv.Atoi(rest[:f2])
		lines = append(lines, Line{File: file, Num: num, Text: rest[f2+1:]})
	}
	return lines
}

// Count returns the number of matching lines per file.
func Count(pattern string, files []string, extended bool) (map[string]int, error) {
	out := map[string]int{}
	for _, f := range files {
		args := []string{"-c"}
		if extended {
			args = append(args, "-E")
		}
		args = append(args, "--", pattern, f)
		data, err := grepOut(args...)
		if err != nil {
			return nil, err
		}
		out[f], _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return out, nil
}

// CountFixed returns the number of fixed-string matching lines per file.
func CountFixed(pattern string, files []string) (map[string]int, error) {
	out := map[string]int{}
	for _, f := range files {
		args := []string{"-c", "-F", "--", pattern, f}
		data, err := grepOut(args...)
		if err != nil {
			return nil, err
		}
		out[f], _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return out, nil
}

// LineDiff is implemented in diff.go.

// --- command execution ---

// run invokes a command with a timeout, feeding optional stdin.
func run(name string, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- name is sed/awk/grep; args are never shell-interpreted
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	return cmd.Output()
}

// grepOut runs grep and treats "no matches" (exit status 1) as an empty result
// rather than an error.
func grepOut(args ...string) ([]byte, error) {
	out, err := run("grep", "", args...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == -1) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func runStdin(name, script, stdinData string) (string, error) {
	out, err := run(name, stdinData, script)
	return string(out), err
}

func grepStdin(pattern, content string) (string, error) {
	out, err := run("grep", content, "-n", "--", pattern)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == -1) {
			return "", nil
		}
		return string(out), err
	}
	return string(out), nil
}

func runSedStdin(script, stdinData string, extended bool) (string, error) {
	path, err := writeTemp(script)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	args := []string{}
	if extended {
		args = append(args, "-E")
	}
	args = append(args, "-f", path)
	out, err := run("sed", stdinData, args...)
	return string(out), err
}

// --- temp files ---

var (
	tempMu     sync.Mutex
	tempDirStr string
)

// tempDir returns a private, process-wide temp directory created with 0700
// permissions. Callers should defer exec.Cleanup() to remove it on exit. The
// directory is recreated if it has been removed (e.g. by a prior Cleanup call),
// so repeated invocations in the same process — such as test suites — remain
// safe.
func tempDir() (string, error) {
	tempMu.Lock()
	defer tempMu.Unlock()
	if tempDirStr == "" {
		p, err := os.MkdirTemp("", "aed-*")
		if err != nil {
			return "", err
		}
		tempDirStr = p
		return tempDirStr, nil
	}
	if info, err := os.Stat(tempDirStr); err != nil || !info.IsDir() {
		_ = os.RemoveAll(tempDirStr)
		tempDirStr = ""
		p, err := os.MkdirTemp("", "aed-*")
		if err != nil {
			return "", err
		}
		tempDirStr = p
	}
	return tempDirStr, nil
}

// Cleanup removes the process-wide temp directory.
func Cleanup() error {
	if tempDirStr != "" {
		return os.RemoveAll(tempDirStr)
	}
	return nil
}

// writeTemp writes content to a private temp file and returns its path. A
// trailing newline is ensured so the final sed command line is properly
// terminated; BSD sed otherwise merges an unterminated `a\`/`i\` text line onto
// the next input.
func writeTemp(content string) (string, error) {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	dir, err := tempDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "aed-sed-*.sed")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
