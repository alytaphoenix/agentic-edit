// Package verify checks that an applied edit did what was intended: it runs the
// per-operation grep asserts/denies, summarizes the diff, and (optionally) asks
// Laya to review the resulting diff.
package verify

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/gen"
	"agentic-edit/internal/laya"
)

// Result summarizes a verification pass.
type Result struct {
	OK    bool
	Notes []string
}

// opTargets returns the files an op should be checked against. Ops with an empty
// Path apply to every target file.
func opTargets(op exec.EditOp, files map[string]string) []string {
	if op.Path != "" {
		return []string{op.Path}
	}
	targets := make([]string, 0, len(files))
	for f := range files {
		targets = append(targets, f)
	}
	return targets
}

// Run verifies res against the on-disk files, comparing before/after snapshots.
func Run(res *gen.Result, before, after map[string]string, client *laya.Client, useLLM bool) Result {
	out := Result{OK: true}
	ctx := context.Background()

	for _, op := range res.Ops {
		if op.Kind == exec.KindGrep {
			continue
		}
		for _, f := range opTargets(op, before) {
			if _, err := os.Stat(f); err != nil {
				out.OK = false
				out.Notes = append(out.Notes, fmt.Sprintf("missing file after edit: %s", f))
				continue
			}
			if op.Assert != "" {
				n, _ := exec.Count(op.Assert, []string{f}, true)
				if n[f] == 0 {
					out.OK = false
					out.Notes = append(out.Notes, fmt.Sprintf("ASSERT failed: %q not found in %s", op.Assert, f))
				}
			}
			if op.Deny != "" {
				n, _ := exec.Count(op.Deny, []string{f}, true)
				if n[f] > 0 {
					out.OK = false
					out.Notes = append(out.Notes, fmt.Sprintf("DENY failed: %q still present in %s", op.Deny, f))
				}
			}
		}
	}

	added, removed := 0, 0
	for f := range before {
		d := exec.LineDiff(before[f], after[f])
		for _, line := range strings.Split(d, "\n") {
			switch {
			case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
				added++
			case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
				removed++
			}
		}
	}
	out.Notes = append(out.Notes, fmt.Sprintf("diff: +%d -%d lines", added, removed))

	if useLLM && client.Enabled() && client.Ping(ctx) {
		diff := ""
		for f := range before {
			diff += exec.LineDiff(before[f], after[f])
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if txt, err := client.Chat(cctx, "Review this unified diff and say whether it correctly satisfies the intent. Diff:\n"+diff); err == nil && txt != "" {
			out.Notes = append(out.Notes, "laya: "+txt)
		}
		cancel()
	}

	return out
}
