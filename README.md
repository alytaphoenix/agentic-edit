# aed — agentic file editing with awk/sed/grep + Laya

`aed` is a command-line tool that turns a natural-language edit instruction into
safe, verifiable edits of one or more files. It drives the three classic Unix
text utilities — **awk**, **sed**, and **grep** — for compatibility, and can use
**Laya** for local edit-risk decisions plus an Ollama / OpenAI-compatible model
for optional command generation.

It is designed to be *safe by default*: it previews changes, applies them through
a byte-exact all-or-nothing transaction, runs a safety denylist and a per-language
syntax gate, confines paths, refuses symlinks/hardlinks, and verifies the result
by hash after applying.

For agents, `aed edit --json` emits a stable machine-readable result, and
`--laya-safety=warn|block` adds a local decision-model review of the diff.

Current version: **0.4.0** (see [CHANGELOG.md](CHANGELOG.md)).

## Install / build

```sh
go build -o aed ./cmd/aed
```

Requires Go 1.26.9+ (the os.Root and net/http fixes in that patch release are part of the security model).

## Usage

```sh
# Preview only (default): shows the diff, changes nothing
aed edit -a 'replace "v1" with "v3"' -f path/to/file.go

# Apply without prompting, then verify the result
aed edit -a 'replace "v1" with "v3"' -f path/to/file.go -y --verify

# Edit several files at once (each replace must match exactly once)
aed edit -a 'replace "v1" with "v3"' -f a.go -f b.go -y --verify

# Allow a replace to hit every occurrence
aed edit -a 'replace "v1" with "v3"' -f a.go -y --all

# Machine-readable result for an agent
aed edit -a 'replace "v1" with "v3"' -f a.go --json

# Optional local Laya edit-risk review
aed edit -a 'replace "v1" with "v3"' -f a.go --json \
  --laya-safety=block --laya-token "$LAYA_MCP_TOKEN"

# Transform lines with awk
aed edit -a 'uppercase every line' -f notes.txt -y --verify

# Plain grep (recursive when no files are given)
aed grep -E '^todo' -n *.md

# Two-phase, digest-bound edit for agents
aed plan --ops ops.json --out plan.json
aed apply --plan plan.json --expect-digest sha256:... -y --json

# Audit, undo, and crash recovery
aed log -n 10
aed undo
aed recover

# Safe MCP server (read-only unless --allow-write)
aed mcp --root . --allow-write
```

### `edit` options

