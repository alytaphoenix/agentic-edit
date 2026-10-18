// Package cli wires the agent loop together: parse flags, gather context,
// generate operations, safety-check, dry-run, apply, verify, and (optionally)
// trigger Laya egress actions.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentic-edit/internal/applyplan"
	"agentic-edit/internal/audit"
	"agentic-edit/internal/exec"
	"agentic-edit/internal/gen"
	"agentic-edit/internal/ir"
	"agentic-edit/internal/lang"
	"agentic-edit/internal/laya"
	"agentic-edit/internal/layaeval"
	"agentic-edit/internal/layasafety"
	"agentic-edit/internal/mcp"
	"agentic-edit/internal/native"
	"agentic-edit/internal/plan"
	"agentic-edit/internal/policy"
	"agentic-edit/internal/safety"
	"agentic-edit/internal/scrub"
	"agentic-edit/internal/tx"
	"agentic-edit/internal/verify"
)

// Version is the CLI version reported by `aed version`.
const Version = "0.3.0"

const usageText = `aed — agentic file editing with awk/sed/grep + Laya

Usage:
  aed edit   -a "instruction" -f FILE [-f FILE...] [options]
  aed plan   --ops OPS.json [--out PLAN.json] [--json]
  aed apply  --plan PLAN.json --expect-digest DIGEST -y [--json]
  aed mcp    [--root DIR] [--allow-write]
  aed laya-eval --corpus FILE [--json]
  aed log    [--json] [-n N] [--verify]
  aed undo   [--tx ID] [--root DIR] [--json]
  aed recover [--root DIR] [--json]
  aed check  [--json] FILE [FILE...]
  aed grep   [-E] [-c] [--json] [--show-secrets] PATTERN [FILE...]
  aed list-laya-tools [--laya-url URL] [--laya-token TOKEN]
  aed version
  aed help

edit options:
  -f, --file       Target file (repeatable) [required]
  -a, --action     Natural-language instruction [required]
  --dry-run        Preview only. Default: on (preview). Use -y to apply.
  -y, --yes        Apply without confirmation (alias: --apply)
  --verify         Verify after applying (grep asserts + optional Laya review)
                   (default: on; use --no-verify to disable the post-apply guard)
  --check          Syntax-check edited files before applying (default: on)
  --no-check       Skip the pre-apply syntax check
  --unique         Require exactly one match for each replace (default)
  --all            Allow a replace to match more than once
  --keep-on-failure  Keep the edited file if verification fails (default: roll back)
  --json           Emit one machine-readable JSON result on stdout
  --llm            Use an Ollama/OpenAI-compatible model to plan the edit (opt-in)
  --no-llm         Skip LLM; use the deterministic generator only
  --laya-safety    Edit-risk review: off, warn, or block (default: off)
  --laya-safety-model  Laya checkpoint/model name (default: server auto)
  --laya-safety-threshold  Review below this confidence (default: 0.65)
  --show-secrets   Do not redact secrets from LLM prompts and output (alias: --no-scrub)
  --allow-protected  Allow editing protected paths (.git, CI, keys, .env, ...)
  --allow-ignored    Allow editing paths matched by .aedignore
  --force          Allow whole-file destructive deletes (e.g. delete matching .)
  --root           Workspace root for path confinement (default: current dir)
  --laya-url       Laya MCP URL (default http://127.0.0.1:8420/mcp/)
  --laya-token     Laya MCP bearer token (env LAYA_MCP_TOKEN)
  --model          Local LLM model name (default llama3.1)
  --ollama-url     Ollama URL (default http://localhost:11434)
  --api-key        OpenAI-compatible key (env OPENAI_API_KEY)
  --base-url       OpenAI-compatible base URL (default https://api.openai.com/v1)
  --egress         Optional Laya outbound action name (opt-in)
`

