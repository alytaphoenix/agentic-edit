// Package mcp implements a small Model Context Protocol server over stdio.
//
// The server is read-only by default. plan/read/search/check/status are always
// available; apply/undo/recover require --allow-write. Every path is confined to
// the server's root and goes through the same policy as the CLI.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"agentic-edit/internal/applyplan"
	"agentic-edit/internal/audit"
	"agentic-edit/internal/exec"
	"agentic-edit/internal/ir"
	"agentic-edit/internal/lang"
	"agentic-edit/internal/policy"
	"agentic-edit/internal/scrub"
	"agentic-edit/internal/tx"
	"agentic-edit/internal/version"
)

// Version is the MCP server version.
const Version = version.Version

// maxMessage bounds a single JSON-RPC line.
const maxMessage = 8 << 20

// maxToolText bounds a single read/search payload so a huge file cannot flood
// the client's context.
const maxToolText = 1 << 20

// maxSearchMatches bounds a search response.
const maxSearchMatches = 2000

// Server is an MCP stdio server bound to one workspace root.
type Server struct {
	Root           string
	AllowWrite     bool
	AllowProtected bool
	AllowIgnored   bool
	ShowSecrets    bool
	In             io.Reader
	Out            io.Writer
	Err            io.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads newline-delimited JSON-RPC from In and writes responses to Out.
func (s *Server) Serve() error {
	sc := bufio.NewScanner(s.In)
	sc.Buffer(make([]byte, 64*1024), maxMessage)
	enc := json.NewEncoder(s.Out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			// Notification (e.g. notifications/initialized): no response.
			continue
		}
		resp := s.handle(req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(req request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "aed", "version": Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		result, err := s.call(req.Params)
		if err != nil {
			resp.Result = toolError(err)
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func toolList() []tool {
	pathSchema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}
	return []tool{
		{Name: "read", Description: "Read a UTF-8 file under the workspace root.", InputSchema: pathSchema},
		{Name: "search", Description: "Search files for a pattern (fixed string or regex).", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
				"regex":   map[string]any{"type": "boolean"},
			},
			"required": []string{"pattern"},
		}},
		{Name: "check", Description: "Run language syntax checkers on files.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
			"required":   []string{"paths"},
		}},
		{Name: "plan", Description: "Build a canonical, digest-bound plan from typed ops.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ops": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}},
			"required":   []string{"ops"},
		}},
		{Name: "apply", Description: "Apply a plan. Requires --allow-write and the expected digest.", InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"plan":          map[string]any{"type": "object"},
				"expect_digest": map[string]any{"type": "string"},
			},
			"required": []string{"plan", "expect_digest"},
		}},
		{Name: "undo", Description: "Undo the latest (or a named) transaction. Requires --allow-write.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"tx": map[string]any{"type": "string"}},
		}},
		{Name: "recover", Description: "Recover interrupted transactions. Requires --allow-write.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}},
		{Name: "log", Description: "Return recent audit records.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"n": map[string]any{"type": "integer"}},
		}},
		{Name: "status", Description: "Report server version, root, and write capability.", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}},
	}
}