| Flag | Description |
|------|-------------|
| `-f, --file` | Target file (repeatable) **[required]** |
| `-a, --action` | Natural-language instruction **[required]** |
| `--dry-run` | Preview only. Default on; pass `-y` to apply. |
| `-y, --yes` | Apply without the interactive confirmation prompt. `--apply` is an alias. |
| `--verify` | After applying, assert the change and summarize the diff. **Default on.** |
| `--no-verify` | Disable the post-apply verification guard. A guard failure exits `3` and rolls back. |
| `--check` | Syntax-check the edited result with the language's own parser **before** applying. **Default on** (a regression exits `4`). |
| `--no-check` | Skip the pre-apply syntax check. |
| `--unique` | Require exactly one match for each literal or regex replace. **Default on.** |
| `--all` | Allow a replace to match more than once (turns off `--unique`). |
| `--keep-on-failure` | Keep the edited file if verification fails. Default is automatic rollback. |
| `--json` | Emit one JSON result on stdout; human progress goes to stderr. |
| `--laya-safety` | `off` (default), `warn`, or `block`. Runs a Laya edit-risk decision over the diff. |
| `--laya-safety-model` | Laya checkpoint/model name (default: server `auto`). |
| `--laya-safety-threshold` | Confidence below this becomes `review` (default `0.65`). |
| `--show-secrets` | Do not redact secrets from LLM prompts, stdout diffs, `grep` output, and the review prompt. Alias: `--no-scrub`. **Default off (scrub on).** |
| `--allow-protected` | Allow editing protected paths (`.git/`, CI workflows, keys, `.env`, shell rc, Makefile, ...). |
| `--allow-ignored` | Allow editing paths matched by `.aedignore`. |
| `--engine` | `native` (default, in-process) or `sed` (compatibility backend). |
| `--llm` | Enable an Ollama / OpenAI-compatible model to plan the edit (**opt-in**). Laya itself is a decision model, not a generator. |
| `--no-llm` | Skip LLM assistance; use the deterministic generator only. |
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
aed grep [-E] [-c] [--json] [--show-secrets] PATTERN [FILE...]
```

A thin wrapper over `grep -Hn` (`-E` for extended regex, `-c` for counts).
With no `FILE` arguments it searches recursively from the current directory
(`grep -rHn`) instead of passing `.` to non-recursive grep. `--json` emits
`{"matches":[...]}` or `{"counts":{...}}`. Match text is secret-scrubbed by
default; pass `--show-secrets` (alias `--no-scrub`) to print it raw. It exits `0`
when nothing matches, so it is safe in scripts and previews.

### `check` subcommand

```sh
aed check [-json] FILE [FILE...]
```

Runs each file's language syntax checker and prints diagnostics in
`path:line:col: message` form. `--json` emits a structured report. Exits `4` if
any file has syntax problems. Returns early on I/O or binary/size errors (exit
`1`). Unsupported file types are reported as skipped, not as failures.

### `plan` / `apply` subcommands

```sh
aed plan  --ops OPS.json [--out PLAN.json] [--json]
aed apply --plan PLAN.json --expect-digest DIGEST -y [--json]
```

`plan` reads a strict JSON array (or `{"version":1,"ops":[...]}`) of typed
operations, applies them in memory, and emits a canonical plan containing the
before/after SHA-256 for every file, the diff, diagnostics, and a digest over
the ops, root, paths, hashes, and modes. `apply` requires that digest, re-reads
the files, rebuilds the plan, and refuses with exit `6` if anything changed. It
runs the syntax gate and policy checks again before committing one transaction.

Supported ops:

| `kind` | Fields |
|---|---|
| `replace_literal` | `path`, `old`, `new`, `expect_count` **or** `global:true` |
| `replace_regex` | same, RE2 syntax |
| `insert` | `path`, `anchor`, `text`, `insert_before`, `all` |
| `replace_range` / `delete_range` | `path`, `start`, `end`, `text` (replace only) |
| `apply_patch` | `path`, `patch` (strict unified diff) |
| `create_file` | `path`, `content`, `mode` |
| `delete_file` | `path` |
| `rename_file` | `from`, `to` |
| `chmod` | `path`, `mode` |

Paths are relative to the root, must be clean, and cannot escape with `..`.
Unknown fields and fields not valid for the operation kind are rejected.
`--out` is confined to the workspace root and written atomically (0600); plan
files are inputs to `apply`, so keep them out of `.aedignore`d trees.

### `log`, `undo`, `recover`

```sh
aed log [-n N] [--json] [--verify]
aed undo [--tx ID] [--root DIR] [--json]
aed recover [--root DIR] [--json]
```

Every successful apply appends a hash-chained JSONL record under
`~/.local/state/aed/` (mode `0700`) with the transaction id, root, instruction,
and per-file before/after hashes. `log --verify` checks the chain.
`undo` restores the latest (or named) transaction, but only if every file still
matches the recorded after-hash. A crash journal with before-content blobs is
written before the first rename, so `recover` can roll back an interrupted
transaction.

### `mcp` subcommand

```sh
aed mcp [--root DIR] [--allow-write] [--allow-protected] [--allow-ignored] [--show-secrets]
```

Speaks MCP over stdio (newline-delimited JSON-RPC). Tools: `read`, `search`,
`check`, `plan`, `apply`, `undo`, `recover`, `log`, `status`. The server is
read-only unless `--allow-write` is passed; `apply` additionally requires the
plan digest. All paths are confined to `--root` and go through the same policy,
syntax gate, transaction, and audit log as the CLI.

### Laya evaluation

```sh
aed laya-eval --corpus cases.jsonl --json \
  --laya-url http://127.0.0.1:8420/mcp/ --laya-token "$LAYA_MCP_TOKEN"
