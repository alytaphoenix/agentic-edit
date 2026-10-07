# aed — agentic file editing with awk/sed/grep + Laya

`aed` is a command-line tool that turns a natural-language edit instruction into
safe, verifiable edits of one or more files. It drives the three classic Unix
text utilities — **awk**, **sed**, and **grep** — and can lean on **Laya** (and
any local Ollama / OpenAI-compatible model) for context and command generation.

It is designed to be *safe by default*: it previews changes, writes backups,
runs a safety denylist, and optionally verifies the result after applying.

## Install / build

```sh
go build -o aed ./cmd/aed
```

Requires Go 1.26+.

## Usage

```sh
# Preview only (default): shows the diff, changes nothing
aed edit -a 'replace "v1" with "v3"' -f path/to/file.go

# Apply without prompting, then verify the result
aed edit -a 'replace "v1" with "v3"' -f path/to/file.go -y --verify

# Edit several files at once
aed edit -a 'insert "// generated" after line matching package' -f a.go -f b.go -y --verify

# Transform lines with awk
aed edit -a 'uppercase every line' -f notes.txt -y --verify

# Plain grep (returns 0 even when nothing matches)
aed grep -E '^todo' -n *.md
```

### `edit` options

| Flag | Description |
|------|-------------|
| `-f, --file` | Target file (repeatable) **[required]** |
| `-a, --action` | Natural-language instruction **[required]** |
| `--dry-run` | Preview only. Default on; pass `-y` to apply. |
| `-y, --yes` | Apply without the interactive confirmation prompt. |
| `--verify` | After applying, assert the change and summarize the diff. **Default on.** |
| `--no-verify` | Disable the post-apply verification guard. A guard failure exits `3`. |
| `--show-secrets` | Do not redact secrets from LLM prompts, stdout diffs, and the review prompt. Alias: `--no-scrub`. **Default off (scrub on).** |
| `--llm` | Enable the LLM (Laya/Ollama/OpenAI) to plan the edit (**opt-in**). |
| `--no-llm` | Skip LLM/Laya; use the deterministic generator only. |
| `--force` | Allow whole-file destructive deletes (e.g. delete matching `.`). |
| `--root` | Workspace root for path confinement (default: current dir). |
| `--laya-url` | Laya MCP endpoint (default `http://127.0.0.1:8420/mcp/`). |
| `--laya-token` | Laya bearer token (env `LAYA_MCP_TOKEN`). |
| `--model` | Local LLM model name (default `llama3.1`). |
| `--ollama-url` | Ollama endpoint (default `http://localhost:11434`). |
| `--api-key` | OpenAI-compatible key (env `OPENAI_API_KEY`). |
| `--base-url` | OpenAI-compatible base URL. |
| `--egress` | Optional Laya outbound action to run after a successful edit. |

### `grep` subcommand

```sh
aed grep [-E] [-c] PATTERN [FILE...]
```

A thin wrapper over `grep -Hn` (`-E` for extended regex, `-c` for counts). It
exits `0` when nothing matches, so it is safe in scripts and previews.

## How it works

```
natural-language instruction
          │
          ▼
   plan.Parse            → structured intent + target files
          │
          ▼
   plan.Enrich (Laya)    → optional background context (skipped if Laya off)
          │
          ▼
   gen.Generate          → typed ops via intent.Parse + compile.Render
         └─ LLM (opt-in, --llm) → emits typed ops only, validated before render
          │
          ▼
   safety.Validate       → denylist blocks :d, 1,$d, rm, system(), etc.
          │
          ▼
   Dry-run (awk/sed/grep on in-memory content) → unified diff shown to the user
          │
          ▼
   Apply (backup → atomic replace) → verify (asserts + diff summary)
```

By default the tool is fully deterministic: a small keyword grammar turns the
instruction into **typed operations**, which are rendered through a fixed
template whitelist. The only way to inject a command is as one of these known
shapes — never a free-form script. The LLM is strictly **opt-in** (`--llm`);
when used, the model may only emit structured, typed operations, which are
validated before rendering.
### Safety

- **Safe by construction**: commands are produced from a fixed template
  whitelist (`internal/compile`) by escaping inputs; a free-form script can
  never be injected. This is the primary guarantee.
- **Denylist** (`internal/safety`): defense-in-depth; operations containing
  `:d`, `1,$d`, `1,$ s`, `rm -`, `> /dev/null`, `system(`, or `exit(1)` are
  rejected.
- **Path confinement**: files are resolved (realpath + symlink check) and must
  stay inside `--root` (default: current dir). `..` escapes are refused.
- **Binary / size guard**: files over 50 MB or containing NUL bytes are
  rejected up front.
- **Stale-edit guard**: each target's mtime+size are re-checked right before
  apply; if the file changed since the preview, the edit is aborted.
- **Mode preservation + fsync**: applying writes to a temp file, fsyncs it, and
  preserves the original file mode (including executable bits) across the atomic
  rename.
- **Exec timeout**: every awk/sed/grep call is bounded (`exec.DefaultTimeout`,
  30s).
- **Private temp dir**: temp files live in a process-private directory that is
  cleaned up on exit.
- **Backups**: applying writes `<file>.bak` before mutating, and rolls back on
  any failure.
- **Secret scrubbing**: file contents flowing to an LLM prompt, printed diffs,
  and the verify-review prompt are redacted by `internal/scrub` (AWS keys,
  private keys, tokens, connection strings, `key = value` assignments) unless
  `--show-secrets` (alias `--no-scrub`) is passed. Redaction is output-only — it
  never changes what is written to disk.
- **Post-apply guard**: `verify` asserts the change actually landed (Assert =
  new value present, Deny = old value / deleted pattern absent) using literal
  `grep -F` matches, skips no-ops, and prints an error diff and exits `3` if not.
  `--no-verify` opts out. On a failure in an interactive shell it prompts to
  restore the file from its backup.
- **Preview first**: `edit` previews by default; use `-y` to apply.
- **Destructive deletes gated**: whole-file deletes (a pattern matching every
  line) require `--force`.

## Architecture

| Package | Responsibility |
|---------|----------------|
| `cmd/aed` | CLI entrypoint |
| `internal/cli` | Flag parsing, the edit/grep command loop |
| `internal/plan` | Intent parsing + Laya context enrichment |
| `internal/intent` | Parses instruction into typed ops; validates structure |
| `internal/compile` | Renders typed ops through a fixed safe-template whitelist |
| `internal/gen` | Wires LLM (opt-in, typed ops only) + deterministic pipeline |
| `internal/exec` | Safe `DryRun`/`RunInPlace` wrappers over awk/sed/grep, line search, diffing |
| `internal/safety` | Denylist validation |
| `internal/scrub` | Redacts secrets from LLM prompts, stdout, and the review prompt |
| `internal/verify` | Post-apply asserts + diff summary + optional Laya review |
| `internal/laya` | Laya MCP client |
| `internal/llm` | Ollama / OpenAI-compatible HTTP clients |

## Tests

```sh
go test ./...
```
