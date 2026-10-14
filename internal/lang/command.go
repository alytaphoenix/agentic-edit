package lang

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StdinPlaceholder inside a CommandChecker.Args slice is replaced with the
// stdin device path so the backing tool reads content from a pipe. Content is
// never written to disk on purpose: it may contain secrets (see scrub).
const StdinPlaceholder = "{{stdin}}"

// stdinPath is where the backing tool reads content from. "/"-prefixed so it
// resolves on macOS/Linux, which this tool targets via awk/sed/realpath.
const stdinPath = "/dev/stdin"

// DefaultTimeout bounds a single checker invocation.
var DefaultTimeout = 10 * time.Second

// CommandChecker adapts an external syntax-checker CLI to the Checker
// interface. It runs Bin with Args, feeding content on stdin, and hands the
// combined stdout/stderr to Parse. This is the recommended way to add a
// language: declare when it applies and write an error parser — no parser code.
type CommandChecker struct {
	Language string // display name, e.g. "bash"
	// Extensionless shebang-based matching. Extensions are matched
	// case-insensitively, including the leading dot.
	Extensions []string
	// Shebangs lists interpreter names/basenames. Matching is a prefix match
	// so "python" covers "python3" and "python3.11".
	Shebangs []string
	Bin      string        // default executable, e.g. "bash"
	Env      string        // optional env var that overrides Bin, e.g. "AED_BASH"
	Args     []string      // arguments; StdinPlaceholder -> the stdin device
	Timeout  time.Duration // defaults to DefaultTimeout
	Parse    func(output string) []Diagnostic
	// Unavailable reports... Optional.
	Unavailable func(output string) bool
}

func (c *CommandChecker) Name() string { return c.Language }

// shebangInterpreter returns the lowercased interpreter basename of a shebang,
// resolving `#!/usr/bin/env bash -e` to "bash". It returns "" when there is no
// shebang.
func shebangInterpreter(content string) string {
	if !strings.HasPrefix(content, "#!") {
		return ""
	}
	rest := content[2:]
	line, _, _ := strings.Cut(rest, "\n")
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	prog := filepath.Base(fields[0])
	if prog == "env" {
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				prog = filepath.Base(f)
				break
			}
		}
	}
	return strings.ToLower(prog)
}

func (c *CommandChecker) Applies(path, content string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != "" {
		for _, e := range c.Extensions {
			if strings.ToLower(e) == ext {
				return true
			}
		}
		// When a file has an extension, respect it over the shebang.
		return false
	}
	interp := shebangInterpreter(content)
	if interp == "" {
		return false
	}
	for _, s := range c.Shebangs {
		s = strings.ToLower(s)
		if interp == s || strings.HasPrefix(interp, s) {
			return true
		}
	}
	return false
}

// bin resolves the executable, honouring the Env override when set & non-blank.
func (c *CommandChecker) bin() string {
	if c.Env != "" {
		if v := strings.TrimSpace(os.Getenv(c.Env)); v != "" {
			return v
		}
	}
	return c.Bin
}

// Check runs the tool with content on stdin. A non-zero exit is parsed via
// Parse; an unset binary reports ErrUnavailable.
func (c *CommandChecker) Check(ctx context.Context, path, content string) ([]Diagnostic, error) {
	bin := c.bin()
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("%w: %s not installed (syntax not checked)", ErrUnavailable, bin)
	}

	args := make([]string, 0, len(c.Args))
	for _, a := range c.Args {
		args = append(args, strings.ReplaceAll(a, StdinPlaceholder, stdinPath))
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Stdin = strings.NewReader(content)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	if runErr == nil {
		return nil, nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		return []Diagnostic{{File: path, Message: fmt.Sprintf("%s syntax check timed out after %s", c.Language, timeout)}}, nil
	}

	out := strings.TrimSpace(buf.String())
	if c.Unavailable == nil {
		c.Unavailable = func(o string) bool {
			l := strings.ToLower(o)
			return strings.Contains(l, "no module named") || strings.Contains(l, "modulenotfounderror") || strings.Contains(l, "cannot find module")
		}
	}
	if c.Unavailable(out) {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, unavailableReason(out))
	}
	if out == "" {
		return []Diagnostic{{File: path, Message: c.Language + ": " + runErr.Error()}}, nil
	}
	diags := c.Parse(out)
	if len(diags) == 0 {
		diags = []Diagnostic{{File: path, Message: c.Language + ": " + out}}
	}
	for i := range diags {
		diags[i].File = path // report against the real file, not the pipe
	}
	return diags, nil
}

// unavailableReason returns a single human-readable line suitable for reporting
// a missing backing tool/module.
func unavailableReason(out string) string {
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		line = "checker unavailable"
	}
	return line
}

// atoiOr0 parses a decimal integer, returning 0 on failure.
func atoiOr0(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