```

Runs a labeled corpus through `layasafety`, reporting per-class recall, the
false-negative rate (unsafe edit allowed), review rate, expected calibration
error, and a recommended threshold for a target review budget. See
`internal/layaeval/testdata/corpus.jsonl` for the format and README's Laya
section for the fine-tuning workflow.

### Exit codes

| Code | Meaning |
|------|---------|
| `0` | Success (including a preview or a no-op). |
| `1` | Argument, I/O, preflight, or other runtime error. |
| `2` | Usage error (unknown command / missing required flag / invalid safety mode). |
| `3` | Post-apply verify guard failed. The transaction is rolled back unless `--keep-on-failure`. |
| `4` | Syntax gate: the edit would leave invalid syntax. Override with `--no-check`. |
| `5` | Policy denied (protected path, Laya `block`/`review`, or destructive edit). |
| `6` | Conflict: a target changed after preview, or the workspace lock is held. |
| `8` | Safety checker unavailable while `--laya-safety=block` (fail closed). |

## How it works

```
instruction / ops.json
          │
          ├─ edit → plan.Parse → intent grammar → typed ops
          └─ plan → ir.Parse → strict typed ops + digest
          │
          ▼
   policy (root confinement, protected paths, .aedignore, limits)
          │
          ▼
   native engine (default) ── or ── sed compatibility backend (--engine=sed)
          │
          ▼
   Safety.Validate + Laya edit-risk decision (warn/block, fail-closed)
          │
          ▼
   Syntax gate (internal/lang) — per-language parse of the in-memory result
          │
          ▼
   Transaction (internal/tx) — os.Root → hash guard → stage → fsync → rename
          │
          ▼
   Audit journal (before blobs) + hash-chained JSONL record
          │
          ▼
   Verify (hash + asserts) → rollback · undo · recover
```

By default the tool is fully deterministic: a small keyword grammar turns the
instruction into **typed operations**, which the in-process native engine
applies without a shell. `--engine=sed` keeps the awk/sed backend for
compatibility. The LLM is strictly **opt-in** (`--llm`); when used, the model may
only emit structured, typed operations, which are validated before rendering.
Unknown JSON fields are rejected.

For agents, `plan`/`apply` split the operation in two: the plan is canonical and
digest-bound, and apply revalidates the digest, hashes, policy, and syntax before
writing. This removes the preview/apply drift that a single-step edit can have.

### Safety

- **Safe by construction**: edit instructions are parsed into a fixed set of
  typed operations and applied by the in-process native engine (`internal/native`)
  with no shell. The sed backend (also fixed-template) remains available for
  compatibility. Operations are produced from a whitelist (`internal/compile`)
  by escaping inputs. Replace is **literal by
  default** and requires exactly one match unless `--all` is passed.
  `"regex": true` opts into POSIX ERE (`sed -E`) matching and is still
  unique-by-default; patterns whose meaning differs between Go's regexp (used
  for the count guard) and sed are rejected. `scope` addresses must be a single
  `/regex/`, and separators, braces, `!`, `#`, and backslashes are refused. A
  free-form script cannot be injected.
- **Root-confined transaction**: `internal/tx` opens the workspace with
  `os.Root`, so every read, stage, rename, and rollback is confined to that tree
  even if a parent directory is swapped for a symlink. It stages the exact bytes
  that were previewed and syntax-checked, fsyncs them, re-checks the original
  hash immediately before each rename, and renames atomically. There is no
  separate re-execution of `sed` at apply time.
- **All-or-nothing rollback**: every touched file is part of one transaction. A
  failure at any point restores every file already replaced. `--keep-on-failure`
  opts out of the automatic rollback after a failed verify.
- **No predictable sibling paths**: the old `file.bak` / `file.aed.tmp` names are
  gone. Staging files use random `.aed-<random>` names created with `O_EXCL`
  through the root, so a pre-created symlink cannot redirect a write.
- **Symlink/hardlink refusal**: target symlinks, hardlinked files, devices, and
  other non-regular files are rejected. `..` escapes and symlinked paths outside
  `--root` are refused.
- **Hash stale-edit guard**: each target's SHA-256 is re-checked immediately
  before the rename; a concurrent change returns exit `6` instead of being
  overwritten. A per-workspace advisory lock serialises aed transactions.
- **Protected paths**: `.git/`, `.hg/`, `.ssh/`, CI workflows and actions,
  `.env`, keys/keystores, credentials, shell rc files, Makefiles, Dockerfiles,
  git hooks, and similar code-execution/credential surfaces are refused unless
  `--allow-protected` is passed. Matching is case-insensitive so `makefile` and
  `Dockerfile.dev` are covered too.
- **Binary / size guard**: files over 50 MB or containing NUL bytes are
  rejected. The size limit is enforced before the read, so an oversized file
  cannot exhaust memory just by being opened.
