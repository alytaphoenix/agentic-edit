package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func runSession(t *testing.T, root string, allowWrite bool, calls []map[string]any) []rpc {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	for i, params := range calls {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": params})
	}
	var out bytes.Buffer
	s := &Server{Root: root, AllowWrite: allowWrite, In: &in, Out: &out}
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}
	var resps []rpc
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r rpc
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad response %q: %v", sc.Text(), err)
		}
		resps = append(resps, r)
	}
	return resps
}

func toolText(t *testing.T, r rpc) string {
	t.Helper()
	var res struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, r.Result)
	}
	if len(res.Content) == 0 {
		t.Fatalf("empty tool result: %s", r.Result)
	}
	return res.Content[0].Text
}

func TestReadAndSearch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resps := runSession(t, root, false, []map[string]any{
		{"name": "read", "arguments": map[string]any{"path": "a.txt"}},
		{"name": "search", "arguments": map[string]any{"pattern": "world"}},
	})
	if !strings.Contains(toolText(t, resps[0]), "hello world") {
		t.Fatalf("read = %s", toolText(t, resps[0]))
	}
	if !strings.Contains(toolText(t, resps[1]), "hello world") {
		t.Fatalf("search = %s", toolText(t, resps[1]))
	}
}

func TestPlanAndApplyRequireWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := []any{map[string]any{
		"kind": "replace_literal", "path": "a.txt", "old": "v1", "new": "v2", "expect_count": 1,
	}}
	planResp := runSession(t, root, false, []map[string]any{
		{"name": "plan", "arguments": map[string]any{"ops": ops}},
	})
	var plan map[string]any
	if err := json.Unmarshal([]byte(toolText(t, planResp[0])), &plan); err != nil {
		t.Fatalf("plan json: %v", err)
	}
	digest, _ := plan["digest"].(string)

	// Without --allow-write, apply is rejected and the file is untouched.
	denied := runSession(t, root, false, []map[string]any{
		{"name": "apply", "arguments": map[string]any{"plan": plan, "expect_digest": digest}},
	})
	var res struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(denied[0].Result, &res)
	if !res.IsError {
		t.Fatal("apply should be rejected without --allow-write")
	}
	if got, _ := os.ReadFile(path); string(got) != "v1\n" {
		t.Fatalf("file changed: %q", got)
	}

	applied := runSession(t, root, true, []map[string]any{
		{"name": "apply", "arguments": map[string]any{"plan": plan, "expect_digest": digest}},
	})
	if got, _ := os.ReadFile(path); string(got) != "v2\n" {
		t.Fatalf("apply result = %s; file=%q", toolText(t, applied[0]), got)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	root := t.TempDir()
	resps := runSession(t, root, false, []map[string]any{
		{"name": "read", "arguments": map[string]any{"path": "../outside"}},
	})
	var res struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(resps[0].Result, &res)
	if !res.IsError {
		t.Fatal("traversal should be rejected")
	}
}