// Run is the CLI entry point; it returns a process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "edit":
		return runEdit(args[1:], stdin, stdout, stderr)
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "apply":
		return runApply(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdin, stdout, stderr)
	case "laya-eval":
		return runLayaEval(args[1:], stdout, stderr)
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "undo":
		return runUndo(args[1:], stdout, stderr)
	case "recover":
		return runRecover(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "grep":
		return runGrep(args[1:], stdout, stderr)
	case "list-laya-tools":
		return listLayaTools(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "aed %s\n", Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

func runEdit(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	var files stringList
	fs.Var(&files, "f", "target file (repeatable)")
	fs.Var(&files, "file", "target file (repeatable)")
	action := fs.String("a", "", "natural-language instruction")
	fs.StringVar(action, "action", "", "natural-language instruction")
	dry := fs.Bool("dry-run", true, "preview only")
	yes := fs.Bool("y", false, "apply without confirmation")
	fs.BoolVar(yes, "yes", false, "apply without confirmation")
	fs.BoolVar(yes, "apply", false, "apply without confirmation (alias for -y)")
	verifyOn := fs.Bool("verify", true, "verify after applying (default on)")
	noVerify := fs.Bool("no-verify", false, "do not verify after applying")
	checkOn := fs.Bool("check", true, "syntax-check edited files with their own language checker before applying")
	noCheck := fs.Bool("no-check", false, "skip the pre-apply syntax check; a syntax regression exits 4")
	llmOn := fs.Bool("llm", false, "use an LLM to plan the edit")
	noLLM := fs.Bool("no-llm", false, "skip LLM; deterministic only")
	safetyMode := fs.String("laya-safety", "off", "Laya edit-risk review: off, warn, block")
	safetyModel := fs.String("laya-safety-model", "", "Laya checkpoint/model name")
	safetyThreshold := fs.Float64("laya-safety-threshold", 0.65, "review below this confidence")
	showSecrets := fs.Bool("show-secrets", false, "do not redact secrets from LLM prompts and output")
	fs.BoolVar(showSecrets, "no-scrub", false, "alias for --show-secrets")
	force := fs.Bool("force", false, "allow whole-file destructive deletes")
	rootFlag := fs.String("root", "", "workspace root for path confinement")
	layaURL := fs.String("laya-url", "http://127.0.0.1:8420/mcp/", "Laya MCP URL")
	layaToken := fs.String("laya-token", os.Getenv("LAYA_MCP_TOKEN"), "Laya MCP token")
	model := fs.String("model", "llama3.1", "local LLM model name")
	ollamaURL := fs.String("ollama-url", "http://localhost:11434", "Ollama URL")
	apiKey := fs.String("api-key", os.Getenv("OPENAI_API_KEY"), "OpenAI-compatible key")
	baseURL := fs.String("base-url", "https://api.openai.com/v1", "OpenAI-compatible base URL")
	egress := fs.String("egress", "", "optional Laya outbound action (opt-in)")
	jsonOut := fs.Bool("json", false, "emit one JSON result on stdout")
	unique := fs.Bool("unique", true, "require exactly one match for each replace")
	all := fs.Bool("all", false, "allow a replace to match more than once")
	allowProtected := fs.Bool("allow-protected", false, "allow editing protected paths")
	allowIgnored := fs.Bool("allow-ignored", false, "allow editing paths listed in .aedignore")
	keepOnFailure := fs.Bool("keep-on-failure", false, "keep the edit if verification fails")
	engine := fs.String("engine", "native", "edit engine: native or sed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	jsonMode := *jsonOut
	humanOut := stdout
	if jsonMode {
		humanOut = stderr
	}
	res := editResult{Version: 1, DryRun: *dry}
	apply := *yes || !*dry
	res.DryRun = !apply
	defer func() {
		if !jsonMode {
			return
		}
		res.OK = code == 0
		if code != 0 && res.Error == nil {
			res.Error = &editError{Code: code, Message: exitMessage(code)}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}()

	if *action == "" {
		fmt.Fprintln(stderr, "error: --action is required")
		res.Error = &editError{Code: 2, Message: "--action is required"}
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "error: at least one -f FILE is required")
		res.Error = &editError{Code: 2, Message: "at least one -f FILE is required"}
		return 2
	}

	useLLM := *llmOn && !*noLLM
	forceDeletes := *force
	requireUnique := *unique && !*all

	// Validate the safety mode up front so a typo is a usage error, not a
	// silent no-op discovered after the edit has been previewed.
	sMode := layasafety.Mode(strings.ToLower(strings.TrimSpace(*safetyMode)))
	if sMode != layasafety.Off && sMode != layasafety.Warn && sMode != layasafety.Block {
		msg := fmt.Sprintf("invalid --laya-safety %q (want off, warn, or block)", *safetyMode)
		fmt.Fprintln(stderr, "error: "+msg)
		res.Error = &editError{Code: 2, Message: msg}
		return 2
	}
	if *engine != "native" && *engine != "sed" {
		msg := fmt.Sprintf("invalid --engine %q (want native or sed)", *engine)
		fmt.Fprintln(stderr, "error: "+msg)
		res.Error = &editError{Code: 2, Message: msg}
		return 2
	}
	if *safetyThreshold < 0 || *safetyThreshold > 1 {
		msg := fmt.Sprintf("invalid --laya-safety-threshold %v (want 0..1)", *safetyThreshold)
		fmt.Fprintln(stderr, "error: "+msg)
		res.Error = &editError{Code: 2, Message: msg}
		return 2
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	// Resolve the root once so protected-path checks compare against the same
	// real tree that confine() enforces. Using an unresolved root here let a
	// --root symlink hide a protected path behind a "../" relative name.
	limits := policy.DefaultLimits()
	ignored, err := policy.LoadIgnore(root)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot read .aedignore: %v\n", err)
		res.Error = &editError{Code: 1, Message: err.Error()}
		return 1
	}

	// scrubFn redacts secrets from LLM prompts and stdout unless --show-secrets.
	var scrubFn func(string) string
	if *showSecrets {
		scrubFn = scrub.NoOp
	} else {
		scrubFn = scrub.Redact
	}

	// Confinement + read + guards. Files are resolved to real paths inside the
	// workspace root, size-checked, and rejected if they look binary.
	confined := make([]string, 0, len(files))
	contents := map[string]string{}
	origMode := map[string]os.FileMode{}
	origHash := map[string]string{}
	for _, f := range files {
		real, rel, err := policy.Confine(root, f)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if err := policy.CheckPath(rel, *allowProtected, *allowIgnored, ignored); err != nil {
			fmt.Fprintf(stderr, "policy: %v\n", err)
			code := 1
			if policy.IsDenied(err) {
				code = 5
			}
			res.Error = &editError{Code: code, Message: err.Error()}
			return code
		}
		data, mode, err := tx.ReadFileLimit(real, limits.MaxFileBytes)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot read %s: %v\n", real, err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if err := policy.CheckText(real, data, limits.MaxFileBytes); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if _, seen := contents[real]; seen {
			continue
		}
		contents[real] = string(data)
		origMode[real] = mode
		origHash[real] = tx.Hash(data)
		confined = append(confined, real)
	}
	defer exec.Cleanup()

	intent := plan.Parse(*action, confined)

	client := laya.New(*layaURL, *layaToken)
	var layaCtx string
	if useLLM {
		layaCtx = plan.Enrich(context.Background(), client, intent, scrubFn)
		if layaCtx != "" {
			fmt.Fprintf(humanOut, "[context] %s\n", scrubFn(layaCtx))
		}
	}

	generated, err := gen.Generate(gen.Input{
		Intent:      intent,
		Contents:    contents,
		LayaContext: layaCtx,
		UseLLM:      useLLM,
		Force:       forceDeletes,
		Model:       *model,
		OllamaURL:   *ollamaURL,
		APIKey:      *apiKey,
		BaseURL:     *baseURL,
		Client:      client,
		Scrub:       scrubFn,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		res.Error = &editError{Code: 1, Message: err.Error()}
		return 1
	}
	res.Reason = generated.Reason

	for _, op := range generated.Ops {
		if err := safety.Validate(op); err != nil {
			fmt.Fprintf(stderr, "safety: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
	}

	// Default to a unique replace: an agent asking to change "one thing" must
	// not silently rewrite every occurrence. --all opts out. This applies to
	// regex replaces too, where sed would otherwise replace the first match on
	// every line.
	for i := range generated.Ops {
		op := &generated.Ops[i]
		if op.Kind == exec.KindSed && op.Old != "" && requireUnique && op.ExpectCount == 0 {
			op.ExpectCount = 1
		}
	}

	// A grep-only plan is a search, not an edit. Never feed grep output into
	// "after": doing so used to make the preview look like the file was deleted.
	mutating := 0
	for _, op := range generated.Ops {
		if op.Kind != exec.KindGrep {
			mutating++
		}
	}
	if mutating == 0 {
		matches := 0
		for _, op := range generated.Ops {
			if op.Kind != exec.KindGrep {
				continue
			}
			lines, err := exec.Search(op.Pattern, confined, false)
			if err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				res.Error = &editError{Code: 1, Message: err.Error()}
				return 1
			}
			for _, l := range lines {
				matches++
				res.Matches = append(res.Matches, matchResult{File: l.File, Line: l.Num, Text: scrubFn(l.Text)})
				fmt.Fprintf(humanOut, "%s:%d:%s\n", l.File, l.Num, scrubFn(l.Text))
			}
		}
		if matches == 0 {
			fmt.Fprintln(humanOut, "(no matches)")
		}
		return 0
	}

	after := map[string]string{}
	for f, content := range contents {
		out := content
		for _, op := range generated.Ops {
			if op.Kind == exec.KindGrep {
				continue
			}
			if op.Path != "" && op.Path != f {
				continue
			}
			var o string
			var err error
			if *engine == "native" {
				o, err = native.Apply(op, out)
			} else {
				o, err = exec.DryRun(op, out)
			}
			if err != nil {
				fmt.Fprintf(stderr, "error: %s failed: %v\n", f, err)
				res.Error = &editError{Code: 1, Message: err.Error()}
				return 1
			}
			out = o
		}
		after[f] = out
	}

	changedFiles := 0
	totalBytes := 0
	changedLines := 0
	deletions := 0
	var diffBytes int64
	for _, f := range confined {
		totalBytes += len(contents[f]) + len(after[f])
		d := exec.LineDiff(contents[f], after[f])
		changed := hasDiff(d)
		if changed {
			changedFiles++
		}
		added, removed := diffStats(d)
		changedLines += added + removed
		deletions += removed
		diffBytes += int64(len(d))
		res.Files = append(res.Files, editFile{
			Path:      f,
			BeforeSHA: origHash[f],
			AfterSHA:  tx.Hash([]byte(after[f])),
			Changed:   changed,
			Added:     added,
			Removed:   removed,
			Diff:      scrubFn(d),
		})
		if !changed {
			fmt.Fprintf(humanOut, "  (no change to %s)\n", f)
		} else {
			fmt.Fprintf(humanOut, "--- %s\n%s", f, scrubFn(d))
		}
	}

	if err := limits.CheckLimits(len(confined), int64(totalBytes), changedLines, deletions, diffBytes); err != nil {
		fmt.Fprintf(stderr, "policy: %v\n", err)
		res.Error = &editError{Code: 5, Message: err.Error()}
		return 5
	}

	// Syntax gate: parse the edited result with each language's own checker
	// before anything is written. A file only counts as a regression if it
	// parsed cleanly before the edit — files that were already broken stay
	// editable so aed can still be used to repair them.
	if *checkOn && !*noCheck {
		reports := lang.CheckEdits(context.Background(), contents, after)
		failures, passed := 0, 0
		for _, r := range reports {
			sr := syntaxResult{Path: r.Path, Lang: r.Lang, Status: r.Status.String(), Reason: r.Reason}
			for _, d := range r.Diags {
				sr.Diagnostics = append(sr.Diagnostics, scrubFn(d.String()))
			}
			res.Syntax = append(res.Syntax, sr)
			switch r.Status {
			case lang.Failed:
				failures++
				for _, d := range r.Diags {
					fmt.Fprintln(humanOut, scrubFn(d.String()))
				}
			case lang.Passed:
				passed++
			case lang.Skipped:
				if r.Lang != "" {
					fmt.Fprintf(humanOut, "syntax[skip]: %s\n", r.Reason)
				}
			}
		}
		if failures > 0 {
			msg := "the edit would leave invalid syntax; nothing was applied (--no-check to override)"
			fmt.Fprintf(humanOut, "syntax[FAILED]: %s\n", msg)
			res.Error = &editError{Code: 4, Message: msg}
			return 4
		}
		if passed > 0 {
			fmt.Fprintf(humanOut, "syntax[ok]: %d file(s) parse cleanly\n", passed)
		}
	}

	// Optional Laya edit-risk review. Deterministic gates above remain
	// authoritative; Laya can only escalate, never grant permission.
	if sMode != layasafety.Off {
		if !client.Configured() {
			msg := "laya safety requested but --laya-token/--laya-url is not configured"
			fmt.Fprintln(humanOut, "laya-safety[unavailable]: "+msg)
			res.Safety = append(res.Safety, safetyResult{Status: "unavailable", Reason: msg})
			if sMode == layasafety.Block {
				res.Error = &editError{Code: 8, Message: msg}
				return 8
			}
		} else {
			for _, f := range confined {
				if after[f] == contents[f] {
					continue
				}
				d := exec.LineDiff(contents[f], after[f])
				added, removed := diffStats(d)
				_, relForFlags, _ := policy.Confine(root, f)
				state := map[string]any{
					"instruction": scrubFn(intent.Description),
					"path":        scrubFn(filepath.ToSlash(relForFlags)),
					"diff":        scrubFn(d),
					"stats":       fmt.Sprintf("+%d -%d", added, removed),
					"flags":       fmt.Sprintf("syntax=checked; protected=%t", policy.IsProtected(relForFlags)),
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				v, err := layasafety.Check(ctx, client, state, layasafety.Options{
					Model:         *safetyModel,
					BlockBelow:    *safetyThreshold,
					RequireIntent: true,
				})
				cancel()
				sr := safetyResult{Path: f, Status: v.Risk, Confidence: v.Confidence, Reason: v.Reason}
				if err != nil {
					sr.Status = "unavailable"
					sr.Reason = err.Error()
				}
				res.Safety = append(res.Safety, sr)
				switch {
				case err != nil:
					fmt.Fprintf(humanOut, "laya-safety[unavailable] %s: %v\n", f, err)
					if sMode == layasafety.Block {
						res.Error = &editError{Code: 8, Message: err.Error()}
						return 8
					}
				case v.Risk == "block":
					msg := fmt.Sprintf("%s: %s (confidence %.2f)", f, v.Reason, v.Confidence)
					fmt.Fprintln(humanOut, "laya-safety[BLOCK]: "+msg)
					if sMode == layasafety.Block {
						res.Error = &editError{Code: 5, Message: msg}
						return 5
					}
				case v.Risk == "review":
					msg := fmt.Sprintf("%s: %s (confidence %.2f)", f, v.Reason, v.Confidence)
					fmt.Fprintln(humanOut, "laya-safety[review] "+msg)
					// In block mode an uncertain answer is not permission to
					// proceed: fail closed and require a human or the explicit
					// advisory mode.
					if sMode == layasafety.Block {
						res.Error = &editError{Code: 5, Message: "laya-safety review required: " + msg}
						return 5
					}
				default:
					fmt.Fprintf(humanOut, "laya-safety[ok] %s (confidence %.2f)\n", f, v.Confidence)
				}
			}
		}
	}

	if changedFiles == 0 {
		fmt.Fprintln(humanOut, "\n(no changes)")
		return 0
	}
	if !apply {
		fmt.Fprintln(humanOut, "\n[dry-run] preview only; no files modified. Re-run with -y to apply.")
		return 0
	}

	// Stale-edit guard: abort if any target's bytes changed since the preview.
	// The transaction repeats this check immediately before each rename.
	for _, f := range confined {
		if after[f] == contents[f] {
			continue
		}
		cur, _, err := tx.ReadFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "error: %q no longer accessible: %v\n", f, err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if tx.Hash(cur) != origHash[f] {
			msg := fmt.Sprintf("%q changed since preview; aborting", f)
			fmt.Fprintln(stderr, "conflict: "+msg)
			res.Error = &editError{Code: 6, Message: msg}
			return 6
		}
	}

	if !*yes && !confirm(stdin, humanOut) {
		fmt.Fprintln(humanOut, "aborted")
		return 0
	}

	t, err := tx.New(root)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		res.Error = &editError{Code: 6, Message: err.Error()}
		return 6
	}
	defer t.Cleanup()
	var entries []audit.FileEntry
	beforeBytes := map[string][]byte{}
	for _, f := range confined {
		if after[f] == contents[f] {
			continue
		}
		if err := t.Stage(f, []byte(contents[f]), []byte(after[f]), origMode[f]); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		rel, _ := filepath.Rel(t.Root, f)
		entries = append(entries, audit.FileEntry{
			Path: filepath.ToSlash(rel), Action: "update", BeforeSHA: origHash[f],
			AfterSHA: tx.Hash([]byte(after[f])), Mode: uint32(origMode[f].Perm()),
		})
		beforeBytes[filepath.ToSlash(rel)] = []byte(contents[f])
	}
	journal, err := audit.Begin(t.ID, t.Root, "", entries, beforeBytes)
	if err != nil {
		fmt.Fprintf(stderr, "error: audit journal: %v\n", err)
		res.Error = &editError{Code: 1, Message: "audit journal: " + err.Error()}
		return 1
	}
	if err := t.Commit(); err != nil {
		if !t.NeedsRecovery() {
			journal.Abort()
		}
		code := 1
		if strings.Contains(err.Error(), "conflict") {
			code = 6
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		res.Error = &editError{Code: code, Message: err.Error()}
		return code
	}
	if err := journal.Commit(); err != nil {
		fmt.Fprintf(stderr, "warning: could not finalize audit journal: %v\n", err)
	}
	if _, err := audit.Append(audit.Record{
		Kind: "apply", TXID: t.ID, Root: t.Root, Tool: "aed",
		Instruction: scrubFn(*action), Files: entries,
	}); err != nil {
		fmt.Fprintf(stderr, "warning: could not append audit record: %v\n", err)
	}
	res.Applied = true
	res.TXID = t.ID
	for _, op := range generated.Ops {
		if op.Kind == exec.KindGrep {
			continue
		}
		fmt.Fprintf(humanOut, "applied: %s\n", scrubFn(op.Purpose))
	}

	// Verify the exact bytes landed before running the semantic guard.
	for _, f := range confined {
		if after[f] == contents[f] {
			continue
		}
		data, _, err := tx.ReadFile(f)
		if err != nil || tx.Hash(data) != tx.Hash([]byte(after[f])) {
			msg := fmt.Sprintf("hash verification failed for %s", f)
			fmt.Fprintln(humanOut, "verify[FAILED]: "+msg)
			if *keepOnFailure {
				fmt.Fprintln(humanOut, "keeping edited files (--keep-on-failure)")
			} else if rbErr := t.Rollback(); rbErr != nil {
				fmt.Fprintf(stderr, "rollback failed: %v\n", rbErr)
			} else {
				res.Applied = false
				journal.Abort()
				_, _ = audit.Append(audit.Record{Kind: "rollback", TXID: t.ID, Root: t.Root, Tool: "aed", Files: entries, Message: msg})
				fmt.Fprintln(humanOut, "restored original files")
			}
			res.Error = &editError{Code: 3, Message: msg}
			return 3
		}
	}

	if *verifyOn && !*noVerify {
		vr := verify.Run(generated, contents, after, client, useLLM, scrubFn)
		res.Verify = vr.Notes
		if vr.OK {
			fmt.Fprintf(humanOut, "verify[ok]: %s\n", strings.Join(vr.Notes, " | "))
		} else {
			msg := "the edit did not land as expected"
			fmt.Fprintln(humanOut, "verify[FAILED]: "+msg)
			for _, n := range vr.Notes {
				fmt.Fprintf(humanOut, "  %s\n", n)
			}
			if *keepOnFailure {
				fmt.Fprintln(humanOut, "keeping edited files (--keep-on-failure)")
			} else if rbErr := t.Rollback(); rbErr != nil {
				fmt.Fprintf(stderr, "rollback failed: %v\n", rbErr)
			} else {
				res.Applied = false
				journal.Abort()
				_, _ = audit.Append(audit.Record{Kind: "rollback", TXID: t.ID, Root: t.Root, Tool: "aed", Files: entries, Message: msg})
				fmt.Fprintln(humanOut, "restored original files")
			}
			res.Error = &editError{Code: 3, Message: msg}
			return 3
		}
	}

	if *egress != "" && useLLM && client.Enabled() && client.Ping(context.Background()) {
		if txt, err := client.CallTool(context.Background(), *egress, map[string]any{"description": scrubFn(intent.Description)}); err != nil {
			fmt.Fprintf(stderr, "egress skipped: %v\n", err)
		} else {
			fmt.Fprintf(humanOut, "egress[%s]: %s\n", *egress, scrubFn(txt))
		}
	}
	return 0
}

func runGrep(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grep", flag.ContinueOnError)
	ext := fs.Bool("E", false, "extended regex")
	count := fs.Bool("c", false, "count matches")
	jsonOut := fs.Bool("json", false, "emit JSON")
	showSecrets := fs.Bool("show-secrets", false, "do not redact secrets from matches")
	fs.BoolVar(showSecrets, "no-scrub", false, "alias for --show-secrets")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: aed grep [-E] [-c] PATTERN [FILE...]")
		return 2
	}
	pattern := rest[0]
	files := rest[1:]
	recursive := len(files) == 0
	scrubFn := scrub.Redact
	if *showSecrets {
		scrubFn = scrub.NoOp
	}

	if *count {
		var n map[string]int
		var err error
		if recursive {
			n, err = exec.CountRecursive(pattern, ".", *ext)
		} else {
			n, err = exec.Count(pattern, files, *ext)
		}
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if *jsonOut {
			_ = json.NewEncoder(stdout).Encode(map[string]any{"counts": n})
			return 0
		}
		for f, c := range n {
			fmt.Fprintf(stdout, "%d\t%s\n", c, f)
		}
		return 0
	}

	var lines []exec.Line
	var err error
	if recursive {
		lines, err = exec.SearchRecursive(pattern, ".", *ext)
	} else {
		lines, err = exec.Search(pattern, files, *ext)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for i := range lines {
		lines[i].Text = scrubFn(lines[i].Text)
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"matches": lines})
		return 0
	}
	for _, l := range lines {
		fmt.Fprintf(stdout, "%s:%d:%s\n", l.File, l.Num, l.Text)
	}
	return 0
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(stderr, "usage: aed check [-json] FILE...")
		return 2
	}
	defer exec.Cleanup()

	type checkResult struct {
		Path        string   `json:"path"`
		Lang        string   `json:"lang,omitempty"`
		Status      string   `json:"status"`
		Reason      string   `json:"reason,omitempty"`
		Diagnostics []string `json:"diagnostics,omitempty"`
	}
	var results []checkResult

	problems := 0
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		data, _, err := tx.ReadFileLimit(abs, policy.DefaultLimits().MaxFileBytes)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot read %s: %v\n", p, err)
			return 1
		}
		if err := policy.CheckText(abs, data, policy.DefaultLimits().MaxFileBytes); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		rep := lang.Check(context.Background(), abs, string(data))
		cr := checkResult{Path: p, Lang: rep.Lang, Status: rep.Status.String(), Reason: rep.Reason}
		switch rep.Status {
		case lang.Failed:
			problems++
			for _, d := range rep.Diags {
				cr.Diagnostics = append(cr.Diagnostics, scrub.Redact(d.String()))
			}
		case lang.Passed:
			cr.Status = "passed"
		case lang.Skipped:
			cr.Status = "skipped"
		}
		results = append(results, cr)
		if !*jsonOut {
			switch rep.Status {
			case lang.Failed:
				for _, d := range rep.Diags {
					fmt.Fprintln(stdout, scrub.Redact(d.String()))
				}
			case lang.Passed:
				fmt.Fprintf(stdout, "%s: ok (%s)\n", p, rep.Lang)
			case lang.Skipped:
				if rep.Lang != "" {
					fmt.Fprintf(stdout, "%s: skipped (%s)\n", p, rep.Reason)
				} else {
					fmt.Fprintf(stdout, "%s: skipped (no checker for this file type)\n", p)
				}
			}
		}
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"ok": problems == 0, "files": results})
	}
	if problems > 0 {
		return 4
	}
	return 0
}