- **Blast-radius limits**: at most 32 files, 256 MB of read+write content, 5,000
  changed lines, 2,500 deletions, and a 4 MB diff per operation. Limits are
  checked while building the plan, before the transaction.
- **`.aedignore`**: root-relative glob patterns can mark generated or vendored
  trees as off-limits; `--allow-ignored` is required to override them.
- **Digest-bound plans**: `aed plan` binds ops, paths, hashes, modes, and the
  root into a SHA-256 digest; `aed apply` revalidates it and rebuilds the plan
  before writing, so a preview cannot drift from the applied bytes.
- **Auditable and reversible**: every apply writes a pending before-content
  journal plus a hash-chained audit record. `aed undo` restores the last
  transaction when the files still match its after-hashes; `aed recover`
  finishes or rolls back a transaction interrupted by a crash.
- **Mode preservation + fsync**: the original mode (including executable bits)
  is preserved across the atomic rename; the file and its directory are fsynced.
- **Exec timeout**: every awk/sed/grep call is bounded (`exec.DefaultTimeout`,
  30s).
- **Private temp dir**: sed scripts live in a process-private directory that is
  cleaned up on exit.
- **Secret scrubbing**: file contents, instructions, printed diffs, `grep`
  output, and the verify-review prompt are redacted by `internal/scrub` (AWS,
  OpenAI, Anthropic, GitHub, GitLab, Slack, Google, and Stripe tokens, JWTs,
  private keys, connection strings, and `key = value` assignments — including
  quoted values) unless `--show-secrets` (alias `--no-scrub`) is passed.
  Redaction is output-only — it never changes what is written to disk.
- **Post-apply guard**: `verify` asserts the change actually landed (Assert =
  new value present, Deny = old value / deleted pattern absent), skips no-ops,
  and prints an error diff. A failure exits `3` and rolls the whole transaction
  back. `--no-verify` opts out.
- **Syntax gate**: before applying, `internal/lang` runs each target's own
  language parser on the in-memory result. A clean → broken transition aborts
  the edit **before** anything is written and exits `4` (use `--no-check` to
  override). Files that were already broken stay editable. The gate is
  pluggable: a language is an `init()`-registered `Checker` (see
  `internal/lang`). Binary location is overridable via `AED_BASH` /
  `AED_PYTHON`.
- **Bounded diff**: the diff uses prefix/suffix trimming, patience anchors, and a
  size-limited LCS instead of an `n×m` table. A 50,000-line file with a one-line
  change now uses ~25 MB rather than ~221 MB at 5,000 lines.
- **Preview first**: `edit` previews by default; use `-y` to apply.
- **Destructive deletes gated**: whole-file deletes (a pattern matching every
  line) require `--force`.
- **Read-only grep plans**: a `find ...` instruction returns matches and never
  pretends the file was deleted or rewritten.

## Language support (pluggable syntax gate)

Every target file passes through `internal/lang` before applying, using the
language's own parser — not a heuristic. Supported today:

| Language | Parser | Env override |
|----------|--------|--------------|
| bash | `bash -n` | `AED_BASH` |
| python | `compile()` (via `python3`) | `AED_PYTHON` |
| go | `go/parser` (native, no external tool) | — |
| json / jsonl | `encoding/json` (native) | — |
| xml | `encoding/xml` (native) | — |
| toml | `tomllib` (via `python3`) | `AED_PYTHON` |
| ini / cfg | `configparser` (via `python3`) | `AED_PYTHON` |
| properties | native line-level check | — |
| yaml / yml | PyYAML (via `python3`) — needs `pip install pyyaml`; otherwise skipped | `AED_PYTHON` |

**Adding a language** is intentionally a one-file job: implement or reuse a
`Checker` and call `lang.Register(...)` from an `init()`. Most languages never
need a Go parser at all — wrap their CLI in a `CommandChecker`:

```go
func init() {
	lang.Register(&lang.CommandChecker{
		Language:   "ruby",
		Extensions: []string{".rb"},
		Shebangs:   []string{"ruby"},
		Bin:        "ruby",
		Env:        "AED_RUBY",
		Args:       []string{"-c", lang.StdinPlaceholder},
		Timeout:    10 * time.Second,
		Parse: func(output string) []lang.Diagnostic { /* map lines -> Diagnostic */ return nil },
	})
}
```

That one file makes `aed edit` show `syntax[FAILED]` (exit `4`) for Go files
too, with no changes to `cli.go`. New-language support is also what lets
`aed check` know about it automatically — no flag or register-table edits.

