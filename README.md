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
| `--verify` | After applying, assert the change and summarize the diff. |
| `--no-llm` | Skip LLM/Laya; use the rule-based generator only. |
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
   gen.Generate          → produces awk/sed/grep operations
        ├─ Laya pipeline  → tries the model Laya is configured with
        ├─ OpenAI-compatible / Ollama → fallback LLMs
        └─ rule-based     → deterministic fallback when no LLM is reachable
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

### Safety

- **Backups**: applying writes `<file>.bak` before mutating.
- **Atomic replace**: output is written to a temp file and renamed into place.
- **Denylist** (`internal/safety`): operations containing `:d`, `1,$d`,
  `1,$ s`, `rm -`, `> /dev/null`, `system(`, or `exit(1)` are rejected.
- **Preview first**: `edit` previews by default; use `-y` to apply.

## Architecture

| Package | Responsibility |
|---------|----------------|
| `cmd/aed` | CLI entrypoint |
| `internal/cli` | Flag parsing, the edit/grep command loop |
| `internal/plan` | Intent parsing + Laya context enrichment |
| `internal/gen` | Turns intent into awk/sed/grep operations (LLM + rule-based) |
| `internal/exec` | Safe `DryRun`/`RunInPlace` wrappers over awk/sed/grep, line search, diffing |
| `internal/safety` | Denylist validation |
| `internal/verify` | Post-apply asserts + diff summary + optional Laya review |
| `internal/laya` | Laya MCP client |
| `internal/llm` | Ollama / OpenAI-compatible HTTP clients |

## Tests

```sh
go test ./...
```