func listLayaTools(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list-laya-tools", flag.ContinueOnError)
	url := fs.String("laya-url", "http://127.0.0.1:8420/mcp/", "Laya MCP URL")
	token := fs.String("laya-token", os.Getenv("LAYA_MCP_TOKEN"), "Laya MCP token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c := laya.New(*url, *token)
	if !c.Configured() {
		fmt.Fprintln(stderr, "error: --laya-url is required")
		return 2
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, t := range tools {
		fmt.Fprintf(stdout, "%-24s %s\n", t.Name, t.Description)
	}
	return 0
}

// editResult is the stable machine-readable result emitted by --json.
type editResult struct {
	Version int            `json:"version"`
	OK      bool           `json:"ok"`
	Applied bool           `json:"applied"`
	DryRun  bool           `json:"dry_run"`
	TXID    string         `json:"tx_id,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Files   []editFile     `json:"files,omitempty"`
	Matches []matchResult  `json:"matches,omitempty"`
	Syntax  []syntaxResult `json:"syntax,omitempty"`
	Safety  []safetyResult `json:"safety,omitempty"`
	Verify  []string       `json:"verify,omitempty"`
	Error   *editError     `json:"error,omitempty"`
}

type editFile struct {
	Path      string `json:"path"`
	BeforeSHA string `json:"before_sha256"`
	AfterSHA  string `json:"after_sha256"`
	Changed   bool   `json:"changed"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Diff      string `json:"diff,omitempty"`
}

type matchResult struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type syntaxResult struct {
	Path        string   `json:"path"`
	Lang        string   `json:"lang,omitempty"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}

type safetyResult struct {
	Path       string  `json:"path,omitempty"`
	Status     string  `json:"status"`
	Confidence float64 `json:"confidence,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

type editError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// hasDiff reports whether a LineDiff output contains any removed/added lines.
func hasDiff(d string) bool {
	for _, line := range strings.Split(d, "\n") {
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
			return true
		}
	}
	return false
}

// diffStats counts added and removed lines in an aed LineDiff.
func diffStats(d string) (added, removed int) {
	for _, line := range strings.Split(d, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

func exitMessage(code int) string {
	switch code {
	case 1:
		return "runtime error"
	case 2:
		return "usage error"
	case 3:
		return "verification failed"
	case 4:
		return "syntax gate failed"
	case 5:
		return "policy denied"
	case 6:
		return "conflict or lock failure"
	case 8:
		return "safety checker unavailable"
	default:
		return "edit failed"
	}
}

func confirm(r io.Reader, w io.Writer) bool {
	fmt.Fprint(w, "Apply these changes? [y/N] ")
	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// runPlan builds a canonical, digest-bound plan from an ops JSON document.
func runPlan(args []string, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	opsPath := fs.String("ops", "", "ops JSON file")
	outPath := fs.String("out", "", "write the plan JSON to this file")
	rootFlag := fs.String("root", "", "workspace root")
	jsonOut := fs.Bool("json", false, "emit the plan JSON on stdout")
	allowProtected := fs.Bool("allow-protected", false, "allow protected paths")
	allowIgnored := fs.Bool("allow-ignored", false, "allow .aedignore paths")
	showSecrets := fs.Bool("show-secrets", false, "do not redact secrets from the rendered diff")
	fs.BoolVar(showSecrets, "no-scrub", false, "alias for --show-secrets")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *opsPath == "" {
		fmt.Fprintln(stderr, "usage: aed plan --ops OPS.json [--out PLAN.json] [--json]")
		return 2
	}
	if *jsonOut {
		defer func() {
			if code == 0 {
				return
			}
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"version": 1, "ok": false,
				"error": map[string]any{"code": code, "message": exitMessage(code)},
			})
		}()
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	data, err := readSmallFile(*opsPath, 2<<20)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	ops, err := ir.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	plan, _, err := applyplan.Build(root, ops, applyplan.Options{
		AllowProtected: *allowProtected,
		AllowIgnored:   *allowIgnored,
	})
	if err != nil {
		return reportPlanError(err, stderr)
	}

	out := stdout
	if !*showSecrets {
		for i := range plan.Files {
			plan.Files[i].Diff = scrub.Redact(plan.Files[i].Diff)
		}
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *outPath != "" {
		real, rel, err := policy.Confine(root, *outPath)
		if err != nil {
			fmt.Fprintf(stderr, "error: --out: %v\n", err)
			return 1
		}
		if err := policy.CheckPath(rel, *allowProtected, *allowIgnored, nil); err != nil {
			fmt.Fprintf(stderr, "policy: %v\n", err)
			return 5
		}
		if err := writeFileAtomic(real, append(encoded, '\n'), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: write plan: %v\n", err)
			return 1
		}
	}
	if *jsonOut {
		_ = json.NewEncoder(out).Encode(plan)
		return 0
	}
	for _, f := range plan.Files {
		fmt.Fprintf(out, "%s %s\n", f.Action, f.Path)
		if f.Diff != "" {
			fmt.Fprintf(out, "%s", f.Diff)
		}
	}
	fmt.Fprintf(out, "\nplan digest: %s\n", plan.Digest)
	if *outPath != "" {
		fmt.Fprintf(out, "apply with: aed apply --plan %s --expect-digest %s -y\n", *outPath, plan.Digest)
	} else {
		fmt.Fprintf(out, "save the plan with --out FILE, then: aed apply --plan FILE --expect-digest %s -y\n", plan.Digest)
	}
	return 0
}

// runApply revalidates a plan and commits it atomically.
func runApply(args []string, stdout, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	planPath := fs.String("plan", "", "plan JSON file")
	expect := fs.String("expect-digest", "", "digest the plan must have")
	rootFlag := fs.String("root", "", "workspace root (must match the plan)")
	jsonOut := fs.Bool("json", false, "emit a JSON result")
	yes := fs.Bool("y", false, "apply without an interactive prompt")
	fs.BoolVar(yes, "yes", false, "apply without an interactive prompt")
	allowProtected := fs.Bool("allow-protected", false, "allow protected paths")
	allowIgnored := fs.Bool("allow-ignored", false, "allow .aedignore paths")
	noCheck := fs.Bool("no-check", false, "skip the syntax gate")
	showSecrets := fs.Bool("show-secrets", false, "do not redact secrets from output")
	fs.BoolVar(showSecrets, "no-scrub", false, "alias for --show-secrets")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *planPath == "" || *expect == "" {
		fmt.Fprintln(stderr, "usage: aed apply --plan PLAN.json --expect-digest DIGEST -y [--json]")
		return 2
	}
	if !*yes {
		fmt.Fprintln(stderr, "error: apply requires -y (non-interactive approval of the plan digest)")
		return 2
	}
	if *jsonOut {
		defer func() {
			if code == 0 {
				return
			}
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"version": 1, "ok": false, "applied": false,
				"error": map[string]any{"code": code, "message": exitMessage(code)},
			})
		}()
	}

	data, err := readSmallFile(*planPath, 32<<20)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	var plan applyplan.Plan
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil {
		fmt.Fprintf(stderr, "error: decode plan: %v\n", err)
		return 1
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		fmt.Fprintln(stderr, "error: decode plan: trailing data")
		return 1
	}
	rootArg := *rootFlag
	if rootArg == "" {
		rootArg, _ = os.Getwd()
	}
	abs, err := filepath.Abs(rootArg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if abs != plan.Root {
		fmt.Fprintf(stderr, "conflict: apply root %q does not match the plan root %q\n", abs, plan.Root)
		return 6
	}
	root := abs

	fresh, _, txID, err := applyplan.Apply(root, &plan, *expect, applyplan.Options{
		AllowProtected: *allowProtected,
		AllowIgnored:   *allowIgnored,
		NoSyntaxCheck:  *noCheck,
	})
	if err != nil {
		return reportPlanError(err, stderr)
	}

	type applyFile struct {
		Path   string           `json:"path"`
		Action applyplan.Action `json:"action"`
		Before string           `json:"before_sha256,omitempty"`
		After  string           `json:"after_sha256,omitempty"`
	}
	res := struct {
		Version int         `json:"version"`
		OK      bool        `json:"ok"`
		Applied bool        `json:"applied"`
		TXID    string      `json:"tx_id,omitempty"`
		Digest  string      `json:"plan_digest"`
		Files   []applyFile `json:"files"`
	}{Version: 1, OK: true, Applied: true, TXID: txID, Digest: fresh.Digest}
	for _, f := range fresh.Files {
		res.Files = append(res.Files, applyFile{Path: f.Path, Action: f.Action, Before: f.BeforeSHA, After: f.AfterSHA})
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(res)
		return 0
	}
	for _, f := range fresh.Files {
		fmt.Fprintf(stdout, "applied: %s %s\n", f.Action, f.Path)
	}
	fmt.Fprintf(stdout, "plan digest: %s\ntx: %s\n", fresh.Digest, txID)
	return 0
}

func reportPlanError(err error, stderr io.Writer) int {
	code := 1
	switch {
	case policy.IsDenied(err):
		code = 5
	case errorsAsConflict(err):
		code = 6
	case errorsAsSyntax(err):
		code = 4
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return code
}

func errorsAsConflict(err error) bool {
	_, ok := err.(*applyplan.ConflictError)
	return ok
}

func errorsAsSyntax(err error) bool {
	_, ok := err.(*applyplan.SyntaxError)
	return ok
}

// readSmallFile reads a file after an explicit size check.
func readSmallFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read it", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > max {
		return nil, fmt.Errorf("%s exceeds max size %d bytes", path, max)
	}
	return os.ReadFile(path) // #nosec G304 -- operator-supplied ops/plan file, size-checked above
}

func runLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit JSON")
	n := fs.Int("n", 20, "number of records (0 for all)")
	verify := fs.Bool("verify", false, "verify the hash chain")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *verify {
		if err := audit.Verify(); err != nil {
			fmt.Fprintf(stderr, "audit chain INVALID: %v\n", err)
			return 1
		}
		fmt.Fprintln(stderr, "audit chain ok")
	}
	recs, err := audit.Read(*n)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"records": recs})
		return 0
	}
	for _, r := range recs {
		fmt.Fprintf(stdout, "%d %s %s tx=%s files=%d root=%s %s\n",
			r.Seq, r.Time, r.Kind, r.TXID, len(r.Files), r.Root, r.Message)
	}
	return 0
}