## Laya edit-risk review

`--laya-safety=warn|block` asks a local Laya MCP server (`laya_decide`) to
classify the proposed diff. The state contains the instruction, the redacted
diff, change stats, and deterministic flags; Laya returns a `risk`
(`allow`/`review`/`block`), `intent_match`, `destructive`, `scope_creep`, and
`secret_exposure`.

Policy is monotonic: deterministic checks and the syntax gate remain
authoritative, and Laya can only **escalate** (allow → review → block). In
`block` mode a `review` is also a denial (`5`), not permission to proceed: only
a high-confidence `allow` whose `intent_match` is at least "mostly" is applied.
`warn` mode prints the verdict and continues. If Laya is unavailable in `block`
mode, aed exits `8` rather than silently continuing. Unknown or missing risk
values and missing confidence are treated as `review`, never `allow`.

Important: Laya is a fast decision model, not a generative LLM. The shipped base
checkpoints are near chance zero-shot on typed decisions (its own benchmark:
0.36 vs 0.318 random), while a fine-tuned checkpoint reaches 0.766. Treat the
base model as an **advisory** prompt-injection/coarse-risk detector. For a real
safety gate, fine-tune and calibrate a checkpoint on your own edit corpus:

1. Log every transaction: instruction, redacted diff, deterministic flags, and
   outcome (accepted, rejected, rolled back, human-modified).
2. Add synthetic adversarial edits: mass deletion, CI/hook edits, `curl | sh`,
   auth bypass, secret addition, bidi/zero-width trojans, prompt injection,
   dependency confusion, permission changes.
3. Keep hard negatives: legitimate large refactors, formatting, vendored code,
   snapshot updates, migrations.
4. Fine-tune with Laya's typed-decisions notebook (or a frozen-encoder head),
   fit temperatures, and evaluate per-class recall, false-negative rate, ECE,
   and review rate on held-out repos.
5. Ship a versioned checkpoint and choose a high-recall threshold; never let the
   model be the only gate.

## Architecture

| Package | Responsibility |
|---------|----------------|
| `cmd/aed` | CLI entrypoint |
| `internal/cli` | Flag parsing, edit/plan/apply/grep/log/undo/recover/mcp loops |
| `internal/plan` | Intent parsing + optional chat-capable context enrichment |
| `internal/intent` | Parses instruction into typed ops; validates structure |
| `internal/compile` | Renders typed ops through a fixed safe-template whitelist |
| `internal/gen` | Wires LLM (opt-in, typed ops only) + deterministic pipeline |
| `internal/native` | In-process, byte-stable edit engine (BOM/CRLF/final newline) |
| `internal/ir` | Strict typed ops, unknown-field rejection, plan limits |
| `internal/applyplan` | Canonical plan, digest, revalidation, apply |
| `internal/policy` | Root confinement, protected paths, `.aedignore`, blast-radius limits |
| `internal/audit` | Hash-chained JSONL log, before-content journal, undo/recover |
| `internal/mcp` | Read-only-by-default MCP stdio server |
| `internal/layaeval` | Recall/FNR/ECE evaluation and threshold recommendation |
| `internal/exec` | sed/awk/grep DryRun backend, grep search, bounded diff |
| `internal/tx` | Root-confined, hash-guarded, all-or-nothing transactions (update/create/delete/rename/chmod) |
| `internal/safety` | Structural template check + denylist for hand-built ops |
| `internal/lang` | Per-language syntax gate (pluggable: bash, python today; CommandChecker adapts any CLI) |
| `internal/scrub` | Redacts secrets from LLM prompts, stdout, and the review prompt |
| `internal/verify` | Post-apply asserts + diff summary + optional chat-capable review |
| `internal/laya` | Laya MCP client (initialize, SSE, `laya_decide`/`laya_predict`) |
| `internal/layasafety` | Typed edit-risk decision + conservative policy normalization |
| `internal/llm` | Ollama / OpenAI-compatible HTTP clients |

## Tests

```sh
go test ./...
go test -race ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

The suite covers the native engine (BOM/CRLF/final newline), IR validation,
digest tampering and stale plans, create/delete/rename/chmod transactions,
symlink and parent-escape attacks, crash recovery, audit-chain tampering, MCP
tool calls, Laya fail-closed normalization, and macro evaluation metrics.
