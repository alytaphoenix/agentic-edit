// Package verify checks that an applied edit did what was intended: it greps
// the on-disk files for each operation's Assert/Deny patterns, summarizes the
// diff, and (optionally) asks Laya to review the resulting diff. The grep
// assertions are the post-apply safety guard — a failed assertion means the
// edit did not land and the caller may roll the file back.
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

// replayOp clears the preflight count so verification can replay an op that was
// already part of a sequence (the count was validated before the first write).
func replayOp(op exec.EditOp) exec.EditOp {
	op.ExpectCount = 0
	return op
}

// Run verifies res against the on-disk files. It replays the ops to skip
// no-ops (an op that changed nothing must not trigger a false assertion
// failure), then greps the actual files for each Assert (must be present) and
// Deny (must be absent). On failure the notes include an error diff. scrub
// redacts secrets from the optional Laya review prompt; nil means identity.
func Run(res *gen.Result, before, after map[string]string, client *laya.Client, useLLM bool, scrub func(string) string) Result {
	out := Result{OK: true}
	ctx := context.Background()
	if scrub == nil {
		scrub = identity
	}

	// Replay ops sequentially: state tracks the in-memory file contents so we
	// can tell whether each op actually changed a file.
	state := make(map[string]string, len(before))
	for f, c := range before {
		state[f] = c
	}

	for _, op := range res.Ops {
		if op.Kind == exec.KindGrep {
			continue
		}
		// A delete op and a regex replace carry a regex Deny; literal replace
		// and insert use literal Assert/Deny values.
		isRegexDeny := op.DenyRegex

		for _, f := range opTargets(op, before) {
			if _, err := os.Stat(f); err != nil {
				out.OK = false
				out.Notes = append(out.Notes, fmt.Sprintf("missing file after edit: %s", f))
				continue
			}

			res2, err := exec.DryRun(replayOp(op), state[f])
			if err == nil && res2 == state[f] {
				// This op changed nothing on this file → no assertion applies.
				continue
			}
			if err == nil {
				state[f] = res2
			}

			if op.Assert != "" {
				n, _ := exec.CountFixed(op.Assert, []string{f})
				if n[f] == 0 {
					out.OK = false
					out.Notes = append(out.Notes, fmt.Sprintf("ASSERT failed: %q not found in %s", op.Assert, f))
					out.Notes = append(out.Notes, assertionDetail(f, op.Assert, true)...)
					out.Notes = append(out.Notes, regionDiff(f, before, after)...)
				}
			}
			if op.Deny != "" {
				var n map[string]int
				if isRegexDeny {
					n, _ = exec.Count(op.Deny, []string{f}, true) // Deny is a regex
				} else {
					n, _ = exec.CountFixed(op.Deny, []string{f}) // literal value
				}
				if n[f] > 0 {
					out.OK = false
					out.Notes = append(out.Notes, fmt.Sprintf("DENY failed: %q still present in %s", op.Deny, f))
					out.Notes = append(out.Notes, assertionDetail(f, op.Deny, false)...)
					out.Notes = append(out.Notes, regionDiff(f, before, after)...)
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
		if txt, err := client.Chat(cctx, "Review this unified diff and say whether it correctly satisfies the intent. Diff:\n"+scrub(diff)); err == nil && txt != "" {
			out.Notes = append(out.Notes, "laya: "+txt)
		}
		cancel()
	}

	return out
}

// identity returns its input unchanged; used when scrubbing is disabled.
func identity(s string) string { return s }

// assertionDetail describes, in human-readable form, what a failed Assert/Deny
// expected versus what grep actually found on disk.
func assertionDetail(f, pattern string, mustExist bool) []string {
	var lines []string
	if mustExist {
		lines = append(lines, fmt.Sprintf("    expected %q to be present in %s", pattern, f))
	} else {
		lines = append(lines, fmt.Sprintf("    expected %q to be absent in %s", pattern, f))
	}
	matches, err := exec.SearchFixed(pattern, []string{f})
	if err != nil {
		lines = append(lines, fmt.Sprintf("    grep error: %v", err))
		return lines
	}
	if len(matches) == 0 {
		lines = append(lines, "    grep found 0 matching line(s)")
	} else {
		lines = append(lines, fmt.Sprintf("    grep found %d matching line(s):", len(matches)))
		for _, m := range matches {
			lines = append(lines, fmt.Sprintf("      %s:%d: %s", m.File, m.Num, m.Text))
		}
	}
	return lines
}

// regionDiff returns a unified diff of before→after for file f, prefixed, when
// the file actually changed.
func regionDiff(f string, before, after map[string]string) []string {
	beforeF, ok := before[f]
	if !ok {
		return nil
	}
	d := exec.LineDiff(beforeF, after[f])
	if !hasDiff(d) {
		return nil
	}
	lines := []string{"    diff (before -> after):"}
	for _, ln := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		lines = append(lines, "      "+ln)
	}
	return lines
}

func hasDiff(d string) bool {
	for _, ln := range strings.Split(d, "\n") {
		if strings.HasPrefix(ln, "-") || strings.HasPrefix(ln, "+") {
			return true
		}
	}
	return false
}
