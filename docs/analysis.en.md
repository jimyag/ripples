# Analysis

[简体中文](analysis.md) · [English](analysis.en.md)

This document explains how ripples calculates impact, which Go usage patterns are covered, and which cases cannot be resolved reliably through static analysis. See [Architecture](architecture.en.md) for source structure and algorithm details, and [Installation and Usage](usage.en.md) for setup and CLI details.

## How It Works

Given old and new revisions in the same repository, ripples:

1. Resolves each revision to a commit and Git tree without modifying the current working tree.
2. Exports both complete trees into temporary directories through a private index, preserving the repository-relative layout. No worktree is registered, no hook runs, and sparse-checkout settings do not apply.
3. Loads local package ASTs and type information under the effective Go build configuration, plus `_test.go` files with `-tests`.
4. Ignores comments and source positions while comparing the semantic content of functions, methods, types, variables, constants, embedded files, and other declarations; syntax expressed through position fields, such as the variadic spread in `f(xs...)` and the alias in `type A = B`, is still compared.
5. Merges the old and new declaration dependency graphs and walks reverse dependencies from each changed declaration.
6. Sorts and prints `<module-relative path>.<package name>`.

The package containing a changed declaration is always returned. Other packages are included only when their declarations reference affected content, convert an affected type to an interface, or instantiate a generic with it. Importing the same package alone is not enough.

Added declarations use the new dependency graph, while removed declarations use the old graph. Both changes therefore propagate through relationships that exist in the relevant revision.

## Supported Analysis

| Category | Supported changes and usage patterns |
| --- | --- |
| Declarations | Functions, methods, types, interface methods, struct fields, package variables, constants, and `init` |
| Change types | Additions, removals, and modifications; removals use the old dependency graph and additions use the new graph |
| Dependency propagation | Direct references, transitive references, function calls, method calls, and cross-package forwarding; fields and methods promoted through embedding also depend on the embedded fields on their path |
| Tests | `_test.go` files are not analyzed by default, so a commit that only changes tests reports no package. With `-tests`: `_test.go` files, external test packages (reported as the package under test), and packages that contain only tests; `init` functions in test files do not affect importers |
| Interfaces | A declaration that converts a concrete value to an interface depends on the type's contract: every method for conversions to `any` or `error`, only the methods that can be invoked dynamically for other interfaces, and in both cases the field layout and nested named types. A method called only through local interfaces affects only packages that reach both the conversion and a call. Constructors, setters, functional options, registries, embedded interface fields, and external parameters such as `fmt` and `encoding/json` all propagate |
| Function values | Referencing a function (as an argument, field, container element, return value, method value, or method expression) is a dependency on it |
| Generics | Generic functions and the methods and fields of generic types; calling a generic function or a method of an instantiated generic type depends on the contracts of its type arguments |
| Field layout | Unkeyed struct literals depend on field order, types, and tags |
| Initialization | Actual references to package variables, runtime-effectful named and blank initializers, multi-variable declarations, constant changes, added/removed/modified `init`, and cross-package initialization order; conversions are not function calls, but potentially panicking conversions and runtime effects in their operands are preserved |
| Build inputs | Build tags, filename build constraints, CGo preambles, `//go:` directives, `go:embed`, and non-Go sources such as assembly, C/C++, headers, and syso files |
| Modules and workspaces | Effective changes to `go.mod`, `go.sum`, the effective `go.work` (including one in a parent directory), `go.work.sum`, dependency versions, and `replace` directives |
| Output and reuse | simple, JSON, text, summary, DOT, and persistent snapshots keyed by Git tree and the effective build configuration |

### Package-Initialization Effect Classification

ripples uses a finite, explainable set of syntax rules to classify package-initialization effects. It does not perform function-purity, complete value-range, or complete panic analysis. An initializer is conservatively connected to package-init when it matches a runtime rule below; other expressions propagate only through actual declaration references.

