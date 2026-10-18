package laya

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeMCP struct {
	mu      sync.Mutex
	methods []string
	tools   []Tool
	sse     bool
}

func (f *fakeMCP) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.mu.Unlock()

		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": f.tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name == "laya_status" {
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": "STATUS"}}}
			} else {
				payload, _ := json.Marshal(map[string]any{
					"values":     map[string]any{"risk": "allow"},
					"confidence": map[string]any{"risk": 0.9},
				})
				result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(payload)}}}
			}
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		if f.sse && req.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

func TestDecideUsesLayaDecideAndHandshakesFirst(t *testing.T) {
	f := &fakeMCP{tools: []Tool{{Name: "laya_status"}, {Name: "laya_decide"}, {Name: "laya_predict"}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, "token")
	raw, err := c.Decide(context.Background(),
		map[string]any{"diff": "+x"},
		map[string]any{"type": "object"},
		"")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"risk"`) {
		t.Fatalf("unexpected decide result: %s", raw)
	}
	f.mu.Lock()
	methods := append([]string(nil), f.methods...)
	f.mu.Unlock()
	if len(methods) == 0 || methods[0] != "initialize" {
		t.Fatalf("expected initialize first, got %v", methods)
	}
	if !contains(methods, "tools/call") {
		t.Fatalf("expected tools/call, got %v", methods)
	}
}

func TestChatDoesNotFallBackToLayaStatus(t *testing.T) {
	f := &fakeMCP{tools: []Tool{{Name: "laya_status"}, {Name: "laya_decide"}}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, "token")
	if _, err := c.Chat(context.Background(), "hello"); err == nil {
		t.Fatal("expected an error when no chat-style tool exists")
	}
	f.mu.Lock()
	methods := append([]string(nil), f.methods...)
	f.mu.Unlock()
	for i, m := range methods {
		if m == "tools/call" {
			t.Fatalf("Chat called tools/call (method index %d): %v", i, methods)
		}
	}
}

func TestCallToolParsesSSE(t *testing.T) {
	f := &fakeMCP{tools: []Tool{{Name: "laya_decide"}}, sse: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := New(srv.URL, "token")
	txt, err := c.CallTool(context.Background(), "laya_decide", map[string]any{"state": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, `"risk"`) {
		t.Fatalf("unexpected text: %s", txt)
	}
}

func TestPickToolNoFallback(t *testing.T) {
	if got := pickTool([]Tool{{Name: "laya_status"}}, []string{"chat"}); got != "" {
		t.Fatalf("pickTool fallback = %q, want empty", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
