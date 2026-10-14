// Package lang provides pluggable, per-language syntax checks for file content.
//
// The point of this package is to let the edit loop refuse to write a result
// that the language's own interpreter/compiler rejects: the check runs on the
// in-memory edited content, before anything touches disk.
//
// Adding a language means registering a Checker — there is no switch statement
// to edit. CommandChecker adapts an existing external tool (bash -n, python's
// compile(), etc.) so most languages need only a File matcher and an error
// parser.
package lang

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Diagnostic is a single syntax problem, located in the caller's file.
type Diagnostic struct {
	File    string
	Line    int // 1-based; 0 when the checker could not determine it
	Col     int // 1-based; 0 when unknown
	Message string
}

// String renders the diagnostic as file:line:col: message, omitting any
// location part that is unknown.
func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString(d.File)
	if d.Line > 0 {
		b.WriteString(":")
		b.WriteString(strconv.Itoa(d.Line))
		if d.Col > 0 {
			b.WriteString(":")
			b.WriteString(strconv.Itoa(d.Col))
		}
	}
	b.WriteString(": ")
	b.WriteString(d.Message)
	return b.String()
}

// Checker validates one language's source text.
type Checker interface {
	// Name identifies the language (e.g. "bash").
	Name() string
	// Applies reports whether this checker handles path/content. Content is
	// supplied so extensionless scripts can be matched by shebang.
	Applies(path, content string) bool
	// Check parses content and returns every diagnostic found. An empty slice
	// means the text parses. ErrUnavailable (wrapped ok) means the tool is
	// missing — callers treat this as "not checked", never as a pass.
	Check(ctx context.Context, path, content string) ([]Diagnostic, error)
}

// ErrUnavailable reports that a checker's backing tool is not installed.
var ErrUnavailable = errors.New("checker unavailable")

var (
	registryMu sync.RWMutex
	registry   []Checker
)

// Register adds c to the registry. Re-registering the same Name replaces the
// earlier entry, so a built-in can be overridden by a later registration
// (e.g. a test or a vendored variant).
func Register(c Checker) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, existing := range registry {
		if existing.Name() == c.Name() {
			registry[i] = c
			return
		}
	}
	registry = append(registry, c)
}

// Checkers returns the registered checkers sorted by name for stable output.
func Checkers() []Checker {
	registryMu.RLock()
	out := append([]Checker(nil), registry...)
	registryMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// CheckerFor returns the first checker that applies (in registry order), or nil.
func CheckerFor(path, content string) Checker {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, c := range registry {
		if c.Applies(path, content) {
			return c
		}
	}
	return nil
}

// Status is the outcome of checking one file.
type Status int

const (
	// Skipped means the file was not checked: no checker matched its type, or
	// the checker's tool is not installed.
	Skipped Status = iota
	// Passed means the content parses.
	Passed
	// Failed means the content does not parse.
	Failed
)

func (s Status) String() string {
	switch s {
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	default:
		return "skipped"
	}
}

// Report is the result of checking one file.
type Report struct {
	Path   string
	Lang   string // checker name; "" when no checker matched
	Status Status
	Diags  []Diagnostic // populated only for Failed
	Reason string       // why the file was skipped, when applicable
}

// Check runs the applicable checker over content for path. A file with no
// matching checker is Skipped (not Passed) so callers never mistake an unknown
// language for a valid one.
func Check(ctx context.Context, path, content string) Report {
	c := CheckerFor(path, content)
	if c == nil {
		return Report{Path: path, Status: Skipped, Reason: "no syntax checker for this file type"}
	}
	diags, err := c.Check(ctx, path, content)
	if errors.Is(err, ErrUnavailable) {
		return Report{Path: path, Lang: c.Name(), Status: Skipped, Reason: err.Error()}
	}
	if err != nil {
		return Report{Path: path, Lang: c.Name(), Status: Skipped, Reason: c.Name() + ": " + err.Error()}
	}
	if len(diags) == 0 {
		return Report{Path: path, Lang: c.Name(), Status: Passed}
	}
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = path
		}
	}
	return Report{Path: path, Lang: c.Name(), Status: Failed, Diags: diags}
}

// CheckEdits checks every file whose content actually changed between before
// and after. A file is reported Failed only when it parsed cleanly before the
// edit and not after: a file that was already broken stays editable, so aed can
// be used to repair it.
func CheckEdits(ctx context.Context, before, after map[string]string) []Report {
	paths := make([]string, 0, len(after))
	for f := range after {
		if before[f] == after[f] {
			continue
		}
		paths = append(paths, f)
	}
	sort.Strings(paths)

	out := make([]Report, 0, len(paths))
	for _, f := range paths {
		rep := Check(ctx, f, after[f])
		if rep.Status == Failed {
			if prev := Check(ctx, f, before[f]); prev.Status != Failed {
				rep.Status = Failed
			} else {
				rep.Status = Skipped
				rep.Reason = "already had syntax errors before the edit"
				rep.Diags = nil
			}
		}
		out = append(out, rep)
	}
	return out
}
