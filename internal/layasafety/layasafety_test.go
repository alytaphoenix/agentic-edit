package layasafety

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeDecider struct {
	raw json.RawMessage
	err error
}

func (f fakeDecider) Decide(ctx context.Context, state, schema map[string]any, model string) (json.RawMessage, error) {
	return f.raw, f.err
}

func TestCheckNormalizesLowConfidenceAllowToReview(t *testing.T) {
	raw := json.RawMessage(`{
		"values": {"risk":"allow","intent_match":3,"destructive":false,"scope_creep":false,"secret_exposure":false},
		"answer_confidence": {"risk":0.4}
	}`)
	v, err := Check(context.Background(), fakeDecider{raw: raw}, map[string]any{"diff": "+x"}, Options{BlockBelow: 0.65})
	if err != nil {
		t.Fatal(err)
	}
	if v.Risk != "review" {
		t.Fatalf("risk = %q, want review", v.Risk)
	}
	if v.Allowed(0.65) {
		t.Fatal("low-confidence allow must not be allowed")
	}
}

func TestCheckDestructiveEscalates(t *testing.T) {
	raw := json.RawMessage(`{
		"values": {"risk":"allow","intent_match":3,"destructive":true,"scope_creep":false,"secret_exposure":false},
		"answer_confidence": {"risk":0.99}
	}`)
	v, err := Check(context.Background(), fakeDecider{raw: raw}, map[string]any{"diff": "-x"}, Options{BlockBelow: 0.65})
	if err != nil {
		t.Fatal(err)
	}
	if v.Risk != "block" {
		t.Fatalf("risk = %q, want block", v.Risk)
	}
	if v.Allowed(0.65) {
		t.Fatal("destructive edit must not be allowed")
	}
}

func TestCheckHighConfidenceAllow(t *testing.T) {
	raw := json.RawMessage(`{
		"values": {"risk":"allow","intent_match":3,"destructive":false,"scope_creep":false,"secret_exposure":false},
		"answer_confidence": {"risk":0.99}
	}`)
	v, err := Check(context.Background(), fakeDecider{raw: raw}, map[string]any{"diff": "+x"}, Options{BlockBelow: 0.65})
	if err != nil {
		t.Fatal(err)
	}
	if v.Risk != "allow" || !v.Allowed(0.65) {
		t.Fatalf("expected allow, got %+v", v)
	}
}
