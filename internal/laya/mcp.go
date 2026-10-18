// Package laya is a small Model Context Protocol (MCP) client for talking to a
// running Laya instance at http://127.0.0.1:8420/mcp/. It speaks JSON-RPC 2.0
// over the streamable-http transport and exposes just enough to gather context
// and review diffs.
package laya

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxResponseBytes bounds an MCP response body. A local server that returns a
// runaway or malicious body must not exhaust aed's memory.
const maxResponseBytes = 8 << 20

// Client talks to a Laya MCP endpoint.
type Client struct {
	URL     string
	Token   string
	HTTP    *http.Client
	session string
	id      int64
	initMu  sync.Mutex
	sessMu  sync.RWMutex
	inited  bool
}

// New returns a client for the given URL and bearer token.
func New(url, token string) *Client {
	return &Client{
		URL:   url,
		Token: token,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
			// Never follow a redirect: the Authorization header must not be
			// replayed to a different endpoint chosen by the server.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Enabled reports whether the client has a URL and token for the generative
// context/review paths. A token is optional for direct decision calls; use
// Configured for those.
func (c *Client) Enabled() bool {
	return c.URL != "" && c.Token != ""
}

// Configured reports whether a URL is set, even if no token is. Local MCP
// servers often run without authentication.
func (c *Client) Configured() bool {
	return c.URL != ""
}

// Ping performs an initialize handshake and reports reachability.
func (c *Client) Ping(ctx context.Context) bool {
	if !c.Enabled() {
		return false
	}
	return c.ensureInit(ctx) == nil
}

// ensureInit performs the MCP initialize handshake once. The streamable-http
// transport expects initialize before tools/list or tools/call; the previous
// code called tools/list first when gathering context.
func (c *Client) ensureInit(ctx context.Context) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.inited {
		return nil
	}
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "aed", "version": "0.2.0"},
	})
	if err != nil {
		return err
	}
	// Best effort: the notification is required by the spec but some simple
	// servers do not require it. A failure here must not fail the handshake.
	_ = c.notify(ctx, "notifications/initialized", nil)
	c.inited = true
	return nil
}

// Tool is a single MCP tool description.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListTools returns the tools exposed by the Laya MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.ensureInit(ctx); err != nil {
		return nil, err
	}
	res, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes an MCP tool by name and returns the concatenated text
// content of the response.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := c.ensureInit(ctx); err != nil {
		return "", err
	}
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	res, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("decode tools/call: %w", err)
	}
	var sb strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// Decide asks Laya to answer a JSON-schema-shaped decision using the
// laya_decide tool. It returns the raw JSON text so callers can keep the
// per-field confidence and probabilities.
func (c *Client) Decide(ctx context.Context, state, schema map[string]any, model string) (json.RawMessage, error) {
	args := map[string]any{"state": state, "schema": schema}
	if model != "" {
		args["model"] = model
	}
	txt, err := c.CallTool(ctx, "laya_decide", args)
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(txt)) {
		return nil, fmt.Errorf("laya_decide returned non-JSON: %s", txt)
	}
	return json.RawMessage(txt), nil
}

// Predict asks Laya arbitrary typed questions using the laya_predict tool.
func (c *Client) Predict(ctx context.Context, state, questions map[string]any, model string) (json.RawMessage, error) {
	args := map[string]any{"state": state, "questions": questions}
	if model != "" {
		args["model"] = model
	}
	txt, err := c.CallTool(ctx, "laya_predict", args)
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(txt)) {
		return nil, fmt.Errorf("laya_predict returned non-JSON: %s", txt)
	}
	return json.RawMessage(txt), nil
}

// Chat finds a chat/query-style tool and asks it a question. It is a convenience
// over CallTool for generative context-gathering tools. It deliberately does not
// fall back to the first listed tool: Laya's first tool is laya_status, which
// takes no query and silently produced an empty context in the old code.
func (c *Client) Chat(ctx context.Context, query string) (string, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return "", err
	}
	name := pickTool(tools, []string{"chat", "query", "ask", "search", "coherence", "research", "semantic"})
	if name == "" {
		return "", fmt.Errorf("no suitable chat-style tool available (have: %s); use laya_decide/laya_predict for Laya", joinNames(tools))
	}
	return c.CallTool(ctx, name, map[string]any{"query": query})
}

func (c *Client) sessionID() string {
	c.sessMu.RLock()
	defer c.sessMu.RUnlock()
	return c.session
}

func (c *Client) setSession(id string) {
	if id == "" {
		return
	}
	c.sessMu.Lock()
	c.session = id
	c.sessMu.Unlock()
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if sid := c.sessionID(); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.setSession(resp.Header.Get("Mcp-Session-Id"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp %s: HTTP %d", method, resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.id, 1)
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if sid := c.sessionID(); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	c.setSession(resp.Header.Get("Mcp-Session-Id"))
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("mcp %s: response exceeds %d bytes", method, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	// Streamable HTTP servers may answer with an SSE stream even for a single
	// JSON-RPC response. Extract the response with our id before decoding.
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") ||
		(len(data) > 0 && bytes.HasPrefix(bytes.TrimSpace(data), []byte("event:"))) ||
		(len(data) > 0 && bytes.HasPrefix(bytes.TrimSpace(data), []byte("data:"))) {
		if parsed, ok := parseSSE(data, id); ok {
			data = parsed
		}
	}

	var out struct {
		Error  *jsonMessageError `json:"error"`
		Result json.RawMessage   `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body=%s)", err, string(data))
	}
	if out.Error != nil {
		return nil, fmt.Errorf("mcp %s error: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

// parseSSE extracts the JSON-RPC response whose id matches want from an SSE
// body. It returns the JSON bytes and true when a matching response is found.
func parseSSE(data []byte, want int64) ([]byte, bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var payload strings.Builder
	flush := func() ([]byte, bool) {
		raw := strings.TrimSpace(payload.String())
		payload.Reset()
		if raw == "" {
			return nil, false
		}
		var probe struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal([]byte(raw), &probe) != nil {
			return nil, false
		}
		if probe.ID != nil && *probe.ID == want {
			return []byte(raw), true
		}
		return nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if out, ok := flush(); ok {
				return out, true
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			payload.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			payload.WriteByte('\n')
		}
	}
	if out, ok := flush(); ok {
		return out, true
	}
	return nil, false
}

type jsonMessageError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func pickTool(tools []Tool, prefs []string) string {
	for _, p := range prefs {
		for _, t := range tools {
			if strings.Contains(strings.ToLower(t.Name), p) {
				return t.Name
			}
		}
	}
	return ""
}

func joinNames(tools []Tool) string {
	parts := make([]string, len(tools))
	for i, t := range tools {
		parts[i] = t.Name
	}
	return strings.Join(parts, ", ")
}