func runUndo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("undo", flag.ContinueOnError)
	txID := fs.String("tx", "", "transaction id (default: latest)")
	rootFlag := fs.String("root", "", "workspace root")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	res, err := audit.Undo(root, *txID)
	if err != nil {
		return reportAuditError(err, stderr)
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"ok": true, "tx_id": res.TXID, "files": res.Files,
		})
		return 0
	}
	fmt.Fprintf(stdout, "undid transaction %s (%d file(s))\n", res.TXID, len(res.Files))
	return 0
}

func runRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "workspace root")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	results, err := audit.Recover(root)
	if err != nil {
		return reportAuditError(err, stderr)
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"ok": true, "recovered": results})
		return 0
	}
	if len(results) == 0 {
		fmt.Fprintln(stdout, "nothing to recover")
		return 0
	}
	for _, r := range results {
		fmt.Fprintf(stdout, "recovered transaction %s (%d file(s))\n", r.TXID, len(r.Files))
	}
	return 0
}

func reportAuditError(err error, stderr io.Writer) int {
	code := 1
	if _, ok := err.(*audit.ConflictError); ok {
		code = 6
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return code
}

func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "workspace root")
	allowWrite := fs.Bool("allow-write", false, "allow apply/undo/recover over MCP")
	allowProtected := fs.Bool("allow-protected", false, "allow protected paths")
	allowIgnored := fs.Bool("allow-ignored", false, "allow .aedignore paths")
	showSecrets := fs.Bool("show-secrets", false, "do not redact secrets in tool output")
	fs.BoolVar(showSecrets, "no-scrub", false, "alias for --show-secrets")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	srv := &mcp.Server{
		Root: abs, AllowWrite: *allowWrite, AllowProtected: *allowProtected,
		AllowIgnored: *allowIgnored, ShowSecrets: *showSecrets,
		In: stdin, Out: stdout, Err: stderr,
	}
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(stderr, "error: mcp: %v\n", err)
		return 1
	}
	return 0
}

func runLayaEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("laya-eval", flag.ContinueOnError)
	corpus := fs.String("corpus", "", "labeled JSONL corpus")
	jsonOut := fs.Bool("json", false, "emit JSON")
	url := fs.String("laya-url", "http://127.0.0.1:8420/mcp/", "Laya MCP URL")
	token := fs.String("laya-token", os.Getenv("LAYA_MCP_TOKEN"), "Laya MCP token")
	model := fs.String("laya-safety-model", "", "Laya checkpoint/model name")
	threshold := fs.Float64("laya-safety-threshold", 0.65, "review threshold")
	maxReview := fs.Float64("max-review", 0.3, "maximum review rate for the recommended threshold")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *corpus == "" {
		fmt.Fprintln(stderr, "usage: aed laya-eval --corpus FILE [--json]")
		return 2
	}
	cases, err := layaeval.Load(*corpus)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	client := laya.New(*url, *token)
	if !client.Configured() {
		fmt.Fprintln(stderr, "error: --laya-url is required")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep, err := layaeval.Run(ctx, client, cases, layasafety.Options{
		Model: *model, BlockBelow: *threshold, RequireIntent: true,
	}, *maxReview)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(rep)
		return 0
	}
	fmt.Fprintf(stdout, "cases: %d  review rate: %.2f  FNR: %.2f  ECE: %.3f  recommended threshold: %.2f\n",
		rep.Total, rep.ReviewRate, rep.FalseNegRate, rep.ECE, rep.Recommended)
	for _, name := range layaeval.SortedClassNames(rep.Classes) {
		cs := rep.Classes[name]
		fmt.Fprintf(stdout, "  %-6s total=%d allow=%d review=%d block=%d recall=%.2f mean_conf=%.2f\n",
			name, cs.Total, cs.Allow, cs.Review, cs.Block, cs.Recall, cs.MeanConf)
	}
	if len(rep.Failures) > 0 {
		fmt.Fprintln(stdout, "unsafe edits allowed:")
		for _, f := range rep.Failures {
			fmt.Fprintf(stdout, "  %s (confidence %.2f)\n", f.ID, f.Conf)
		}
	}
	return 0
}

// writeFileAtomic writes a small file via a random sibling and rename, refusing
// a pre-existing symlink. Used for plan output so --out cannot be redirected.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write it", path)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".aed-out-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
