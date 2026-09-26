# CLAUDE.md

## Project

ripples compares two immutable Git revisions and returns affected Go packages as `<relative-path>.<package-name>`.

## Architecture

```text
Git ref
  -> internal/snapshot: resolve tree, export it through a private index, persistent cache
  -> internal/impact: AST/type declaration digest (tests with -tests) + SSA interface conversions + old/new dependency graph
  -> internal/output: simple/json/text/summary/dot
  -> main.go: CLI
```

Important invariants:

- Never checkout or mutate the analyzed repository: no worktrees, no hooks, no index changes.
- Analyze old and new revisions independently.
- Detect source changes from AST structure, not diff line numbers.
- Ignore comments and token positions in AST hashes.
- Propagate through static references, embedded-field paths and type contracts: calling generic code or converting a value to `any` or `error` depends on all of the type's methods and its layout; converting to another interface depends only on methods some code can invoke dynamically through an interface the type implements. A method called only through local interfaces affects only packages that reach both the conversion and a call. Do not reintroduce value-flow tracking.
- Use both old and new dependency graphs so deleted declarations retain their old users.
- Preserve package initialization edges without treating every import as a use; test-only initialization never reaches importers.
- Deduplicate by full package path and sort output deterministically.
- Treat analysis errors as command failures; never return partial results as complete.
- Cache keys must include the Git tree, tool version, effective `go env` build configuration, the `-prepare` command and `-tests`.
- Prefer the standard library, the go command, git plumbing and golang.org/x/tools over hand-written analysis.

## Commands

```bash
task ci
```

Every behavior change requires a focused test. Prefer temporary Git modules in tests; do not initialize repositories inside committed `testdata`.
