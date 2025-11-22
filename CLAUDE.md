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
- **Constants**: Full support via `golang.References` API (added 2025-11-22)
- **Global Variables**: Full support via `golang.References` API (added 2025-11-22)
- **init Functions**: Full support via workspace package import analysis (added 2025-11-22)
- **Blank Imports (_ import)**: Full support via workspace package import analysis (added 2025-11-22)

### Not Currently Supported
- Type changes (struct field additions/removals)
- Struct field changes
- Reflection-based calls (`reflect.Call`)
- CGO calls
- Interface method changes (may report inaccurately)
- Callback functions (GORM hooks, HTTP handlers)

See [internal/analyzer/lsp_analyzer.go](internal/analyzer/lsp_analyzer.go) for the `isSupportedSymbolKind` function that defines supported types.

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

## Development Guidelines

### Test-Driven Development (CRITICAL)

**MANDATORY**: Every new feature or bug fix MUST include corresponding unit tests.

#### Testing Requirements

1. **Write Tests First or Immediately After Implementation**
   - DO NOT consider a feature complete without tests
   - Tests should verify the functionality works as expected
   - Tests should cover both happy path and edge cases

2. **Test Location and Naming**
   - Place tests in the same package as the code being tested
   - Name test files with `_test.go` suffix (e.g., `lsp_analyzer_test.go`)
   - Name test functions with `Test` prefix (e.g., `TestTraceConstantToMain`)

3. **Test Quality Standards**
   - Tests must be deterministic (not flaky)
   - Tests should use `testdata/` directory for test fixtures
   - Tests should clean up resources (use `defer tracer.Close()`)
   - Tests should provide clear failure messages
   - **NEVER initialize git repositories inside testdata directories** - testdata should be part of the main repository

4. **Running Tests**
   ```bash
   # Run all tests
   go test ./...

   # Run specific test
   go test -v ./internal/analyzer -run TestTraceConstantToMain

   # Run with coverage
   go test -cover ./...
   ```

5. **Example Test Structure**
   ```go
   func TestFeatureName(t *testing.T) {
       // Setup
       ctx := context.Background()
       tracer, err := createTracer(ctx)
       if err != nil {
           t.Fatalf("Setup failed: %v", err)
       }
       defer tracer.Close()

       // Execute
       result, err := tracer.SomeMethod(input)

       // Verify
       if err != nil {
           t.Errorf("Expected no error, got %v", err)
       }
       if result != expected {
           t.Errorf("Expected %v, got %v", expected, result)
       }
   }
   ```

### Adding Support for New Symbol Types

When adding support for a new symbol type (e.g., struct fields, init functions):

1. **Design the approach**
   - Document the approach in `docs/EXTENDED_SUPPORT.md`
   - Consider using `golang.References` for non-function symbols
   - For init functions, analyze package import relationships

2. **Implement in golang-tools fork**
   - Add necessary methods to `gopls/internal/ripplesapi/tracer.go`
   - Export types/methods via `gopls/pkg/ripplesapi/api.go`
   - Commit changes with descriptive message

3. **Integrate in ripples**
   - Update `internal/lsp/direct_tracer.go` to handle new symbol kind
   - Update `isSupportedSymbolKind` in `internal/analyzer/lsp_analyzer.go`
   - Add the symbol kind to the switch statement in `TraceToMain`

4. **Write comprehensive tests (REQUIRED)**
   - Create test fixtures in `testdata/`
   - Write unit tests in `internal/analyzer/*_test.go`
   - Verify tests pass: `go test -v ./internal/analyzer`
   - Example: See `constant_trace_test.go` for reference

5. **Document the feature**
   - Update README.md to reflect new capabilities
   - Update CLAUDE.md (this file) in "Supported" section
   - Add usage examples if applicable

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

## Recent Changes and Extensions

### Constant and Variable Tracking (2025-11-22)

Added support for tracking constants and global variables using gopls References API.

