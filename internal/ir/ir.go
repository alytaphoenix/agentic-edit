// Package ir defines the strict, typed edit operations used by the two-phase
// plan/apply interface and the MCP server.
//
// Decoding rejects unknown fields, trailing data, and fields that do not apply
// to an operation's kind. Paths are always relative to the workspace root; a
// plan can never address an absolute path or escape with "..". The model may
// produce an operation, but it cannot produce a path outside the plan's root.
package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// Kind is a discriminated operation type.
type Kind string

const (
	ReplaceLiteral Kind = "replace_literal"
	ReplaceRegex   Kind = "replace_regex"
	Insert         Kind = "insert"
	ReplaceRange   Kind = "replace_range"
	DeleteRange    Kind = "delete_range"
	ApplyPatch     Kind = "apply_patch"
	CreateFile     Kind = "create_file"
	DeleteFile     Kind = "delete_file"
	RenameFile     Kind = "rename_file"
	Chmod          Kind = "chmod"
)

// MaxOps bounds a single plan.
const MaxOps = 100

// MaxOpBytes bounds the total instruction text in a plan.
const MaxOpBytes = 1 << 20

// Op is one typed operation. Exactly the fields relevant to Kind may be set.
type Op struct {
	ID      string `json:"id,omitempty"`
	Kind    Kind   `json:"kind"`
	Path    string `json:"path,omitempty"`
	Purpose string `json:"purpose,omitempty"`

	// replace_literal / replace_regex.
	Old         string `json:"old,omitempty"`
	New         string `json:"new,omitempty"`
	Global      bool   `json:"global,omitempty"`
	ExpectCount int    `json:"expect_count,omitempty"`

	// insert.
	Anchor       string `json:"anchor,omitempty"`
	Text         string `json:"text,omitempty"`
	InsertBefore bool   `json:"insert_before,omitempty"`
	All          bool   `json:"all,omitempty"`

	// replace_range / delete_range (1-based, inclusive).
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`

	// apply_patch.
	Patch string `json:"patch,omitempty"`

	// create_file.
	Content string `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`

	// rename_file.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Document is the optional wrapper form accepted by Parse.
type Document struct {
	Version int  `json:"version,omitempty"`
	Ops     []Op `json:"ops"`
}

// Parse decodes an operation array or a {"version":1,"ops":[...]} document.
// Unknown fields and trailing JSON are rejected.
func Parse(data []byte) ([]Op, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty ops document")
	}
	var ops []Op
	if trimmed[0] == '[' {
		if err := decodeStrict(trimmed, &ops); err != nil {
			return nil, err
		}
	} else {
		var doc Document
		if err := decodeStrict(trimmed, &doc); err != nil {
			return nil, err
		}
		if doc.Version != 0 && doc.Version != 1 {
			return nil, fmt.Errorf("unsupported ops version %d", doc.Version)
		}
		ops = doc.Ops
	}
	if err := ValidateAll(ops); err != nil {
		return nil, err
	}
	return ops, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode ops: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("decode ops: trailing data after JSON value")
	}
	return nil
}

// ValidateAll validates the operation list and its aggregate limits.
func ValidateAll(ops []Op) error {
	if len(ops) == 0 {
		return fmt.Errorf("ops: at least one operation is required")
	}
	if len(ops) > MaxOps {
		return fmt.Errorf("ops: %d operations exceeds the limit of %d", len(ops), MaxOps)
	}
	total := 0
	for i := range ops {
		if err := ops[i].Validate(); err != nil {
			return fmt.Errorf("ops[%d]: %w", i, err)
		}
		total += len(ops[i].Path) + len(ops[i].From) + len(ops[i].To)
		total += len(ops[i].Old) + len(ops[i].New) + len(ops[i].Text) + len(ops[i].Content) + len(ops[i].Patch)
	}
	if total > MaxOpBytes {
		return fmt.Errorf("ops: %d bytes of edit text exceeds the limit of %d", total, MaxOpBytes)
	}
	return nil
}

// allowedFields lists the operation-specific fields valid for each kind. id and
// purpose are always allowed.
var allowedFields = map[Kind][]string{
	ReplaceLiteral: {"path", "old", "new", "global", "expect_count"},
	ReplaceRegex:   {"path", "old", "new", "global", "expect_count"},
	Insert:         {"path", "anchor", "text", "insert_before", "all"},
	ReplaceRange:   {"path", "start", "end", "text"},
	DeleteRange:    {"path", "start", "end"},
	ApplyPatch:     {"path", "patch"},
	CreateFile:     {"path", "content", "mode"},
	DeleteFile:     {"path"},
	RenameFile:     {"from", "to"},
	Chmod:          {"path", "mode"},
}

// Validate checks one operation's required and forbidden fields.
func (o Op) Validate() error {
	if o.Kind == "" {
		return fmt.Errorf("kind is required")
	}
	if err := o.checkUnused(); err != nil {
		return err
	}
	switch o.Kind {
	case ReplaceLiteral, ReplaceRegex:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if o.Old == "" {
			return fmt.Errorf("%s requires old", o.Kind)
		}
		if strings.ContainsAny(o.Old, "\r\n") || strings.ContainsAny(o.New, "\r\n") {
			return fmt.Errorf("replace values must be single-line")
		}
		if o.ExpectCount < 0 {
			return fmt.Errorf("expect_count must not be negative")
		}
		if !o.Global && o.ExpectCount == 0 {
			return fmt.Errorf("%s requires expect_count or global:true", o.Kind)
		}
		if o.Kind == ReplaceRegex {
			if _, err := regexp.Compile(o.Old); err != nil {
				return fmt.Errorf("invalid regex %q: %w", o.Old, err)
			}
		}
	case Insert:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if o.Anchor == "" {
			return fmt.Errorf("insert requires anchor")
		}
		if strings.ContainsAny(o.Anchor, "\r\n") {
			return fmt.Errorf("anchor must be single-line")
		}
		if o.Text == "" {
			return fmt.Errorf("insert requires text")
		}
	case ReplaceRange, DeleteRange:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if o.Start < 1 || o.End < o.Start {
			return fmt.Errorf("%s requires 1 <= start <= end", o.Kind)
		}
	case ApplyPatch:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if strings.TrimSpace(o.Patch) == "" {
			return fmt.Errorf("apply_patch requires a patch")
		}
	case CreateFile:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if o.Mode > 0o777 {
			return fmt.Errorf("mode %o is invalid (special bits are not allowed)", o.Mode)
		}
	case DeleteFile:
		if err := validPath(o.Path); err != nil {
			return err
		}
	case RenameFile:
		if err := validPath(o.From); err != nil {
			return fmt.Errorf("from: %w", err)
		}
		if err := validPath(o.To); err != nil {
			return fmt.Errorf("to: %w", err)
		}
		if o.From == o.To {
			return fmt.Errorf("rename: from and to are identical")
		}
	case Chmod:
		if err := validPath(o.Path); err != nil {
			return err
		}
		if o.Mode == 0 || o.Mode > 0o777 {
			return fmt.Errorf("chmod requires a mode between 0001 and 0777")
		}
	default:
		return fmt.Errorf("unknown kind %q", o.Kind)
	}
	return nil
}

// checkUnused rejects a field that has a value but is not valid for this kind.
// This keeps the IR a discriminated union instead of a bag of optional fields.
func (o Op) checkUnused() error {
	allowed := map[string]bool{"id": true, "kind": true, "purpose": true}
	for _, name := range allowedFields[o.Kind] {
		allowed[name] = true
	}
	fields := []struct {
		name string
		set  bool
	}{
		{"path", o.Path != ""}, {"old", o.Old != ""}, {"new", o.New != ""},
		{"global", o.Global}, {"expect_count", o.ExpectCount != 0},
		{"anchor", o.Anchor != ""}, {"text", o.Text != ""},
		{"insert_before", o.InsertBefore}, {"all", o.All},
		{"start", o.Start != 0}, {"end", o.End != 0},
		{"patch", o.Patch != ""}, {"content", o.Content != ""},
		{"mode", o.Mode != 0}, {"from", o.From != ""}, {"to", o.To != ""},
	}
	for _, f := range fields {
		if f.set && !allowed[f.name] {
			return fmt.Errorf("%s is not valid for %s", f.name, o.Kind)
		}
	}
	return nil
}

// validPath requires a clean, relative path with no traversal or control
// characters. Absolute paths are rejected so a model cannot choose an arbitrary
// file on the host.
func validPath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.ContainsAny(p, "\x00\r\n\\:") {
		return fmt.Errorf("path contains a control character, backslash, or drive separator")
	}
	if len(p) > 4096 {
		return fmt.Errorf("path is too long")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be relative to the root", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q escapes the root", p)
	}
	if clean != p {
		return fmt.Errorf("path %q is not clean (want %q)", p, clean)
	}
	return nil
}

// Marshal encodes ops as a stable canonical document. Struct field order is
// stable and encoding/json sorts map keys, so this is suitable for digesting.
func Marshal(ops []Op) ([]byte, error) {
	return json.Marshal(Document{Version: 1, Ops: ops})
}
