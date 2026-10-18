// Package cli wires the agent loop together: parse flags, gather context,
// generate operations, safety-check, dry-run, apply, verify, and (optionally)
// trigger Laya egress actions.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/gen"
	"agentic-edit/internal/lang"
	"agentic-edit/internal/laya"
	"agentic-edit/internal/layasafety"
	"agentic-edit/internal/plan"
	"agentic-edit/internal/safety"
	"agentic-edit/internal/scrub"
	"agentic-edit/internal/tx"
	"agentic-edit/internal/verify"
)

// maxFileBytes caps how large a target file may be. Files above this (or that
// look binary) are rejected up front.
const maxFileBytes = 50 * 1024 * 1024

// Version is the CLI version reported by `aed version`.
const Version = "0.2.0"

const usageText = `aed — agentic file editing with awk/sed/grep + Laya

Usage:
  aed edit   -a "instruction" -f FILE [-f FILE...] [options]
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
	keepOnFailure := fs.Bool("keep-on-failure", false, "keep the edit if verification fails")
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
	rootForRel, _ := filepath.Abs(root)
	if realRoot, err := filepath.EvalSymlinks(rootForRel); err == nil {
		rootForRel = realRoot
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
		real, err := confine(f, root)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if rel, err := filepath.Rel(rootForRel, real); err == nil && isProtected(rel) && !*allowProtected {
			msg := fmt.Sprintf("%q is a protected path; re-run with --allow-protected to edit it", f)
			fmt.Fprintf(stderr, "policy: %s\n", msg)
			res.Error = &editError{Code: 5, Message: msg}
			return 5
		}
		data, mode, err := tx.ReadFileLimit(real, maxFileBytes)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot read %s: %v\n", real, err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
		if err := checkText(real, data); err != nil {
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
			o, err := exec.DryRun(op, out)
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
	for _, f := range confined {
		d := exec.LineDiff(contents[f], after[f])
		changed := hasDiff(d)
		if changed {
			changedFiles++
		}
		added, removed := diffStats(d)
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
				relForFlags, _ := filepath.Rel(rootForRel, f)
				state := map[string]any{
					"instruction": scrubFn(intent.Description),
					"path":        scrubFn(filepath.ToSlash(relForFlags)),
					"diff":        scrubFn(d),
					"stats":       fmt.Sprintf("+%d -%d", added, removed),
					"flags":       fmt.Sprintf("syntax=checked; protected=%t", isProtected(relForFlags)),
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
	for _, f := range confined {
		if after[f] == contents[f] {
			continue
		}
		if err := t.Stage(f, []byte(contents[f]), []byte(after[f]), origMode[f]); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			res.Error = &editError{Code: 1, Message: err.Error()}
			return 1
		}
	}
	if err := t.Commit(); err != nil {
		code := 1
		if strings.Contains(err.Error(), "conflict") {
			code = 6
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		res.Error = &editError{Code: code, Message: err.Error()}
		return code
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
		data, _, err := tx.ReadFileLimit(abs, maxFileBytes)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot read %s: %v\n", p, err)
			return 1
		}
		if err := checkText(abs, data); err != nil {
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

// confine resolves path against the workspace root and refuses anything that
// escapes it (via .. or symlinks). It returns the resolved real path.
func confine(path, root string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Refuse to edit a path whose final component is a symlink. Resolving it
	// would silently edit a different file; --follow-symlinks is intentionally
	// not offered in this version.
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%q is a symlink; refusing to edit it", path)
	}
	real := abs
	if _, err := os.Stat(abs); err == nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			real = r
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("workspace root %q is not accessible: %v", root, err)
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q is outside the workspace root %q", path, root)
	}
	return real, nil
}

// checkText rejects files that are too large or contain NUL bytes (binary).
func checkText(name string, data []byte) error {
	if len(data) > maxFileBytes {
		return fmt.Errorf("%q exceeds max size %d bytes", name, maxFileBytes)
	}
	for _, b := range data {
		if b == 0 {
			return fmt.Errorf("%q looks like a binary file (contains a NUL byte)", name)
		}
	}
	return nil
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

// isProtected reports whether rel (relative to the workspace root) is a path
// whose modification commonly leads to code execution or credential exposure.
// The list is deliberately conservative and can be overridden explicitly with
// --allow-protected (which is recorded in the audit log in a future release).
func isProtected(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	// Compare the basename case-insensitively: "makefile" is just as valid to
	// make as "Makefile", and macOS filesystems are case-insensitive anyway.
	relLower := strings.ToLower(rel)
	base := strings.ToLower(filepath.Base(rel))
	for _, p := range []string{
		".git/", ".hg/", ".svn/", ".ssh/", ".config/git/",
		".github/workflows/", ".github/actions/",
		".husky/", ".git/hooks/", ".gnupg/", ".kube/", ".docker/",
		".aws/", ".azure/", ".config/gcloud/", ".config/fish/",
	} {
		if relLower == strings.TrimSuffix(p, "/") || strings.HasPrefix(relLower, p) {
			return true
		}
	}
	for _, name := range []string{
		".env", ".envrc", ".npmrc", ".pypirc", ".netrc", ".bashrc", ".zshrc",
		".profile", ".bash_profile", ".zprofile", ".gitmodules",
		".gitconfig", "jenkinsfile", "makefile", "gnumakefile",
		"docker-compose.yml", "docker-compose.yaml", ".gitlab-ci.yml",
		".pre-commit-config.yaml", "lefthook.yml",
	} {
		if base == name {
			return true
		}
	}
	if strings.HasPrefix(base, ".env.") ||
		strings.HasPrefix(base, "id_rsa") ||
		strings.HasPrefix(base, "id_ed25519") ||
		strings.HasPrefix(base, "id_ecdsa") ||
		strings.HasPrefix(base, "credentials") ||
		strings.HasPrefix(base, "secrets") ||
		strings.HasPrefix(base, "dockerfile") ||
		strings.HasPrefix(base, "containerfile") ||
		strings.HasSuffix(base, ".pem") ||
		strings.HasSuffix(base, ".key") ||
		strings.HasSuffix(base, ".p12") ||
		strings.HasSuffix(base, ".pfx") ||
		strings.HasSuffix(base, ".jks") ||
		strings.HasSuffix(base, ".mk") ||
		strings.HasPrefix(base, "docker-compose.") {
		return true
	}
	return false
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