| Syntax | Decision | Reason |
| --- | --- | --- |
| Explicit `init()` | Propagate | Go executes `init()` during package initialization; importers cannot bypass it |
| Local package import | Propagate the imported package-init | Preserves Go's cross-package initialization order |
| Regular function or builtin call | Propagate | ripples does not perform purity analysis; a call may modify state, block, or panic |
| Stored function literal | Do not propagate its body | Creating a function value does not execute its body; actual users still propagate through declaration references |
| Immediately invoked function literal | Propagate | The enclosing `CallExpr` executes the function body immediately |
| Channel receive | Propagate | `<-ch` receives during initialization and may block |
| Type conversion | Do not propagate the conversion itself by default | Go uses `CallExpr` for conversions too, but a conversion is not a function call; its operand is still inspected |
| Slice → non-empty array or array pointer | Propagate | The conversion panics at run time when the slice is too short |
| Literals, call-free composite literals, and ordinary operations | Do not propagate | The expression itself has none of the runtime effects above; nested calls and receives are still inspected |
| Other expressions whose panic depends on runtime values | Do not add a dedicated package-init edge | Index bounds, nil dereferences, and runtime division by zero are not currently inferred; declaration references and nested effects are still preserved |

Real calls in named and blank variables both propagate. The call runs when the package is imported even when no declaration uses its result:

```go
var Registry = sets.New("a", "b") // Conservatively treated as potentially effectful.
var _ = registerHandlers()         // The result is discarded, but the call still runs.
var Ready = <-readyCh              // Receives during initialization and may block.
```

Creating a function value does not execute its body, while invoking it immediately does:

```go
var Handler = func() { registerHandlers() } // Its body is not connected to package-init.
var _ = func() bool {                       // Invoked immediately; connected to package-init.
	registerHandlers()
	return true
}()
```

Go represents function calls and type conversions with the same `CallExpr` AST node. ripples uses `types.Info` to distinguish them, preventing a compile-time interface assertion with no runtime effect from becoming a package-initialization dependency:

```go
var _ API = (*Client)(nil) // Checks that *Client implements API; importers are unaffected.
```

After identifying a conversion, ripples still inspects its operand. A real call inside the conversion is executed and must propagate:

```go
var _ = ID(load()) // Changes to load affect every importer.
```

Converting a slice to an array or array pointer panics at run time when the slice is shorter than the array. The conversion itself is therefore an initialization-time runtime effect and must also propagate:

```go
var data = []byte{1}
var _ = [2]byte(data)    // Panics during package initialization.
var _ = (*[2]byte)(data) // Panics during package initialization.
```

Named variables are initialized whenever their package is imported. If an initializer contains one of these runtime effects, it is connected to package-init even when the variable is unused. An unreferenced initializer without these effects does not broaden the impact set:

```go
var Version = "v1"          // Does not affect importers when unused.
var Ports = []int{80, 443}  // Does not affect importers when it has no nested runtime effect.
```

This classification is not a complete Go panic analysis. Whether `items[index]`, `*pointer`, or `value/divisor` panics depends on runtime values, so ripples does not currently broaden the result to every importer solely because of these expressions. Their declaration references and any nested calls, channel receives, or slice conversions described above still propagate normally.

## Interfaces, Generics, and Function Values

Once a concrete value becomes an interface, its methods can run through dynamic dispatch, type assertions, or reflection wherever the interface value flows afterwards. ripples therefore does not track interface values. Instead, the declaration that performs the conversion depends on the type's contract. There are two kinds:

- Full contract: every method in the method set (including promoted methods), the field layout, and the full contracts of local named types used in fields, elements, and type arguments. Conversions to `any` use it, because `fmt`, `encoding/json`, templates, and similar code can reach any method or field through reflection. So do conversions to `error` (and interfaces containing `Error() string`): many libraries inspect errors through anonymous interfaces, such as `Cause()` in `pkg/errors` and `GRPCStatus()` in gRPC, which the exported type information of dependencies does not show.
- Dispatch contract: the field layout, the methods that can be invoked dynamically, and the dispatch contracts of nested named types. Conversions to other non-empty interfaces use it.

A call through an interface J only dispatches to types that implement J, so only interfaces the type implements count. A method can be invoked "anywhere" when:

- Local code checks an interface J containing the method with a type assertion or type switch, such as `v.(interface{ Flush() })`.
- An interface J exported by the standard library or a third-party dependency declares the method, such as `fmt.Stringer`, `json.Marshaler`, or `http.Flusher`, because the dependency's function bodies may call it.
- The method is named `Unwrap`, `Is`, `As`, `Timeout`, or `Temporary`, which the standard library checks through anonymous interfaces.

