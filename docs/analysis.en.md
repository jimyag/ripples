# Analysis

[简体中文](analysis.md) · [English](analysis.en.md)

This document explains how ripples calculates impact, which Go usage patterns are covered, and which cases cannot be resolved reliably through static analysis. See [Architecture](architecture.en.md) for source structure and algorithm details, and [Installation and Usage](usage.en.md) for setup and CLI details.

## How It Works

Given old and new revisions in the same repository, ripples:

1. Resolves each revision to a commit and Git tree without modifying the current working tree.
2. Creates temporary detached worktrees for both trees while preserving the complete repository-relative directory layout.
3. Loads local package ASTs and type information under the current Go build configuration.
4. Ignores comments and source positions while comparing the semantic content of functions, methods, types, variables, constants, embedded files, and other declarations.
5. Merges the old and new declaration dependency graphs and walks reverse dependencies from each changed declaration.
6. Sorts and prints `<module-relative path>.<package name>`.

The package containing a changed declaration is always returned. Other packages are included only when their declarations reference or call affected content. Importing the same package alone is not enough.

Added declarations use the new dependency graph, while removed declarations use the old graph. Both changes therefore propagate through relationships that exist in the relevant revision.

## Supported Analysis

| Category | Supported changes and usage patterns |
| --- | --- |
| Declarations | Functions, methods, types, interface methods, struct fields, package variables, constants, and `init` |
| Change types | Additions, removals, and modifications; removals use the old dependency graph and additions use the new graph |
| Dependency propagation | Direct references, transitive references, function calls, method calls, and cross-package forwarding |
| Interface calls | Interface parameters, returns, fields, type assertions, type switches, and concrete implementations that can be resolved at the call site |
| Function values | Parameters, returns, multiple returns, closure captures, function conversions, method values, method expressions, and indirect pointer assignments |
| Construction and containers | Constructor struct fields, slices, arrays, maps, channels, ranges, `append`, container returns, and statically known indexes or keys |
| Call syntax | Regular calls, `go`, `defer`, variadic calls, generic functions, and generic forwarding |
| Initialization | Package variable initialization, multi-variable declarations, constant changes, added/removed/modified `init`, and cross-package initialization order |
| Build inputs | Build tags, filename build constraints, CGo preambles, `//go:` directives, `go:embed`, and other compiler inputs |
| Modules and workspaces | Effective changes to `go.mod`, `go.sum`, `go.work`, `go.work.sum`, dependency versions, and `replace` directives |
| Output and reuse | simple, JSON, text, summary, DOT, and persistent snapshots keyed by Git tree and build configuration |

## Interface and Function Value Propagation

- Interface parameters and fields, variable assignments, factories and multiple returns, closures, generic forwarding, type assertions and type switches, method values, and method expressions participate in value-flow analysis.
- Concrete implementations stored in slices, arrays, maps, channels, ranges, and `append` propagate when they can be resolved from AST and type information.
- When one static location may hold multiple runtime values, every candidate is conservatively included.
- When a local concrete value is passed to an external interface, propagation continues through the interface method contract, but third-party function bodies are not traversed.

Interface implementation resolution is based on call sites and value flow. Changing one concrete implementation does not include other implementations and their callers merely because they satisfy the same interface.

## Build and Module Changes

- Analysis follows the current `GOOS`, `GOARCH`, and build tags. Run ripples separately for every build configuration that needs coverage.
- CGo preambles and `//go:` compiler directives participate in semantic comparison. Declaration-level directives propagate through actual users; linker-level directives conservatively affect the package.
- Effective `go.mod` and `go.work` build configuration changes affect the relevant build.
- Dependency version or `replace` changes affect only local packages that transitively use the relevant module.
- Adding or removing ordinary cache entries in `go.sum` or `go.work.sum` has no impact. A checksum change for the same module version propagates to actual users.

## Explicit Boundaries

- `_test.go` files are not analyzed by default.
- The standard library and third-party dependencies from `go.mod` are treated as black boxes; their function bodies are not traversed.
- Temporary worktrees preserve same-repository local `replace` directories so nested modules load correctly. The declaration graph still covers only the module selected by `-repo`; modifying another local replacement module in the same commit does not yet propagate across modules.
- Reflection, `unsafe`, plugins, runtime registration, and calls determined only by external configuration cannot be resolved completely from the Go AST. ripples does not invent call relationships without static evidence.
- DOT relationship graphs contain package nodes only, not functions, fields, or other declarations.
- Output represents Go package impact only. Consumers map packages to binaries, services, labels, or deployment units.
