# CLAUDE.md

## Project

ripples compares two immutable Git revisions and returns affected Go packages as `<relative-path>.<package-name>`.

## Architecture

```text
Git ref
  -> internal/snapshot: resolve tree, extract archive, persistent cache
  -> internal/impact: AST/type declaration digest + old/new dependency graph
  -> internal/output: simple/json/text/summary
  -> main.go: CLI
```

Important invariants:

- Never checkout or mutate the analyzed repository.
- Analyze old and new revisions independently.
- Detect source changes from AST structure, not diff line numbers.
- Ignore comments and token positions in AST hashes.
- Propagate only through declarations and members that statically use changed content.
- Use both old and new dependency graphs so deleted declarations retain their old users.
- Preserve package initialization edges without treating every import as a use.
- Deduplicate by full package path and sort output deterministically.
- Treat analysis errors as command failures; never return partial results as complete.
- Cache keys must include the Git tree, tool version and build configuration.

## Commands

```bash
task ci
```

Every behavior change requires a focused test. Prefer temporary Git modules in tests; do not initialize repositories inside committed `testdata`.
