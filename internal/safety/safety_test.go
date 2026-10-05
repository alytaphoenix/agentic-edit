package safety

import (
	"testing"

	"agentic-edit/internal/exec"
)

func TestValidateAllowsNormal(t *testing.T) {
	if err := Validate(exec.EditOp{Kind: "sed", Script: "s|v1|v3|g"}); err != nil {
		t.Errorf("normal sed should pass: %v", err)
	}
	if err := Validate(exec.EditOp{Kind: "grep", Pattern: "foo"}); err != nil {
		t.Errorf("grep should always pass: %v", err)
	}
}

func TestValidateBlocksDestructive(t *testing.T) {
	bad := []string{":d", "1,$d", "system()", "rm -rf /", "> /dev/null"}
	for _, s := range bad {
		if err := Validate(exec.EditOp{Kind: "sed", Script: s}); err == nil {
			t.Errorf("expected %q to be blocked", s)
		}
	}
}
