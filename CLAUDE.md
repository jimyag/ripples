# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

ripples is a Go code change impact analysis tool that uses gopls internal APIs directly (zero IPC overhead) to trace function call chains and determine which services are affected by Git commit changes.

**Key insight**: This project embeds gopls as a library rather than communicating via LSP protocol, achieving significant performance gains by eliminating serialization/IPC overhead.

## Build and Development Commands

### Building
```bash
# Build the main binary (~20MB, includes full gopls analysis engine)
go build -o ripples main.go

# Or simply
go build -o ripples
```

### Testing
```bash
# Run all unit tests
go test ./...

# Run integration tests with testdata
./ripples -repo testdata -old <commit> -new <commit>
```

### Running
```bash
# Basic usage
./ripples -repo <path> -old <old-commit> -new <new-commit>

# With different output formats
./ripples -repo . -old HEAD~1 -new HEAD -output json
./ripples -repo . -old main -new develop -output summary

# Verbose logging for debugging
./ripples -repo . -old abc123 -new def456 -verbose
```

## Architecture

### Critical Dependency Setup

**IMPORTANT**: This project uses a forked/modified version of `golang.org/x/tools` to access gopls internal packages.

The `go.mod` uses `replace` directives to point to a remote repository:
```go
replace golang.org/x/tools => github.com/jimyag/golang-tools v0.0.0-...
replace golang.org/x/tools/gopls => github.com/jimyag/golang-tools/gopls v0.0.0-...
```

**Why this is necessary**: Go's `internal` package restriction prevents external imports. The fork adds a public API wrapper (`pkg/ripplesapi/`) that re-exports functionality from `internal/ripplesapi/`, which directly uses gopls internals.

### Analysis Flow

The tool follows a 5-stage pipeline:

1. **Git Diff Parsing** ([internal/git/diff.go](internal/git/diff.go))
   - Parses diff between two commits
   - Extracts changed files and line ranges

2. **AST Symbol Extraction** ([internal/parser/ast_parser.go](internal/parser/ast_parser.go))
   - Uses `go/packages` to load entire project
   - Uses `go/ast` to parse source code
   - Matches changed line numbers to specific function symbols

3. **gopls Initialization** ([internal/lsp/direct_tracer.go](internal/lsp/direct_tracer.go))
   - Creates gopls Cache + Session + View
   - Obtains immutable project Snapshot
   - Snapshot contains complete type information for analysis

4. **Call Chain Tracing** ([internal/analyzer/lsp_analyzer.go](internal/analyzer/lsp_analyzer.go))
   - For each changed function, calls `golang.PrepareCallHierarchy`
   - Recursively calls `golang.IncomingCalls` to find callers
   - Filters cross-service false positives
   - Traces upward until reaching `main` functions

5. **Result Output** ([internal/output/reporter.go](internal/output/reporter.go))
   - Aggregates all call chains
   - Deduplicates (same service reported once)
   - Formats as text/JSON/summary

### Key Integration Points

**gopls API Wrapper** ([internal/lsp/direct_tracer.go](internal/lsp/direct_tracer.go)):
- `DirectCallTracer` wraps `ripplesapi.DirectTracer`
- Converts between internal types and `parser.Symbol`
- Main method: `TraceToMain(symbol *parser.Symbol) ([]CallPath, error)`

**The Bridge**: `golang.org/x/tools/gopls/pkg/ripplesapi`
- This package lives in the forked golang-tools repository
- Re-exports internal gopls functionality
- Key API: `DirectTracer.TraceToMain(pos Position, funcName string)`

### Cross-Service Call Filtering

The tool implements intelligent service boundary detection to avoid false positives:

- Recognizes `cmd/` and `internal/` as service boundaries
- Filters calls between different services (e.g., `cmd/service1` → `cmd/service2`)
- Preserves calls through shared packages (`pkg/`, `common/`)

This prevents reporting Service B as affected when Service A merely references a shared interface that Service B also implements.

## Symbol Types and Limitations

### Supported
- **Functions**: Full support via `golang.PrepareCallHierarchy`
- **Methods**: Full support (including receiver types)

### Not Currently Supported
- Type changes (struct field additions/removals)
- Constant/variable changes
- Reflection-based calls (`reflect.Call`)
- CGO calls
- Interface method changes (may report inaccurately)
- Callback functions (GORM hooks, HTTP handlers)

The analyzer explicitly skips non-function symbols in [internal/analyzer/lsp_analyzer.go:43](internal/analyzer/lsp_analyzer.go#L43).

## Module Structure

```
internal/
├── parser/          # AST parsing via go/packages + go/ast
│   ├── ast_parser.go    # Loads project, extracts symbols from files
│   └── symbol.go        # Symbol type definitions
├── git/             # Git diff parsing
│   └── diff.go
├── lsp/             # gopls integration layer
│   ├── direct_tracer.go # Wraps ripplesapi.DirectTracer
│   └── types.go         # CallPath, CallNode definitions
├── analyzer/        # Core analysis logic
│   ├── lsp_analyzer.go      # Main analyzer using DirectCallTracer
│   ├── change_detector.go   # Detects changed symbols from git diff
│   └── impact.go            # AffectedBinary result types
└── output/          # Output formatting
    └── reporter.go      # Text/JSON/summary formatters
```

## Performance Characteristics

**Typical Analysis Times**:
- Small projects (<10K LOC): <5 seconds
- Medium projects (10K-100K LOC): 5-30 seconds
- Large projects (>100K LOC): 30 seconds - 2 minutes

Performance depends on:
- Number of changed functions (not total LOC)
- Call chain depth and breadth
- Project's dependency complexity

**Why it's fast**:
- Zero serialization (shared memory with gopls)
- Incremental analysis (only changed symbols)
- Deduplication (same call chain traced once)
- Reusable Snapshot (immutable, cached type info)

## Common Development Patterns

### Adding Support for New Symbol Types

To support type/constant changes:
1. Remove the kind filter in [internal/analyzer/lsp_analyzer.go:43](internal/analyzer/lsp_analyzer.go#L43)
2. Implement alternative tracing logic (LSP doesn't provide call hierarchy for non-functions)
3. Consider using `golang.References` or `golang.Implementation` APIs instead

### Debugging Analysis Issues

When functions aren't found or traced:
1. Enable `-verbose` flag to see detailed logging
2. Check if symbol position is accurate (parser might mismatch line numbers)
3. Verify gopls can analyze the project (`gopls check <file>`)
4. Look for private/unexported functions (position detection may fail)

### Modifying Call Filtering Logic

Cross-service filtering happens in the forked `golang-tools` repository, likely in `gopls/internal/ripplesapi/tracer.go`. To adjust:
1. Modify the filtering logic in the golang-tools fork
2. Update the replace directive to point to new commit
3. Run `go mod tidy` to update pseudo-version

## Output Format Details

### Text Format
Human-readable with call chains from main → changed function, annotated with "(main)" and "(Changed)"

### JSON Format
Machine-parsable array of objects with `name`, `package`, and `trace_path` fields

### Summary Format
Minimal output showing only affected service names (useful for CI/CD pipelines)

## Dependencies Note

**Go Version**: Requires Go 1.25+ (uses latest language features)

**Critical External Dependency**: The forked `github.com/jimyag/golang-tools` must remain compatible with this codebase. Any gopls API changes require updating both repositories in sync.