Such methods are part of the dispatch contract, and converting declarations depend on them directly. A method that only local code calls through interfaces (locally declared ones, anonymous ones such as a struct field of type `interface{ List() }`, or interfaces defined inside functions) needs both sides: a binary can only run it if it both converts the type to an interface and executes one of those calls. ripples records the declarations making the calls; package initialization and `main` are declarations of one binary that do not reference each other, so the two sides meet per package: when the method changes, only packages that have a declaration reaching the conversion and a declaration reaching a call are reported. A method declared in an interface that no code calls through an interface does not affect the converting code.

A call to a server interface method `OrderService.Cancel(ctx, id)` therefore does not make the client method `OrderClient.Cancel` with the same signature reachable, as long as `OrderClient` does not implement `OrderService`. Methods of generic types and interfaces that mention type parameters cannot be checked before instantiation, so they match by method name.

Other rules:

- Conversion sites come from the official `golang.org/x/tools/go/ssa` package. SSA makes every implicit conversion explicit as `MakeInterface`, covering assignments, call arguments, returns, composite literals, channel sends, map writes, and package variable initializers.
- Calling a generic function, or a method of an instantiated generic type, depends on the full contracts of the type arguments, because generic code calls their methods through the type parameters. Merely naming an instantiated type in a signature or variable type (such as `Page[Order]`) runs no generic code and adds no such dependency.
- Function values are not tracked: referencing a function is a dependency on it.

For example, several services obtain the same client from `orders.New()`, and only the reporting service calls `Export`:

```go
// The orders package adds or changes Client.Export.
// While no code calls Export through an interface, only orders is reported;
// cmd/reporting calls api.Export(...), reaches both the conversion and a
// call, and is reported; cmd/billing, which only calls api.Get(), is not.
func New() API { return &Client{} }
```

Impact lands on code that contains both the conversion and a call, not on code that only calls through the interface or only converts. For example:

```go
// main injects FileStore into service and runs it. When FileStore.Save
// changes, main is affected; the service package, which only calls
// s.store.Save(), and a factory package that only builds FileStore are not.
func main() { service.New(store.FileStore{}).Run() }
```

Compared with the earlier value-flow analysis, this is:

- More conservative: two unrelated pieces of code in one package that reach the conversion and a call separately still meet; referencing a function value counts even if it is never called.
- More complete: setters, functional options, local registries, registration during package initialization, embedded interface fields, `fmt.Stringer`, `json.Marshaler`, and other dynamic calls all propagate, and analysis time grows linearly with the code.

Conversion sites stay independent: when two binaries inject different implementations of the same interface, changing one implementation affects only the binary that injects it.

## Build and Module Changes

- Analysis follows the effective `GOOS`, `GOARCH`, and build tags, including settings written with `go env -w`. Run ripples separately for every build configuration that needs coverage.
- CGo preambles and `//go:` compiler directives participate in semantic comparison. Declaration-level directives propagate through actual users; linker-level directives conservatively affect the package.
- When non-Go sources such as assembly, C/C++, headers, or syso files change, declarations without a Go body (implemented in assembly) and declarations from cgo-processed files propagate to their callers.
- Effective `go.mod` and `go.work` build configuration changes affect the relevant build. `go.work` is located the way the go command locates it and may live in a parent directory of `-repo`.
- Dependency version or `replace` changes affect only local packages that transitively use the relevant module.
- Adding or removing ordinary cache entries in `go.sum` or `go.work.sum` has no impact. A checksum change for the same module version propagates to actual users.

## Explicit Boundaries

- The standard library and third-party dependencies from `go.mod` are treated as black boxes; their function bodies are not traversed.
- Temporary exports preserve same-repository local `replace` directories so nested modules load correctly. The declaration graph still covers only the module selected by `-repo`; modifying another local replacement module in the same commit does not yet propagate across modules.
- Reflection and anonymous interface checks on values converted to `any` or `error` are covered by the full contract. When a value is first converted to another non-empty interface and then has a method called by name through reflection (for example, an interface field rendered by a template as `{{.Method}}`), or a dependency checks a method outside the list above through an unexported or anonymous interface, that method is not part of the dispatch contract.
- `unsafe`, plugins, `//go:linkname`, and behavior determined only by external configuration cannot be derived from Go source.
- Test files are not analyzed by default; pass `-tests` when test jobs are selected by impact.
- Generated code that the repository does not commit must be generated in the export with `-prepare`; otherwise loading fails.
- DOT relationship graphs contain package nodes only, not functions, fields, or other declarations.
- Output represents Go package impact only. Consumers map packages to binaries, services, labels, or deployment units.
