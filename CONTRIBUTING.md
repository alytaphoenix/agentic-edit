# Contributing

## Build and test

```sh
go build -o aed ./cmd/aed
go test ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go test -race ./...
go vet ./...
```

The project deliberately has no third-party runtime dependencies. Please keep it
that way unless a dependency is necessary and well-vetted.

## Adding a language checker

Implement `lang.Checker` or adapt an existing CLI with `lang.CommandChecker` and
call `lang.Register` from an `init()`. See `internal/lang/bash.go` and the
README table.

## Safety invariants

Changes must preserve these invariants:

1. An operation can only be one of the typed shapes in `internal/ir` or
   `internal/compile`; model output never becomes a raw script. The native
   engine is the default; `--engine=sed` is a compatibility backend.
2. A write is the exact byte sequence that was previewed and checked.
3. A transaction is all-or-nothing and re-hashes targets immediately before
   rename. Create/delete/rename/chmod are part of the same transaction.
4. No predictable sibling temp/backup path is ever opened.
5. A target symlink, hardlink, device, or non-regular file is refused, and all
   filesystem access goes through `os.Root` so a symlinked parent cannot escape
   the workspace.
6. A stale target is a conflict, never an overwrite.
7. The syntax gate runs before the write; verification runs after it.
8. Laya may only escalate a decision. In `block` mode, `review`, unknown risks,
   and missing confidence all deny; a down checker fails closed.
9. Regex replaces are counted with the same semantics they are executed with,
   and are unique by default like literal replaces.
10. A plan digest binds ops, paths, hashes, modes, and root; apply must refuse a
    digest or hash mismatch with exit `6`.
11. Every apply leaves a before-content journal and a hash-chained audit record;
    undo must refuse when a file no longer matches its after-hash.

Add a regression test with every safety fix. For filesystem behavior, test
symlink swaps, concurrent modification, CRLF/BOM, and no-final-newline files.
