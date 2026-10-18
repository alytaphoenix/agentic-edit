# Security policy

`aed` edits files on behalf of an agent. Treat every model output and every
repository file as untrusted input.

## Reporting

Please report vulnerabilities privately to the maintainers rather than opening a
public issue. Include a minimal reproduction, the `aed version`, the platform,
and whether `--llm` / `--laya-safety` were enabled.

## Security model

Guarantees:

- Edits are rendered from a fixed typed-op whitelist. Free-form shell scripts
  are not accepted.
- Writes are byte-exact and all-or-nothing: a transaction stages exact bytes,
  fsyncs, re-hashes the target immediately before an atomic rename, and restores
  every file on failure.
- Paths are confined to `--root` and every filesystem operation goes through
  `os.Root`, so a symlinked parent swapped in after a check cannot escape the
  tree. Symlinks, hardlinks, and non-regular files are refused. Protected paths
  (VCS internals, CI, keys, `.env`, shell rc, build files) require
  `--allow-protected`.
- Secrets are redacted from model prompts, `grep` output, and stdout diffs
  unless `--show-secrets` is set.
- Laya can escalate a decision, never grant one that deterministic policy or the
  syntax gate denies. In `block` mode a `review`, an unknown risk value, or a
  missing confidence is a denial; a down checker exits `8`.

Non-goals:

- Preventing a user with legitimate write access from making a bad semantic
  change.
- Guaranteeing that generated code is free of vulnerabilities. Use tests, code
  review, and language-specific linters in addition to `aed`.
- Sandboxing arbitrary commands. `aed` does not execute model-provided shell.

## Toolchain

Build with **Go 1.26.9 or newer**. The module declares that minimum because
earlier 1.26 patch releases contained `os.Root` and `net/http` vulnerabilities
that are reachable from this code. CI runs `govulncheck ./...`; run it yourself
after any dependency change. `aed` has no third-party runtime dependencies.

## Hardening checklist for agent deployments

- Run with an explicit `--root` and a dedicated capability profile.
- Keep `--laya-safety=block`: it denies `review` as well as `block`, and exits
  `8` when the checker is down.
- Deny network egress for the agent and the local model.
- Never expose `--allow-protected` to an untrusted agent.
- Keep `--json` output and audit logs; scrub them before sharing.
- Pin the Laya checkpoint and validate it on held-out repositories before
  trusting its thresholds.
