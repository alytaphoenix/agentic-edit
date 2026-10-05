// Package laya is a small Model Context Protocol (MCP) client for talking to a
// running Laya instance at http://127.0.0.1:8420/mcp/. It speaks JSON-RPC 2.0
// over the streamable-http transport and exposes just enough to gather context
// and review diffs.
package laya

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client talks to a Laya MCP endpoint.
type Client struct {
	URL     string
	Token   string
	HTTP    *http.Client
	session string
	id      int64
}

// New returns a client for the given URL and bearer token.
func New(url, token string) *Client {
	return &Client{
		URL:   url,
		Token: token,
		HTTP:  &http.Client{Timeout: 20 * time.Second},
	}
}

// Enabled reports whether the client has what it needs to attempt a connection.
func (c *Client) Enabled() bool {
	return c.URL != "" && c.Token != ""
}

// Ping performs an initialize handshake and reports reachability.
func (c *Client) Ping(ctx context.Context) bool {
	if !c.Enabled() {
		return false
	}
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "aed", "version": "0.1.0"},
	})
	return err == nil
}

// Tool is a single MCP tool description.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListTools returns the tools exposed by the Laya MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
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

// Chat finds a chat/query-style tool and asks it a question. It is a convenience
// over CallTool for the context-gathering use case.
func (c *Client) Chat(ctx context.Context, query string) (string, error) {
	tools, err := c.ListTools(ctx)
	if err != nil {
		return "", err
	}
	name := pickTool(tools, []string{"chat", "query", "ask", "search", "coherence", "research", "semantic"})
	if name == "" {
		return "", fmt.Errorf("no suitable chat-style tool available (have: %s)", joinNames(tools))
	}
	return c.CallTool(ctx, name, map[string]any{"query": query})
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
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	data, _ := io.ReadAll(resp.Body)

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
	if len(tools) > 0 {
		return tools[0].Name
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
