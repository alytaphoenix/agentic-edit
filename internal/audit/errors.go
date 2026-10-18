package audit

import "fmt"

// ConflictError reports that a file no longer matches the state a transaction
// recorded, so undo/recovery must not overwrite it.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// conflictf constructs a ConflictError.
func conflictf(format string, args ...any) error {
	return &ConflictError{Msg: fmt.Sprintf(format, args...)}
}
