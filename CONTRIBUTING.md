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

1. A rendered operation can only be one of the typed shapes in
   `internal/compile`; model output never becomes a raw script.
2. A write is the exact byte sequence that was previewed and checked.
3. A transaction is all-or-nothing and re-hashes targets immediately before
   rename.
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

Add a regression test with every safety fix. For filesystem behavior, test
symlink swaps, concurrent modification, CRLF/BOM, and no-final-newline files.
