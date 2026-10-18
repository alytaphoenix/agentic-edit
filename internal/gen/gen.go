// Package gen turns a parsed intent into concrete awk/sed/grep operations. It
// prefers a deterministic, typed grammar (intent.Parse + compile.Render) so the
// tool is fully usable with no external services. When --llm is enabled it first
// tries an LLM (Laya -> OpenAI-compatible -> Ollama), but the model may only
// emit structured, typed operations, which are validated before rendering — it
// can never inject a raw script.
package gen

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentic-edit/internal/compile"
	"agentic-edit/internal/exec"
	"agentic-edit/internal/intent"
	"agentic-edit/internal/laya"
	"agentic-edit/internal/llm"
	"agentic-edit/internal/plan"
)

// Input carries everything the generator needs.
type Input struct {
	Intent      plan.Intent
	Contents    map[string]string
	LayaContext string
	UseLLM      bool
	Force       bool
	Model       string
	OllamaURL   string
	APIKey      string
	BaseURL     string
	Client      *laya.Client
	// Scrub redacts secrets from the LLM prompt. nil means identity.
	Scrub func(string) string
}

// Result is the set of operations produced for an intent.
type Result struct {
	Ops    []exec.EditOp
	Reason string
}

// Generate produces operations for the intent. The deterministic path is used
// unless UseLLM is set and an LLM backend returns valid typed operations.
func Generate(in Input) (*Result, error) {
	if len(in.Contents) == 0 {
		return nil, fmt.Errorf("no file contents available")
	}
	if in.UseLLM {
		if ops, ok := callLLM(in); ok {
			return compileOps(ops, in.Force, "llm")
		}
	}
	ops, err := intent.Parse(in.Intent.Description, in.Intent.Files)
	if err != nil {
		return nil, err
	}
	return compileOps(ops, in.Force, "rule")
}

func compileOps(ops []intent.Op, force bool, reason string) (*Result, error) {
	out := make([]exec.EditOp, 0, len(ops))
	for _, op := range ops {
		eo, err := compile.Render(op, force)
		if err != nil {
			return nil, err
		}
		out = append(out, eo)
	}
	return &Result{Ops: out, Reason: reason}, nil
}

// callLLM asks a configured model for typed operations and validates the JSON.
// It returns (nil, false) when no backend is reachable or the reply is not a
// valid, validated list of typed ops.
func callLLM(in Input) ([]intent.Op, bool) {
	ctx := context.Background()
	if in.Client.Enabled() && in.Client.Ping(ctx) {
		if s, err := in.Client.Chat(ctx, genPrompt(in)); err == nil && s != "" {
			if ops, ok := intent.ParseOps(s); ok {
				return ops, true
			}
		}
	}
	if in.APIKey != "" {
		if s, err := llm.OpenAICompatible(ctx, in.BaseURL, in.APIKey, in.Model, genPrompt(in)); err == nil && s != "" {
			if ops, ok := intent.ParseOps(s); ok {
				return ops, true
			}
		}
	}
	if s, err := llm.Ollama(ctx, in.OllamaURL, in.Model, genPrompt(in)); err == nil && s != "" {
		if ops, ok := intent.ParseOps(s); ok {
			return ops, true
		}
	}
	return nil, false
}

func genPrompt(in Input) string {
	var b strings.Builder
	scrub := in.Scrub
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	fmt.Fprintf(&b, "Instruction: %s\n", scrub(in.Intent.Description))
	if in.LayaContext != "" {
		fmt.Fprintf(&b, "\nBackground context from Laya:\n%s\n", scrub(in.LayaContext))
	}
	b.WriteString("\nTarget file contents (first 4000 chars each):\n")
	paths := make([]string, 0, len(in.Contents))
	for path := range in.Contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		content := in.Contents[path]
		if len(content) > 4000 {
			content = content[:4000] + "\n...[truncated]..."
		}
		// Redact secrets before they leave the machine for the model.
		fmt.Fprintf(&b, "\n### %s\n%s\n", path, scrub(content))
	}
	b.WriteString("\nAny text containing the marker [REDACTED] has been redacted for security; do not attempt to match or replace it.\n")
	b.WriteString(`Respond with ONLY a JSON array (no markdown fences) of typed edit operations. Each object uses one of these shapes:
{"kind":"sed","x":"<old literal>","y":"<new>","expect_count":1}
{"kind":"sed","x":"<old regex>","y":"<new>","regex":true,"expect_count":1}
{"kind":"sed","pattern":"<regex>"}
{"kind":"sed","anchor":"<regex>","text":"<insert>","insert_before":true|false}
{"kind":"awk","transform":"upper|lower"}
{"kind":"grep","pattern":"<regex>"}
Rules: prefer literal x values and expect_count=1. Keep operations targeted; never emit rm, whole-file deletes, scope addresses, paths, or system() calls. If no edit is needed, return [].`)
	return b.String()
}
