// Package llm provides minimal clients for the two local/compatible LLM
// backends the generator can use: Ollama (api/generate) and any OpenAI-compatible
// /chat/completions endpoint.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client refuses redirects so an Authorization header cannot be replayed to a
// different endpoint chosen by the server.
var client = &http.Client{
	Timeout: 120 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Ollama calls a locally running Ollama server and returns the generated text.
func Ollama(ctx context.Context, url, model, prompt string) (string, error) {
	payload := map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := readLimited(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var out struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode ollama response: %w", err)
	}
	return strings.TrimSpace(out.Response), nil
}

// OpenAICompatible calls an OpenAI-compatible /chat/completions endpoint.
func OpenAICompatible(ctx context.Context, baseURL, apiKey, model, prompt string) (string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a precise text-editing assistant. Respond with only the requested output."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
	}
	if apiKey == "" {
		apiKey = "sk-none"
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := readLimited(resp)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai-compatible: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode openai response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty completion")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// maxResponseBytes bounds a model response body so a misbehaving backend cannot
// exhaust aed's memory.
const maxResponseBytes = 8 << 20

func readLimited(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("model response exceeds %d bytes", maxResponseBytes)
	}
	return data, nil
}
