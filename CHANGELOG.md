# Changelog

## 0.4.0

- Native in-process edit engine (default), preserving BOM, CRLF, and the final
  newline; `--engine=sed` keeps the compatibility backend.
- Strict typed IR and digest-bound `aed plan` / `aed apply`; apply revalidates
  the digest, hashes, policy, and syntax before writing.
- Transactions now cover create/delete/rename/chmod as well as updates.
- Hash-chained audit log, before-content journal, `aed log --verify`,
  `aed undo`, and `aed recover`.
- Read-only-by-default MCP stdio server (`aed mcp`) with plan/apply/undo/recover.
- Shared policy layer: `.aedignore`, protected paths, blast-radius limits.
- Laya evaluation/calibration harness (`aed laya-eval`) and seed corpus.
- Single shared version constant across CLI, MCP, and the Laya client.
- Requires Go 1.26.9; CI runs govulncheck.

## 0.3.0

- Root-confined, byte-exact all-or-nothing transactions.
- Fail-closed Laya edit-risk gate, JSON output, protected paths, exit codes
  5/6/8, and the first security/audit hardening pass.

## 0.2.0

- Deterministic typed-op compiler, secret scrubber, post-edit guard, and
  pluggable per-language syntax gate.
