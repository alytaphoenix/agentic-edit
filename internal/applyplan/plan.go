// Package applyplan builds and applies canonical, digest-bound edit plans.
//
// A plan is the agent-facing safety contract: it records the typed operations,
// the exact before/after hashes, the resulting diff, and a SHA-256 digest over
// all of it. Applying a plan requires the expected digest, re-reads every file,
// rebuilds the plan, and refuses if anything drifted. The transaction then
// writes exactly the bytes that were previewed.
package applyplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentic-edit/internal/audit"
	"agentic-edit/internal/exec"
	"agentic-edit/internal/ir"
	"agentic-edit/internal/lang"
	"agentic-edit/internal/native"
	"agentic-edit/internal/policy"
	"agentic-edit/internal/tx"
)

// Version is the plan format version.
const Version = 1

// Action identifies what happens to one file.
type Action string

const (
	ActionUpdate Action = "update"
	ActionCreate Action = "create"
	ActionDelete Action = "delete"
	ActionRename Action = "rename"
	ActionChmod  Action = "chmod"
)

// FilePlan describes one file's contribution to a plan.
type FilePlan struct {
	Path      string `json:"path"`
	Action    Action `json:"action"`
	BeforeSHA string `json:"before_sha256,omitempty"`
	AfterSHA  string `json:"after_sha256,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Added     int    `json:"added,omitempty"`
	Removed   int    `json:"removed,omitempty"`
	Diff      string `json:"diff,omitempty"`
}

// Plan is the canonical, serializable plan.
type Plan struct {
	Version     int        `json:"version"`
	Root        string     `json:"root"`
	Digest      string     `json:"digest"`
	Ops         []ir.Op    `json:"ops"`
	Files       []FilePlan `json:"files"`
	Diagnostics []string   `json:"diagnostics,omitempty"`
}

// Options controls policy during build/apply.
type Options struct {
	AllowProtected bool
	AllowIgnored   bool
	Limits         policy.Limits
	NoSyntaxCheck  bool
}

// Materialized holds the in-memory bytes corresponding to a plan.
type Materialized struct {
	Before    map[string][]byte
	After     map[string][]byte
	Modes     map[string]os.FileMode
	OrigModes map[string]os.FileMode
}

// ConflictError reports a stale or tampered plan.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// SyntaxError reports a syntax regression caught before writing.
type SyntaxError struct{ Msg string }

func (e *SyntaxError) Error() string { return e.Msg }

type fileState struct {
	path     string // slash form, relative
	real     string
	before   []byte
	after    []byte
	mode     os.FileMode
	origMode os.FileMode
	action   Action
	from     string
	to       string
	modeSet  bool
	existed  bool
}

type builder struct {
	root    string
	opts    Options
	limits  policy.Limits
	ignored *policy.Ignore
	states  map[string]*fileState
	order   []string
}

// Build validates ops and produces a plan plus the in-memory content needed to
// apply it. It reads every target through the policy layer.
func Build(root string, ops []ir.Op, opts Options) (*Plan, *Materialized, error) {
	if err := ir.ValidateAll(ops); err != nil {
		return nil, nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	realRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace root %q is not accessible: %v", root, err)
	}
	ig, err := policy.LoadIgnore(realRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("read .aedignore: %w", err)
	}
	b := &builder{
		root:    realRoot,
		opts:    opts,
		limits:  opts.Limits.Normalize(),
		ignored: ig,
		states:  map[string]*fileState{},
	}
	if err := b.build(ops); err != nil {
		return nil, nil, err
	}
	plan, mat, err := b.finish(ops)
	if err != nil {
		return nil, nil, err
	}
	return plan, mat, nil
}

func (b *builder) state(path string) (*fileState, error) {
	if s, ok := b.states[path]; ok {
		return s, nil
	}
	real, rel, err := policy.Confine(b.root, filepath.Join(b.root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	if filepath.ToSlash(rel) != path {
		return nil, fmt.Errorf("path %q does not resolve to itself under the root", path)
	}
	if err := policy.CheckPath(path, b.opts.AllowProtected, b.opts.AllowIgnored, b.ignored); err != nil {
		return nil, err
	}
	data, mode, err := tx.ReadFileLimit(real, b.limits.MaxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := policy.CheckText(path, data, b.limits.MaxFileBytes); err != nil {
		return nil, err
	}
	s := &fileState{
		path: path, real: real, before: data, after: data,
		mode: mode, origMode: mode, existed: true,
	}
	b.states[path] = s
	b.order = append(b.order, path)
	return s, nil
}

func (b *builder) build(ops []ir.Op) error {
	for _, op := range ops {
		switch op.Kind {
		case ir.CreateFile:
			if _, ok := b.states[op.Path]; ok {
				return fmt.Errorf("%s is already touched by another operation", op.Path)
			}
			real, rel, err := policy.Confine(b.root, filepath.Join(b.root, filepath.FromSlash(op.Path)))
			if err != nil {
				return err
			}
			if filepath.ToSlash(rel) != op.Path {
				return fmt.Errorf("path %q does not resolve to itself under the root", op.Path)
			}
			if err := policy.CheckPath(op.Path, b.opts.AllowProtected, b.opts.AllowIgnored, b.ignored); err != nil {
				return err
			}
			if _, err := os.Lstat(real); err == nil {
				return fmt.Errorf("%s already exists; create_file requires a new path", op.Path)
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := policy.CheckText(op.Path, []byte(op.Content), b.limits.MaxFileBytes); err != nil {
				return err
			}
			mode := os.FileMode(op.Mode)
			if mode == 0 {
				mode = 0o644
			}
			s := &fileState{
				path: op.Path, real: real, after: []byte(op.Content),
				mode: mode, origMode: mode, action: ActionCreate,
			}
			b.states[op.Path] = s
			b.order = append(b.order, op.Path)
		case ir.DeleteFile:
			s, err := b.state(op.Path)
			if err != nil {
				return err
			}
			if s.action == ActionCreate {
				return fmt.Errorf("%s: cannot delete a file created in the same plan", op.Path)
			}
			s.action = ActionDelete
			s.after = nil
		case ir.RenameFile:
			s, err := b.state(op.From)
			if err != nil {
				return err
			}
			if s.action != "" || !bytes.Equal(s.before, s.after) || s.mode != s.origMode {
				return fmt.Errorf("%s: rename cannot be combined with edits to the same file", op.From)
			}
			to := op.To
			toReal, toRel, err := policy.Confine(b.root, filepath.Join(b.root, filepath.FromSlash(to)))
			if err != nil {
				return err
			}
			if filepath.ToSlash(toRel) != to {
				return fmt.Errorf("path %q does not resolve to itself under the root", to)
			}
			if err := policy.CheckPath(to, b.opts.AllowProtected, b.opts.AllowIgnored, b.ignored); err != nil {
				return err
			}
			if _, ok := b.states[to]; ok {
				return fmt.Errorf("%s is already touched by another operation", to)
			}
			if _, err := os.Lstat(toReal); err == nil {
				return fmt.Errorf("%s already exists; refusing to overwrite it", to)
			} else if !os.IsNotExist(err) {
				return err
			}
			s.action = ActionRename
			s.from = op.From
			s.to = to
		case ir.Chmod:
			s, err := b.state(op.Path)
			if err != nil {
				return err
			}
			if s.action == ActionDelete || s.action == ActionRename {
				return fmt.Errorf("%s: chmod cannot be combined with %s", op.Path, s.action)
			}
			s.mode = os.FileMode(op.Mode)
			s.modeSet = true
		default:
			s, err := b.state(op.Path)
			if err != nil {
				return err
			}
			if s.action == ActionDelete || s.action == ActionRename {
				return fmt.Errorf("%s: cannot edit a file that is deleted or renamed in the same plan", op.Path)
			}
			out, err := native.ApplyIR(op, string(s.after))
			if err != nil {
				return fmt.Errorf("%s: %w", op.Path, err)
			}
			s.after = []byte(out)
		}
	}
	return nil
}

func (b *builder) finish(ops []ir.Op) (*Plan, *Materialized, error) {
	plan := &Plan{Version: Version, Root: b.root, Ops: ops}
	mat := &Materialized{
		Before: map[string][]byte{}, After: map[string][]byte{},
		Modes: map[string]os.FileMode{}, OrigModes: map[string]os.FileMode{},
	}

	totalBytes := int64(0)
	changedLines, deletions := 0, 0
	var diffBytes int64

	for _, name := range b.order {
		s := b.states[name]
		mat.Before[name] = s.before
		mat.After[name] = s.after
		mat.Modes[name] = s.mode
		mat.OrigModes[name] = s.origMode

		fp := FilePlan{Path: s.path, Mode: uint32(s.mode.Perm())}
		switch s.action {
		case ActionCreate:
			fp.Action = ActionCreate
			fp.AfterSHA = tx.Hash(s.after)
		case ActionDelete:
			fp.Action = ActionDelete
			fp.BeforeSHA = tx.Hash(s.before)
		case ActionRename:
			fp.Action = ActionRename
			fp.From = s.from
			fp.To = s.to
			fp.BeforeSHA = tx.Hash(s.before)
			fp.AfterSHA = tx.Hash(s.before)
		case ActionChmod:
			fp.Action = ActionChmod
			fp.BeforeSHA = tx.Hash(s.before)
			fp.AfterSHA = tx.Hash(s.after)
		default:
			if bytes.Equal(s.before, s.after) && s.mode == s.origMode {
				continue // no-op
			}
			if bytes.Equal(s.before, s.after) {
				fp.Action = ActionChmod
			} else {
				fp.Action = ActionUpdate
			}
			fp.BeforeSHA = tx.Hash(s.before)
			fp.AfterSHA = tx.Hash(s.after)
		}

		switch fp.Action {
		case ActionUpdate, ActionCreate, ActionChmod:
			oldS, newS := string(s.before), string(s.after)
			if fp.Action == ActionCreate {
				oldS = ""
			}
			d := exec.LineDiff(oldS, newS)
			added, removed := countDiff(d)
			fp.Added, fp.Removed, fp.Diff = added, removed, d
			changedLines += added + removed
			deletions += removed
			diffBytes += int64(len(d))
			mat.After[name] = s.after
		case ActionDelete:
			d := exec.LineDiff(string(s.before), "")
			fp.Removed = lineCount(string(s.before))
			fp.Diff = d
			deletions += fp.Removed
			diffBytes += int64(len(d))
			mat.After[name] = nil
		case ActionRename:
			mat.Before[name] = s.before
			mat.After[name] = nil
		}

		if err := policy.CheckText(s.path, s.after, b.limits.MaxFileBytes); err != nil {
			return nil, nil, err
		}
		totalBytes += int64(len(s.before)) + int64(len(s.after))
		plan.Files = append(plan.Files, fp)
	}
	if len(plan.Files) == 0 {
		return nil, nil, fmt.Errorf("plan makes no changes")
	}
	if err := b.limits.CheckLimits(len(plan.Files), totalBytes, changedLines, deletions, diffBytes); err != nil {
		return nil, nil, err
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Path < plan.Files[j].Path })
	plan.Diagnostics = append(plan.Diagnostics, fmt.Sprintf("%d operation(s), %d file(s)", len(ops), len(plan.Files)))
	plan.Digest = ComputeDigest(plan)
	return plan, mat, nil
}

// ComputeDigest hashes the canonical plan content (excluding the digest field).
func ComputeDigest(p *Plan) string {
	cp := digestPlan{Version: p.Version, Root: p.Root, Ops: p.Ops}
	for _, f := range p.Files {
		cp.Files = append(cp.Files, digestFile{
			Path: f.Path, Action: f.Action, BeforeSHA: f.BeforeSHA,
			AfterSHA: f.AfterSHA, Mode: f.Mode, From: f.From, To: f.To,
		})
	}
	data, _ := json.Marshal(cp)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type digestPlan struct {
	Version int          `json:"version"`
	Root    string       `json:"root"`
	Ops     []ir.Op      `json:"ops"`
	Files   []digestFile `json:"files"`
}

type digestFile struct {
	Path      string `json:"path"`
	Action    Action `json:"action"`
	BeforeSHA string `json:"before_sha256,omitempty"`
	AfterSHA  string `json:"after_sha256,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// VerifyDigest checks that p has not been modified and matches expect.
func VerifyDigest(p *Plan, expect string) error {
	expect = strings.TrimSpace(expect)
	if expect == "" {
		return fmt.Errorf("--expect-digest is required")
	}
	if !strings.HasPrefix(expect, "sha256:") {
		expect = "sha256:" + expect
	}
	if p.Digest == "" || ComputeDigest(p) != p.Digest {
		return &ConflictError{Msg: "plan digest does not match its contents; the plan was modified"}
	}
	if expect != p.Digest {
		return &ConflictError{Msg: fmt.Sprintf("expected digest %s but plan is %s", expect, p.Digest)}
	}
	return nil
}

// Apply revalidates and commits a plan.
func Apply(root string, p *Plan, expect string, opts Options) (*Plan, *Materialized, string, error) {
	if p.Version != Version {
		return nil, nil, "", fmt.Errorf("unsupported plan version %d", p.Version)
	}
	if err := VerifyDigest(p, expect); err != nil {
		return nil, nil, "", err
	}
	// Check every file against the plan's recorded hash before doing any work,
	// so a concurrent edit is reported as a conflict rather than as a failed
	// operation inside the engine.
	if err := precheck(root, p, opts); err != nil {
		return nil, nil, "", err
	}

	// Rebuild from the embedded ops against the current disk state. If anything
	// changed, the recomputed digest differs from the plan's.
	fresh, mat, err := Build(root, p.Ops, opts)
	if err != nil {
		return nil, nil, "", err
	}
	if fresh.Digest != p.Digest {
		return nil, nil, "", &ConflictError{Msg: "files changed since the plan was built; rebuild the plan"}
	}

	if !opts.NoSyntaxCheck {
		before := map[string]string{}
		after := map[string]string{}
		for _, f := range fresh.Files {
			if f.Action != ActionUpdate && f.Action != ActionCreate {
				continue
			}
			before[f.Path] = string(mat.Before[f.Path])
			after[f.Path] = string(mat.After[f.Path])
		}
		for _, r := range lang.CheckEdits(context.Background(), before, after) {
			if r.Status == lang.Failed {
				var b strings.Builder
				b.WriteString("syntax gate: the plan would leave invalid syntax")
				for _, d := range r.Diags {
					b.WriteString("\n  " + d.String())
				}
				return nil, nil, "", &SyntaxError{Msg: b.String()}
			}
		}
	}

	t, err := tx.New(fresh.Root)
	if err != nil {
		return nil, nil, "", err
	}
	defer t.Cleanup()
	entries := make([]audit.FileEntry, 0, len(fresh.Files))
	for _, f := range fresh.Files {
		path := f.Path
		if f.Action == ActionRename {
			path = f.From
		}
		entries = append(entries, audit.FileEntry{
			Path: path, Action: string(f.Action),
			BeforeSHA: f.BeforeSHA, AfterSHA: f.AfterSHA,
			Mode: uint32(mat.Modes[path].Perm()), OrigMode: uint32(mat.OrigModes[path].Perm()),
			From: f.From, To: f.To,
		})
	}
	journal, err := audit.Begin(t.ID, fresh.Root, fresh.Digest, entries, mat.Before)
	if err != nil {
		return nil, nil, "", fmt.Errorf("audit: %w", err)
	}
	for _, f := range fresh.Files {
		switch f.Action {
		case ActionUpdate:
			if err := t.Stage(f.Path, mat.Before[f.Path], mat.After[f.Path], mat.Modes[f.Path]); err != nil {
				journal.Abort()
				return nil, nil, "", err
			}
		case ActionCreate:
			if err := t.StageCreate(f.Path, mat.After[f.Path], mat.Modes[f.Path]); err != nil {
				journal.Abort()
				return nil, nil, "", err
			}
		case ActionDelete:
			if err := t.StageDelete(f.Path, mat.Before[f.Path], mat.Modes[f.Path]); err != nil {
				journal.Abort()
				return nil, nil, "", err
			}
		case ActionRename:
			if err := t.StageRename(f.From, f.To, mat.Before[f.Path], mat.Modes[f.Path]); err != nil {
				journal.Abort()
				return nil, nil, "", err
			}
		case ActionChmod:
			if err := t.StageChmod(f.Path, mat.Modes[f.Path]); err != nil {
				journal.Abort()
				return nil, nil, "", err
			}
		}
	}
	if err := t.Commit(); err != nil {
		if !t.NeedsRecovery() {
			journal.Abort()
		}
		return nil, nil, "", err
	}
	if err := journal.Commit(); err != nil {
		fresh.Diagnostics = append(fresh.Diagnostics, "warning: could not finalize audit journal: "+err.Error())
	}
	if _, err := audit.Append(audit.Record{
		Kind: "apply", TXID: t.ID, Root: fresh.Root, Tool: "aed",
		Digest: fresh.Digest, Files: entries,
	}); err != nil {
		fresh.Diagnostics = append(fresh.Diagnostics, "warning: could not append audit record: "+err.Error())
	}
	return fresh, mat, t.ID, nil
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func precheck(root string, p *Plan, opts Options) error {
	limits := opts.Limits.Normalize()
	ig, err := policy.LoadIgnore(root)
	if err != nil {
		return err
	}
	for _, f := range p.Files {
		name := f.Path
		if f.Action == ActionRename {
			name = f.From
		}
		if err := policy.CheckPath(name, opts.AllowProtected, opts.AllowIgnored, ig); err != nil {
			return err
		}
		real, _, err := policy.Confine(root, filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		if f.Action == ActionCreate {
			if _, err := os.Lstat(real); err == nil {
				return &ConflictError{Msg: fmt.Sprintf("conflict: %s appeared since the plan was built", name)}
			} else if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if f.Action == ActionRename {
			toReal, _, err := policy.Confine(root, filepath.Join(root, filepath.FromSlash(f.To)))
			if err != nil {
				return err
			}
			if _, err := os.Lstat(toReal); err == nil {
				return &ConflictError{Msg: fmt.Sprintf("conflict: %s appeared since the plan was built", f.To)}
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		data, _, err := tx.ReadFileLimit(real, limits.MaxFileBytes)
		if err != nil {
			return &ConflictError{Msg: fmt.Sprintf("conflict: %s is no longer readable: %v", name, err)}
		}
		if f.BeforeSHA != "" && tx.Hash(data) != f.BeforeSHA {
			return &ConflictError{Msg: fmt.Sprintf("conflict: %s changed since the plan was built", name)}
		}
	}
	return nil
}

func countDiff(d string) (added, removed int) {
	for _, line := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}