func (s *Server) call(params json.RawMessage) (any, error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, fmt.Errorf("decode tools/call: %w", err)
	}
	switch call.Name {
	case "read":
		return s.read(call.Arguments)
	case "search":
		return s.search(call.Arguments)
	case "check":
		return s.check(call.Arguments)
	case "plan":
		return s.plan(call.Arguments)
	case "apply":
		return s.apply(call.Arguments)
	case "undo":
		return s.undo(call.Arguments)
	case "recover":
		return s.recover(call.Arguments)
	case "log":
		return s.log(call.Arguments)
	case "status":
		return s.status()
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (s *Server) read(raw json.RawMessage) (any, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	real, rel, err := policy.Confine(s.Root, filepath.Join(s.Root, filepath.FromSlash(a.Path)))
	if err != nil {
		return nil, err
	}
	if err := policy.CheckPath(rel, s.AllowProtected, s.AllowIgnored, nil); err != nil {
		return nil, err
	}
	data, _, err := tx.ReadFileLimit(real, policy.DefaultLimits().MaxFileBytes)
	if err != nil {
		return nil, err
	}
	if err := policy.CheckText(a.Path, data, policy.DefaultLimits().MaxFileBytes); err != nil {
		return nil, err
	}
	content := s.scrub(string(data))
	if len(content) > maxToolText {
		content = content[:maxToolText] + "\n...[truncated by aed]"
	}
	return textResult(map[string]any{"path": a.Path, "content": content}), nil
}

func (s *Server) search(raw json.RawMessage) (any, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Regex   bool   `json:"regex"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	var lines []exec.Line
	var err error
	if a.Path != "" {
		real, rel, cerr := policy.Confine(s.Root, filepath.Join(s.Root, filepath.FromSlash(a.Path)))
		if cerr != nil {
			return nil, cerr
		}
		if cerr := policy.CheckPath(rel, s.AllowProtected, s.AllowIgnored, nil); cerr != nil {
			return nil, cerr
		}
		lines, err = exec.Search(a.Pattern, []string{real}, a.Regex)
	} else {
		lines, err = exec.SearchRecursive(a.Pattern, s.Root, a.Regex)
	}
	if err != nil {
		return nil, err
	}
	truncated := false
	if len(lines) > maxSearchMatches {
		lines = lines[:maxSearchMatches]
		truncated = true
	}
	for i := range lines {
		lines[i].Text = s.scrub(lines[i].Text)
	}
	return textResult(map[string]any{"matches": lines, "truncated": truncated}), nil
}

func (s *Server) check(raw json.RawMessage) (any, error) {
	var a struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if len(a.Paths) == 0 {
		return nil, fmt.Errorf("paths is required")
	}
	type report struct {
		Path        string   `json:"path"`
		Lang        string   `json:"lang,omitempty"`
		Status      string   `json:"status"`
		Reason      string   `json:"reason,omitempty"`
		Diagnostics []string `json:"diagnostics,omitempty"`
	}
	var out []report
	problems := 0
	for _, p := range a.Paths {
		real, rel, err := policy.Confine(s.Root, filepath.Join(s.Root, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		if err := policy.CheckPath(rel, s.AllowProtected, s.AllowIgnored, nil); err != nil {
			return nil, err
		}
		data, _, err := tx.ReadFileLimit(real, policy.DefaultLimits().MaxFileBytes)
		if err != nil {
			return nil, err
		}
		r := lang.Check(context.Background(), p, string(data))
		rr := report{Path: p, Lang: r.Lang, Status: r.Status.String(), Reason: r.Reason}
		if r.Status == lang.Failed {
			problems++
			for _, d := range r.Diags {
				rr.Diagnostics = append(rr.Diagnostics, s.scrub(d.String()))
			}
		}
		out = append(out, rr)
	}
	return textResult(map[string]any{"ok": problems == 0, "files": out}), nil
}

func (s *Server) plan(raw json.RawMessage) (any, error) {
	var a struct {
		Ops json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	ops, err := ir.Parse(a.Ops)
	if err != nil {
		return nil, err
	}
	plan, _, err := applyplan.Build(s.Root, ops, applyplan.Options{
		AllowProtected: s.AllowProtected, AllowIgnored: s.AllowIgnored,
	})
	if err != nil {
		return nil, err
	}
	for i := range plan.Files {
		plan.Files[i].Diff = s.scrub(plan.Files[i].Diff)
	}
	return textResult(plan), nil
}

func (s *Server) apply(raw json.RawMessage) (any, error) {
	if !s.AllowWrite {
		return nil, fmt.Errorf("apply is disabled; start aed mcp with --allow-write")
	}
	var a struct {
		Plan         json.RawMessage `json:"plan"`
		ExpectDigest string          `json:"expect_digest"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	var p applyplan.Plan
	dec := json.NewDecoder(bytes.NewReader(a.Plan))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	plan, _, txID, err := applyplan.Apply(s.Root, &p, a.ExpectDigest, applyplan.Options{
		AllowProtected: s.AllowProtected, AllowIgnored: s.AllowIgnored,
	})
	if err != nil {
		return nil, err
	}
	return textResult(map[string]any{
		"applied": true, "tx_id": txID, "digest": plan.Digest, "files": plan.Files,
	}), nil
}

func (s *Server) undo(raw json.RawMessage) (any, error) {
	if !s.AllowWrite {
		return nil, fmt.Errorf("undo is disabled; start aed mcp with --allow-write")
	}
	var a struct {
		TX string `json:"tx"`
	}
	_ = json.Unmarshal(raw, &a)
	res, err := audit.Undo(s.Root, a.TX)
	if err != nil {
		return nil, err
	}
	return textResult(res), nil
}

func (s *Server) recover(raw json.RawMessage) (any, error) {
	if !s.AllowWrite {
		return nil, fmt.Errorf("recover is disabled; start aed mcp with --allow-write")
	}
	results, err := audit.Recover(s.Root)
	if err != nil {
		return nil, err
	}
	return textResult(map[string]any{"recovered": results}), nil
}

func (s *Server) log(raw json.RawMessage) (any, error) {
	var a struct {
		N int `json:"n"`
	}
	_ = json.Unmarshal(raw, &a)
	if a.N <= 0 {
		a.N = 20
	}
	recs, err := audit.Read(a.N)
	if err != nil {
		return nil, err
	}
	return textResult(map[string]any{"records": recs}), nil
}

func (s *Server) status() (any, error) {
	return textResult(map[string]any{
		"version": Version, "root": s.Root, "allow_write": s.AllowWrite,
		"allow_protected": s.AllowProtected, "allow_ignored": s.AllowIgnored,
	}), nil
}

func (s *Server) scrub(text string) string {
	if s.ShowSecrets {
		return scrub.NoOp(text)
	}
	return scrub.Redact(text)
}

func textResult(v any) map[string]any {
	data, _ := json.Marshal(v)
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(data)}},
	}
}

func toolError(err error) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
	}
}
