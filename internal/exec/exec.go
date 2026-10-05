// Package exec wraps the three core text utilities (awk, sed, grep) used by the
// agent. It provides safe dry-run and in-place execution plus line search and
// diffing helpers.
package exec

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Kind identifies the underlying utility.
const (
	KindSed  = "sed"
	KindAwk  = "awk"
	KindGrep = "grep"
)

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

// RunInPlace applies the operation to op.Path. It writes a .bak backup first and
// performs an atomic replace, rolling back on any failure.
func RunInPlace(op EditOp) error {
	if op.Kind == KindGrep || op.Path == "" {
		return nil
	}
	if err := Backup(op.Path); err != nil {
		return err
	}
	cmd, script, err := buildOpCommand(op)
	if err != nil {
		Rollback(op.Path)
		return err
	}
	defer os.Remove(script)
	out, err := cmd.Output()
	if err != nil {
		Rollback(op.Path)
		return fmt.Errorf("%s failed: %w", op.Kind, err)
	}
	tmp := op.Path + ".aed.tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		Rollback(op.Path)
		return err
	}
	if err := os.Rename(tmp, op.Path); err != nil {
		Rollback(op.Path)
		return err
	}
	return nil
}

// Backup copies path to path.bak.
func Backup(path string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak", in, 0o644)
}

// Rollback restores path from path.bak and removes any leftover temp file.
func Rollback(path string) {
	os.Remove(path + ".aed.tmp")
	if data, err := os.ReadFile(path + ".bak"); err == nil {
		_ = os.WriteFile(path, data, 0o644)
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
	cmd := exec.Command("grep", args...)
	data, _ := cmd.Output() // exit status 1 (no matches) is not an error

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
		cmd := exec.Command("grep", args...)
		data, _ := cmd.Output() // exit 1 when zero matches is fine
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

func runStdin(name, script, stdinData string) (string, error) {
	cmd := exec.Command(name, script)
	cmd.Stdin = strings.NewReader(stdinData)
	out, err := cmd.Output()
	return string(out), err
}

func grepStdin(pattern, content string) (string, error) {
	cmd := exec.Command("grep", "-n", pattern)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 means "no matches": a valid empty result, not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == -1) {
			return "", nil
		}
		return string(out), err
	}
	return string(out), nil
}

// buildOpCommand constructs the command that applies an edit op in place. Sed is
// run via a temporary -f script file so the POSIX `a\`/`i\` forms work on both
// GNU (Linux) and BSD (macOS) sed.
func buildOpCommand(op EditOp) (*exec.Cmd, string, error) {
	switch op.Kind {
	case KindSed:
		script, err := writeTemp(op.Script)
		if err != nil {
			return nil, "", err
		}
		return exec.Command("sed", "-f", script, op.Path), script, nil
	case KindAwk:
		return exec.Command("awk", op.Script, op.Path), "", nil
	default:
		return nil, "", fmt.Errorf("cannot apply %s in place", op.Kind)
	}
}

func runSedStdin(script, stdinData string) (string, error) {
	path, err := writeTemp(script)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	cmd := exec.Command("sed", "-f", path)
	cmd.Stdin = strings.NewReader(stdinData)
	out, err := cmd.Output()
	return string(out), err
}

// writeTemp writes content to a temp file and returns its path. A trailing
// newline is ensured so the final sed command line is properly terminated; BSD
// sed otherwise merges an unterminated `a\`/`i\` text line onto the next input.
func writeTemp(content string) (string, error) {
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	f, err := os.CreateTemp("", "aed-sed-*.sed")
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
