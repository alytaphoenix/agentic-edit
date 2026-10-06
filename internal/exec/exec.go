// Package exec wraps the three core text utilities (awk, sed, grep) used by the
// agent. It provides safe dry-run and in-place execution plus line search and
// diffing helpers.
package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
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
}

// Line is a single grep match result.
type Line struct {
	File string
	Num  int
	Text string
}

// DryRun runs the operation against in-memory content and returns the result
// without touching disk.
func DryRun(op EditOp, content string) (string, error) {
	switch op.Kind {
	case KindSed:
		return runSedStdin(op.Script, content)
	case KindAwk:
		return runStdin(op.Kind, op.Script, content)
	case KindGrep:
		return grepStdin(op.Pattern, content)
	default:
		return "", fmt.Errorf("unknown operation kind %q", op.Kind)
	}
}

// RunInPlace applies the operation to op.Path. It writes a .bak backup first,
// preserves the original file mode, and performs an atomic replace, rolling
// back on any failure.
func RunInPlace(op EditOp) error {
	if op.Kind == KindGrep || op.Path == "" {
		return nil
	}
	info, err := os.Stat(op.Path)
	if err != nil {
		return err
	}
	if err := Backup(op.Path); err != nil {
		return err
	}
	out, script, err := applyOp(op)
	if err != nil {
		Rollback(op.Path)
		return fmt.Errorf("%s failed: %w", op.Kind, err)
	}
	if script != "" {
		defer os.Remove(script)
	}
	tmp := op.Path + ".aed.tmp"
	if err := writePreserving(tmp, out, info.Mode()); err != nil {
		Rollback(op.Path)
		return err
	}
	if err := os.Rename(tmp, op.Path); err != nil {
		Rollback(op.Path)
		return err
	}
	return nil
}

// applyOp runs a mutating op and returns its stdout plus any temp script path.
func applyOp(op EditOp) ([]byte, string, error) {
	switch op.Kind {
	case KindSed:
		script, err := writeTemp(op.Script)
		if err != nil {
			return nil, "", err
		}
		out, err := run("sed", "", "-f", script, op.Path)
		return out, script, err
	case KindAwk:
		out, err := run("awk", "", op.Script, op.Path)
		return out, "", err
	default:
		return nil, "", fmt.Errorf("cannot apply %s in place", op.Kind)
	}
}

// Backup copies path to path.bak, preserving the file mode.
func Backup(path string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode()
	}
	return writePreserving(path+".bak", in, mode)
}

// Rollback restores path from path.bak and removes any leftover temp file.
func Rollback(path string) {
	os.Remove(path + ".aed.tmp")
	if info, err := os.Stat(path + ".bak"); err == nil {
		data, rerr := os.ReadFile(path + ".bak")
		if rerr == nil {
			_ = writePreserving(path, data, info.Mode())
		}
	}
}

// Search runs grep -Hn over files and returns parsed matches.
func Search(pattern string, files []string, extended bool) ([]Line, error) {
	args := []string{"-H", "-n"}
	if extended {
		args = append(args, "-E")
	}
	args = append(args, pattern)
	args = append(args, files...)
	data, err := grepOut(args...)
	if err != nil {
		return nil, err
	}

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
	return lines, nil
}

// Count returns the number of matching lines per file.
func Count(pattern string, files []string, extended bool) (map[string]int, error) {
	out := map[string]int{}
	for _, f := range files {
		args := []string{"-c"}
		if extended {
			args = append(args, "-E")
		}
		args = append(args, pattern, f)
		data, err := grepOut(args...)
		if err != nil {
			return nil, err
		}
		out[f], _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	return out, nil
}

// LineDiff returns a simple line-level diff of old vs new using an LCS table.
// Each returned line is prefixed with ' ' (unchanged), '-' (removed) or '+'
// (added).
func LineDiff(old, new string) string {
	a := splitLines(old)
	b := splitLines(new)
	table := lcs(a, b)

	var sb strings.Builder
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			sb.WriteString("  " + a[i] + "\n")
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			sb.WriteString("-" + a[i] + "\n")
			i++
		default:
			sb.WriteString("+" + b[j] + "\n")
			j++
		}
	}
	for ; i < len(a); i++ {
		sb.WriteString("-" + a[i] + "\n")
	}
	for ; j < len(b); j++ {
		sb.WriteString("+" + b[j] + "\n")
	}
	return sb.String()
}

// splitLines splits on newlines, dropping a single trailing newline so that
// identical files produce an all-context diff (no spurious empty final line).
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func lcs(a, b []string) [][]int {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	return t
}

// --- command execution ---

// run invokes a command with a timeout, feeding optional stdin.
func run(name string, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
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
	out, err := run("grep", content, "-n", pattern)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == -1) {
			return "", nil
		}
		return string(out), err
	}
	return string(out), nil
}

func runSedStdin(script, stdinData string) (string, error) {
	path, err := writeTemp(script)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	out, err := run("sed", stdinData, "-f", path)
	return string(out), err
}

// writePreserving writes data to path using the given mode and fsyncs it before
// returning, so the mode (including executable bits) is preserved exactly.
func writePreserving(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	// OpenFile applies the umask; enforce the requested mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- temp files ---

var (
	tempOnce   sync.Once
	tempDirStr string
)

// tempDir returns a private, process-wide temp directory created with 0700
// permissions. Callers should defer exec.Cleanup() to remove it on exit.
func tempDir() (string, error) {
	tempOnce.Do(func() {
		p, err := os.MkdirTemp("", "aed-*")
		if err == nil {
			tempDirStr = p
		}
	})
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
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
