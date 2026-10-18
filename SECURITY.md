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
- Paths are confined to `--root`; symlinks, hardlinks, and non-regular files are
  refused. Protected paths (VCS internals, CI, keys, `.env`, shell rc, build
  files) require `--allow-protected`.
- Secrets are redacted from model prompts and output unless `--show-secrets` is
  set.
- Laya is advisory. It can escalate a decision, never grant one that
  deterministic policy or the syntax gate denies.

Non-goals:

- Preventing a user with legitimate write access from making a bad semantic
  change.
- Guaranteeing that generated code is free of vulnerabilities. Use tests, code
  review, and language-specific linters in addition to `aed`.
- Sandboxing arbitrary commands. `aed` does not execute model-provided shell.

## Hardening checklist for agent deployments

- Run with an explicit `--root` and a dedicated capability profile.
- Keep `--laya-safety=block` fail-closed (exit `8` when the checker is down).
- Deny network egress for the agent and the local model.
- Never expose `--allow-protected` to an untrusted agent.
- Keep `--json` output and audit logs; scrub them before sharing.
- Pin the Laya checkpoint and validate it on held-out repositories before
  trusting its thresholds.