**Implementation**:
- Added `FindReferences`, `TraceReferencesToMain`, `findContainingFunction`, and `findFunctionDeclaration` methods to `golang-tools/gopls/internal/ripplesapi/tracer.go`
- Updated `DirectCallTracer.TraceToMain` to handle `SymbolKindConstant` and `SymbolKindVariable`
- Created comprehensive test suite in `internal/analyzer/constant_trace_test.go`

**How it works**:
1. Find all references to the constant/variable
2. For each reference, find the containing function
3. Trace each containing function to main
4. Deduplicate and return affected services

**Tests**: All 4 tests passing in `internal/analyzer/constant_trace_test.go`
- `TestTraceConstantToMain` - Verifies constant tracking
- `TestTraceVariableToMain` - Verifies variable tracking
- `TestTraceFunctionToMain` - Verifies existing function tracking still works
- `TestIsSupportedSymbolKind` - Verifies symbol kind filtering

**Documentation**: See `docs/CONSTANT_VARIABLE_SUPPORT.md` for detailed implementation notes.

### init Function Tracking (2025-11-22)

Added support for tracking `init` functions using workspace-level package import analysis.

**Implementation**:
- Added `FindMainPackagesImporting` method to `golang-tools/gopls/internal/ripplesapi/tracer.go`
- Uses `snapshot.LoadMetadataGraph()` to load all workspace packages
- Recursively checks package dependencies to find all main packages that import the target package
- Updated `DirectCallTracer.TraceToMain` to handle `SymbolKindInit`
- Created comprehensive test suite in `internal/analyzer/init_trace_test.go`

**How it works**:
1. When an `init` function changes, get its package path
2. Load complete workspace metadata graph using gopls
3. Find all main packages in the workspace
4. For each main package, recursively check if it imports the target package
5. Return all main packages that import the changed package (directly or indirectly)

**Key insight**: init functions run automatically when a package is imported, so any main package that imports the package (even transitively) is affected.

**Test project structure** (`testdata/init-test`):
- Multiple services: `server`, `api-server`, `worker`
- Shared packages: `pkg/config`, `internal/db`, `internal/cache`, `internal/logger`
- Different import relationships to test various scenarios

**Tests**: All tests passing in `internal/analyzer/init_trace_test.go`
- `TestTraceInitToMain/config.init` - Verifies config package affects all 3 services
- `TestTraceInitToMain/db.init` - Verifies db package affects all 3 services
- `TestTraceInitToMain/cache.init` - Verifies cache affects only server and api-server
- `TestTraceInitToMain/logger.init` - Verifies logger affects only api-server
- `TestIsSupportedSymbolKindInit` - Verifies init is supported

**Performance**: Uses gopls's cached metadata graph for efficient lookup across large codebases.

### Blank Import Tracking (2025-11-22)

Added support for tracking blank imports (`_ import`) which are commonly used to trigger init functions.

**Implementation**:
- Reuses `FindMainPackagesImporting` from init function tracking
- Added blank import handling in `internal/lsp/direct_tracer.go`
- Validates that imports are blank (alias == "_") before tracing
- Created comprehensive test suite in `internal/analyzer/blank_import_test.go`

**How it works**:
1. Detect blank import change (e.g., `_ "database/sql/driver"`)
2. Extract the imported package path from `ImportExtra`
3. Verify it's a blank import (not a normal import)
4. Use `FindMainPackagesImporting` to find all main packages that import it
5. Return affected services

**Key insight**: Blank imports are primarily used to trigger side effects (usually init functions), so tracking them is equivalent to tracking which main packages import the target package.

**Common use cases**:
- Database driver registration: `_ "github.com/lib/pq"`
- Plugin registration
- Side-effect initialization

**Tests**: All tests passing in `internal/analyzer/blank_import_test.go`
- `TestTraceBlankImportToMain` - Verifies blank import tracking for multiple packages
- `TestBlankImportVsNormalImport` - Verifies only blank imports are traced (normal imports rejected)
- `TestIsSupportedSymbolKindImport` - Verifies import symbol kind support

**Note**: Only blank imports (`_`) are tracked. Normal imports are not tracked as they don't affect runtime behavior by themselves.
