# CLAUDE.md

## Project

ripples compares two immutable Git revisions and returns affected Go packages as `<relative-path>.<package-name>`.

## Architecture

```text
Git ref
  -> internal/snapshot: resolve tree, extract archive, persistent cache
  -> internal/impact: AST package digest + old/new reverse import graph
  -> internal/output: simple/json/text/summary
  -> main.go: CLI
```

Important invariants:

- Never checkout or mutate the analyzed repository.
- Analyze old and new revisions independently.
- Detect source changes from AST structure, not diff line numbers.
- Ignore comments and token positions in AST hashes.
- Use both old and new import graphs so deleted packages retain their old importers.
- Deduplicate by full package path and sort output deterministically.
- Treat analysis errors as command failures; never return partial results as complete.
- Cache keys must include the Git tree, tool version and build configuration.

## Commands

```bash
go build ./...
go test ./...
```

Every behavior change requires a focused test. Prefer temporary Git modules in tests; do not initialize repositories inside committed `testdata`.
