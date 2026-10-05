// Package safety validates planned edit operations against a small denylist of
// destructive before they are ever executed.
package safety

import (
	"fmt"
	"strings"

	"agentic-edit/internal/exec"
)

// dangerous lists substrings that, if present in an operation script, indicate a
// destructive or unsafe command. They are blocked before execution.
var dangerous = []string{
	":d",          // sed loop-delete-all
	"1,$d",        // sed delete every line
	"1,$ s",       // sed substitute across whole file unbounded
	"rm -",        // shell removal
	"> /dev/null", // discarding output
	"system(",     // awk/system call execution
	"exit(1)",     // awk abort
}

// Validate returns an error if the operation uses a blocked pattern.
func Validate(op exec.EditOp) error {
	if op.Kind == "grep" {
		return nil // grep is read-only
	}
	for _, b := range dangerous {
		if strings.Contains(op.Script, b) {
			return fmt.Errorf("blocked dangerous pattern %q in %s script", b, op.Kind)
		}
	}
	return nil
}
