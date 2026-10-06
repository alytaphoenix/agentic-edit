// Package cli wires the agent loop together: parse flags, gather context,
// generate operations, safety-check, dry-run, apply, verify, and (optionally)
// trigger Laya egress actions.
package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agentic-edit/internal/exec"
	"agentic-edit/internal/gen"
	"agentic-edit/internal/laya"
	"agentic-edit/internal/plan"
	"agentic-edit/internal/safety"
	"agentic-edit/internal/verify"
)

// maxFileBytes caps how large a target file may be. Files above this (or that
// look binary) are rejected up front.
const maxFileBytes = 50 * 1024 * 1024

const usageText = `aed — agentic file editing with awk/sed/grep + Laya

Usage:
  aed edit   -a "instruction" -f FILE [-f FILE...] [options]
  aed grep   [-E] [-c] PATTERN [FILE...]

edit options:
  -f, --file       Target file (repeatable) [required]
  -a, --action     Natural-language instruction [required]
  --dry-run        Preview only. Default: on (preview). Use -y to apply.
  -y, --yes        Apply without confirmation
  --verify         Verify after applying (grep asserts + optional Laya review)
  --llm            Use an LLM (Laya/Ollama/OpenAI) to plan the edit (opt-in)
  --no-llm         Skip LLM; use the deterministic generator only
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
	case "grep":
		return runGrep(args[1:], stdout, stderr)
	case "list-laya-tools":
		return listLayaTools(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, usageText)
		return 2
	}
}

func runEdit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	var files stringList
	fs.Var(&files, "f", "target file (repeatable)")
	fs.Var(&files, "file", "target file (repeatable)")
	action := fs.String("a", "", "natural-language instruction")
	fs.StringVar(action, "action", "", "natural-language instruction")
	dry := fs.Bool("dry-run", true, "preview only")
	yes := fs.Bool("y", false, "apply without confirmation")
	fs.BoolVar(yes, "yes", false, "apply without confirmation")
	verifyOn := fs.Bool("verify", false, "verify after applying")
	llmOn := fs.Bool("llm", false, "use an LLM to plan the edit")
	noLLM := fs.Bool("no-llm", false, "skip LLM; deterministic only")
	force := fs.Bool("force", false, "allow whole-file destructive deletes")
	rootFlag := fs.String("root", "", "workspace root for path confinement")
	layaURL := fs.String("laya-url", "http://127.0.0.1:8420/mcp/", "Laya MCP URL")
	layaToken := fs.String("laya-token", os.Getenv("LAYA_MCP_TOKEN"), "Laya MCP token")
	model := fs.String("model", "llama3.1", "local LLM model name")
	ollamaURL := fs.String("ollama-url", "http://localhost:11434", "Ollama URL")
	apiKey := fs.String("api-key", os.Getenv("OPENAI_API_KEY"), "OpenAI-compatible key")
	baseURL := fs.String("base-url", "https://api.openai.com/v1", "OpenAI-compatible base URL")
	egress := fs.String("egress", "", "optional Laya outbound action (opt-in)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *action == "" {
		fmt.Fprintln(stderr, "error: --action is required")
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "error: at least one -f FILE is required")
		return 2
	}

	useLLM := *llmOn && !*noLLM
	forceDeletes := *force
	root := *rootFlag
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}

	// Confinement + read + guards. Files are resolved to real paths inside the
	// workspace root, size-checked, and rejected if they look binary.
	confined := make([]string, 0, len(files))
	contents := map[string]string{}
	origStat := map[string]os.FileInfo{}
	for _, f := range files {
		real, err := confine(f, root)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		data, err := os.ReadFile(real)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot read %s: %v\n", real, err)
			return 1
		}
		if err := checkText(real, data); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		contents[real] = string(data)
		info, err := os.Stat(real)
		if err != nil {
			fmt.Fprintf(stderr, "error: cannot stat %s: %v\n", real, err)
			return 1
		}
		origStat[real] = info
		confined = append(confined, real)
	}
	defer exec.Cleanup()

	intent := plan.Parse(*action, confined)

	client := laya.New(*layaURL, *layaToken)
	var layaCtx string
	if useLLM {
		layaCtx = plan.Enrich(context.Background(), client, intent)
		if layaCtx != "" {
			fmt.Fprintf(stdout, "[context] %s\n", strings.ReplaceAll(layaCtx, "\n", "\n           "))
		}
	}

	res, err := gen.Generate(gen.Input{
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
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	for _, op := range res.Ops {
		if err := safety.Validate(op); err != nil {
			fmt.Fprintf(stderr, "safety: %v\n", err)
			return 1
		}
	}

	after := map[string]string{}
	for f, content := range contents {
		out := content
		for _, op := range res.Ops {
			if op.Path != "" && op.Path != f {
				continue
			}
			o, err := exec.DryRun(op, out)
			if err != nil {
				fmt.Fprintf(stderr, "error: %s failed: %v\n", f, err)
				return 1
			}
			out = o
		}
		after[f] = out
	}

	for _, f := range confined {
		d := exec.LineDiff(contents[f], after[f])
		if !hasDiff(d) {
			fmt.Fprintf(stdout, "  (no change to %s)\n", f)
		} else {
			fmt.Fprintf(stdout, "--- %s\n%s", f, d)
		}
	}

	apply := *yes || !*dry
	if !apply {
		fmt.Fprintln(stdout, "\n[dry-run] preview only; no files modified. Re-run with -y to apply.")
		return 0
	}

	// Stale-edit guard: abort if any target changed since the preview.
	for _, f := range confined {
		newInfo, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(stderr, "error: %q no longer accessible: %v\n", f, err)
			return 1
		}
		old := origStat[f]
		if old != nil && (old.ModTime().UnixNano() != newInfo.ModTime().UnixNano() || old.Size() != newInfo.Size()) {
			fmt.Fprintf(stderr, "error: %q changed since preview; aborting\n", f)
			return 1
		}
	}

	if !*yes && !confirm(stdin, stdout) {
		fmt.Fprintln(stdout, "aborted")
		return 0
	}

	for _, op := range res.Ops {
		for _, f := range confined {
			if op.Path != "" && op.Path != f {
				continue
			}
			target := op
			target.Path = f
			if err := exec.RunInPlace(target); err != nil {
				fmt.Fprintf(stderr, "error applying %s: %v\n", f, err)
				return 1
			}
		}
		fmt.Fprintf(stdout, "applied: %s\n", op.Purpose)
	}

	if *verifyOn {
		res2 := verify.Run(res, contents, after, client, useLLM)
		status := "ok"
		if !res2.OK {
			status = "FAILED"
		}
		fmt.Fprintf(stdout, "verify[%s]: %s\n", status, strings.Join(res2.Notes, " | "))
		if !res2.OK {
			return 3
		}
	}

	if *egress != "" && useLLM && client.Enabled() && client.Ping(context.Background()) {
		if txt, err := client.CallTool(context.Background(), *egress, map[string]any{"description": intent.Description}); err != nil {
			fmt.Fprintf(stderr, "egress skipped: %v\n", err)
		} else {
			fmt.Fprintf(stdout, "egress[%s]: %s\n", *egress, txt)
		}
	}
	return 0
}

func runGrep(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grep", flag.ContinueOnError)
	ext := fs.Bool("E", false, "extended regex")
	count := fs.Bool("c", false, "count matches")
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
	if len(files) == 0 {
		files = []string{"."}
	}

	if *count {
		n, err := exec.Count(pattern, files, *ext)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		for f, c := range n {
			fmt.Fprintf(stdout, "%d\t%s\n", c, f)
		}
		return 0
	}

	lines, err := exec.Search(pattern, files, *ext)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	for _, l := range lines {
		fmt.Fprintf(stdout, "%s:%d:%s\n", l.File, l.Num, l.Text)
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
	if !c.Enabled() {
		fmt.Fprintln(stderr, "error: --laya-token is required")
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
	real := abs
	if _, err := os.Stat(abs); err == nil {
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			real = r
		}
	}
	rootReal, err := filepath.EvalSymlinks(root)
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

// hasDiff reports whether a LineDiff output contains any removed/added lines.
func hasDiff(d string) bool {
	for _, line := range strings.Split(d, "\n") {
		if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+") {
			return true
		}
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
