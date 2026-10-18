// Package plan performs the light-weight intent parsing step: it captures the
// user's natural-language instruction and the target files, and (optionally)
// enriches the plan with context gathered from Laya.
package plan

import (
	"context"
	"fmt"
	"time"

	"agentic-edit/internal/laya"
)

// Intent is the parsed user request.
type Intent struct {
	Description string
	Files       []string
}

// Parse normalizes a raw instruction and file list into an Intent.
func Parse(description string, files []string) Intent {
	return Intent{Description: description, Files: files}
}

// Enrich asks Laya for background context about the request (e.g. which ticket
// or thread relates to this change) so the generator can produce more targeted
// commands. It returns an empty string if Laya is unavailable. scrub redacts
// secrets from the description before it leaves the process; nil means identity.
func Enrich(ctx context.Context, c *laya.Client, intent Intent, scrub func(string) string) string {
	if c == nil || !c.Enabled() {
		return ""
	}
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	files := make([]string, 0, len(intent.Files))
	for _, f := range intent.Files {
		files = append(files, scrub(f))
	}
	q := fmt.Sprintf("Give me concise background for this file-edit request so I can act accurately. Description: %s\nFiles: %v\n", scrub(intent.Description), files)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if txt, err := c.Chat(ctx, q); err == nil && txt != "" {
		return txt
	}
	return ""
}
