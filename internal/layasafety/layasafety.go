// Package layasafety turns a proposed edit into a Laya decision.
//
// Laya is a fast, local, non-autoregressive decision model. The check is
// deliberately advisory by default: deterministic policy and the syntax gate
// remain authoritative, and Laya can only escalate a decision (allow -> review
// -> block), never grant permission that deterministic policy denies.
//
// The zero-shot base checkpoints are not reliable enough to be the only gate.
// This package is the integration point for a fine-tuned, calibrated edit-risk
// checkpoint; callers must choose a threshold from measured held-out data.
package layasafety

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Decider is the subset of laya.Client used here, so tests and other backends
// can provide a decision without an MCP server.
type Decider interface {
	Decide(ctx context.Context, state, schema map[string]any, model string) (json.RawMessage, error)
}

// Mode controls what the caller does with a verdict.
type Mode string

const (
	// Off disables the check.
	Off Mode = "off"
	// Warn prints the verdict but never changes the exit code.
	Warn Mode = "warn"
	// Block treats a high-confidence "block" as a policy denial and an
	// uncertain/low-confidence answer as requiring review.
	Block Mode = "block"
)

// Options configures a check.
type Options struct {
	Model         string  // Laya model/checkpoint; "" means server default (auto)
	BlockBelow    float64 // confidence below this escalates to review; default 0.65
	RequireIntent bool    // treat intent_match < 2 as review
}

// Verdict is the normalized result of one edit-risk decision.
type Verdict struct {
	Risk           string          `json:"risk"` // allow | review | block
	Confidence     float64         `json:"confidence"`
	IntentMatch    int             `json:"intent_match"`
	Destructive    bool            `json:"destructive"`
	ScopeCreep     bool            `json:"scope_creep"`
	SecretExposure bool            `json:"secret_exposure"`
	Reason         string          `json:"reason,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

// Allowed reports whether the verdict permits the edit under the given
// threshold. A low-confidence answer is never treated as allow.
func (v Verdict) Allowed(threshold float64) bool {
	if canonicalRisk(v.Risk) != "allow" {
		return false
	}
	// Missing or zero confidence means "unknown", never a confident allow.
	if v.Confidence <= 0 || v.Confidence < threshold {
		return false
	}
	return !v.Destructive && !v.ScopeCreep && !v.SecretExposure
}

// schema is deliberately small: enum/boolean/integer properties are the subset
// Laya's structured decide API projects cleanly.
func schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"risk": map[string]any{
				"type":        "string",
				"enum":        []string{"allow", "review", "block"},
				"description": "Whether the edit in `diff` is safe to apply for `instruction`.",
			},
			"intent_match": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"maximum":     3,
				"description": "How completely `diff` implements exactly `instruction`: 0 unrelated, 1 partly, 2 mostly, 3 exactly.",
			},
			"destructive": map[string]any{
				"type":        "boolean",
				"description": "Does `diff` delete or weaken data, authentication, tests, or safety controls beyond the instruction?",
			},
			"scope_creep": map[string]any{
				"type":        "boolean",
				"description": "Does `diff` change files or regions not implied by `instruction`?",
			},
			"secret_exposure": map[string]any{
				"type":        "boolean",
				"description": "Does `diff` add credentials, tokens, private keys, or exfiltration code?",
			},
		},
		"required": []string{"risk", "intent_match", "destructive", "scope_creep", "secret_exposure"},
	}
}

// Check asks Laya to classify one proposed edit. state must contain the
// instruction, the redacted diff, and deterministic flags; secrets must already
// be removed by the caller.
func Check(ctx context.Context, d Decider, state map[string]any, opts Options) (Verdict, error) {
	if d == nil {
		return Verdict{}, fmt.Errorf("laya safety: no decider")
	}
	raw, err := d.Decide(ctx, state, schema(), opts.Model)
	if err != nil {
		return Verdict{}, err
	}
	var out struct {
		Values     map[string]any `json:"values"`
		AnswerConf map[string]any `json:"answer_confidence"`
		Conf       map[string]any `json:"confidence"`
		Answers    map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Verdict{}, fmt.Errorf("decode laya decision: %w", err)
	}
	v := Verdict{Raw: raw}
	v.Risk = canonicalRisk(stringValue(out.Values["risk"]))
	v.Confidence = floatValue(out.AnswerConf["risk"])
	if v.Confidence == 0 {
		v.Confidence = floatValue(out.Conf["risk"])
	}
	v.IntentMatch = intValue(out.Values["intent_match"])
	v.Destructive = boolValue(out.Values["destructive"])
	v.ScopeCreep = boolValue(out.Values["scope_creep"])
	v.SecretExposure = boolValue(out.Values["secret_exposure"])
	switch {
	case v.Destructive:
		v.Reason = "laya classified the edit as destructive"
	case v.ScopeCreep:
		v.Reason = "laya classified the edit as scope creep"
	case v.SecretExposure:
		v.Reason = "laya classified the edit as exposing secrets"
	case opts.RequireIntent && v.IntentMatch < 2:
		v.Reason = "laya could not match the edit to the instruction"
	}
	return Normalize(v, Threshold(opts.BlockBelow), opts.RequireIntent), nil
}

// Threshold returns the effective review threshold.
func Threshold(v float64) float64 {
	if v <= 0 || v > 1 {
		return 0.65
	}
	return v
}

// Normalize turns a raw decision into the conservative policy outcome. It is
// exported so callers can apply the same rules to a stored verdict.
func Normalize(v Verdict, threshold float64, requireIntent bool) Verdict {
	// Canonicalize first: an unknown/typo'd risk value must never behave like
	// "allow" in the caller's switch. Anything unrecognized becomes review.
	v.Risk = canonicalRisk(v.Risk)
	// A missing confidence is treated as unknown (0), not as certainty. Both a
	// zero and a below-threshold confidence downgrade an allow to review.
	if (v.Confidence <= 0 || v.Confidence < threshold) && v.Risk == "allow" {
		v.Risk = "review"
		if v.Reason == "" {
			v.Reason = "laya confidence below the review threshold"
		}
	}
	if v.Destructive || v.ScopeCreep || v.SecretExposure {
		if v.Risk == "allow" {
			v.Risk = "block"
		}
	}
	if requireIntent && v.IntentMatch < 2 && v.Risk == "allow" {
		v.Risk = "review"
		if v.Reason == "" {
			v.Reason = "laya could not match the edit to the instruction"
		}
	}
	return v
}

// canonicalRisk maps a decoded risk answer onto the three policy outcomes.
// Unknown, empty, or malformed values degrade to "review" (fail closed); common
// denial synonyms degrade to "block".
func canonicalRisk(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return "allow"
	case "review", "ask", "uncertain":
		return "review"
	case "block", "blocked", "deny", "denied", "unsafe":
		return "block"
	default:
		return "review"
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		if math.IsNaN(n) {
			return 0
		}
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func floatValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0
		}
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
