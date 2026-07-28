<h1 align="center">ripples</h1>

<p align="center">
  <strong>Find the Go packages affected by a Git change.</strong>
</p>

<p align="center">
  <a href="https://github.com/jimyag/ripples/actions/workflows/check.yaml"><img src="https://github.com/jimyag/ripples/actions/workflows/check.yaml/badge.svg" alt="Check"></a>
  <a href="https://github.com/jimyag/ripples/actions/workflows/release.yaml"><img src="https://github.com/jimyag/ripples/actions/workflows/release.yaml/badge.svg" alt="Release"></a>
</p>

<p align="center">
  <a href="#installation">Installation</a> ·
  <a href="#quick-start">Quick Start</a> ·
  <a href="#supported-analysis">Supported Analysis</a> ·
  <a href="#github-actions">GitHub Actions</a>
</p>

<p align="center">
  <a href="./README.md">简体中文</a> · <strong>English</strong>
</p>

---

ripples uses the Go AST, type information, and a declaration dependency graph to find Go packages that are directly or transitively affected between two Git revisions. It follows actual references to changed declarations instead of returning every package that imports a changed package.

The stable output contract is a list of packages. CI workflows can map those packages to binaries, services, build jobs, or deployment units:

```text
cmd/server.main
internal/order.order
payment.payment
```

## Core Capabilities

- **Declaration-level impact analysis**: detects added, removed, and modified functions, methods, types, fields, variables, constants, and `init` functions.
- **Direct and transitive propagation**: walks actual declaration references and call relationships in reverse to find every affected package.
- **Interface implementation resolution**: resolves concrete implementations from call sites and value flow without mixing unrelated implementations of the same interface.
- **Function values and value flow**: follows parameters, return values, closures, method values, struct fields, containers, multiple returns, type assertions, and generic forwarding.
- **Build input awareness**: detects effective changes to build tags, CGo, `//go:` directives, `go:embed`, `go.mod`, and `go.work`.
- **CI-friendly output**: emits stable, sorted package lists with persistent caching, JSON, summaries, and DOT relationship graphs.

## Installation

Choose either method below, then run `ripples --version` to confirm the installation.

### Install with Go

If a Go toolchain is already available, this is the shortest installation path:

```bash
go install github.com/jimyag/ripples@latest
ripples --version
```

The binary is installed into `$(go env GOPATH)/bin`. Add that directory to `PATH` if your shell cannot find `ripples`.

### Download the Latest Binary

[GitHub Releases](https://github.com/jimyag/ripples/releases/latest) provides raw amd64 and arm64 binaries for Linux, macOS, and Windows. Building ripples locally is not required.

On macOS and Linux, the following commands automatically select the current platform:

```bash
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) asset="ripples_darwin_arm64" ;;
  Darwin-x86_64) asset="ripples_darwin_amd64" ;;
  Linux-aarch64 | Linux-arm64) asset="ripples_linux_arm64" ;;
  Linux-x86_64) asset="ripples_linux_amd64" ;;
  *) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

download_dir="$(mktemp -d)"
trap 'rm -rf "$download_dir"' EXIT
gh release download \
  --repo jimyag/ripples \
  --pattern "$asset" \
  --dir "$download_dir"
mkdir -p "$HOME/.local/bin"
install -m 0755 "$download_dir/$asset" "$HOME/.local/bin/ripples"

"$HOME/.local/bin/ripples" --version
```

This example requires the [GitHub CLI](https://cli.github.com/) and always downloads the latest release. Make sure `$HOME/.local/bin` is in `PATH`.

The binaries can also be downloaded manually:

| OS | amd64 | arm64 |
| --- | --- | --- |
| Linux | `ripples_linux_amd64` | `ripples_linux_arm64` |
| macOS | `ripples_darwin_amd64` | `ripples_darwin_arm64` |
| Windows | `ripples_windows_amd64.exe` | `ripples_windows_arm64.exe` |

### Runtime Requirements

ripples also requires:

- `git`, to resolve revisions and create temporary worktrees.
- A Go toolchain, to load the target repository according to its `go.mod`, build constraints, and current environment.
- A Go module directory passed through `-repo` where `go list ./...` succeeds.

Even when using a prebuilt release binary, the target project still requires a compatible Go toolchain for analysis.

## Quick Start

Analyze the latest commit:

```bash
ripples -repo . -old HEAD~1 -new HEAD
```

Example output:

```text
internal/order.order
cmd/server.main
```

Add `-verbose` to print the number of affected packages and elapsed time to stderr:

```bash
ripples -repo . -old HEAD~1 -new HEAD -verbose
```

`-repo` must point to the Go module being analyzed. It may be the Git repository root or a module subdirectory inside a monorepo. ripples finds the Git root automatically and preserves relative paths required by same-repository `replace` directives:

```bash
ripples \
  -repo /path/to/monorepo/services/api \
  -old HEAD~1 \
  -new HEAD
```

`-old` and `-new` must resolve to commits. ripples analyzes committed Git trees and does not include uncommitted working tree changes.

### Options

| Option | Description | Default |
| --- | --- | --- |
| `-repo` | Git repository and Go module root | `.` |
| `-old` | Old commit ID or ref | required |
| `-new` | New commit ID or ref | required |
| `-output` | `simple`, `json`, `text`, `summary`, or `dot` | `simple` |
| `-verbose` | Print the affected package count and elapsed time to stderr | `false` |

## How It Works

Given old and new revisions in the same repository, ripples:

1. Resolves each revision to a commit and Git tree without modifying the current working tree.
2. Creates temporary detached worktrees for both trees while preserving the complete repository-relative directory layout.
3. Loads local package ASTs and type information under the current Go build configuration.
4. Ignores comments and source positions while comparing the semantic content of functions, methods, types, variables, constants, embedded files, and other declarations.
5. Merges the old and new declaration dependency graphs and walks reverse dependencies from each changed declaration.
6. Sorts and prints `<module-relative path>.<package name>`.

The package containing a changed declaration is always returned. Other packages are included only when their declarations reference or call affected content. Importing the same package alone is not enough.

## Output Formats

The default `simple` format prints one package per line and works well in shell scripts and CI:

```text
cmd/server.main
payment.payment
```

The `json` format:

```json
[
  {
    "path": "cmd/server",
    "name": "main"
  },
  {
    "path": "payment",
    "name": "payment"
  }
]
```

The `text` and `summary` formats print a human-readable package count:

```text
受影响的包: 2 个
- cmd/server.main
- payment.payment
```

The human-readable label is currently emitted in Chinese. Use `simple` or `json` for language-neutral automation.

The `dot` format emits the reverse package relationship subgraph for the current change. Edges point from a dependency to the package that uses it, and a red border marks packages containing changed declarations:

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

Generating DOT text does not require Graphviz. The `dot` command is only needed to convert that text to SVG, PNG, or another image format. The graph contains only packages involved in the current impact propagation; it is not a separate full import graph.

DOT output always uses package-level nodes. ripples still computes impact from declaration relationships between functions, methods, fields, types, variables, and constants, but it does not render those declarations as separate nodes or emit a complete function call graph. This keeps propagation paths visible without making large repository graphs unreadable.

## Cache

ripples builds content-addressed cache keys from the Git tree, analysis format version, Go toolchain, and build configuration. Repeated analyses of the same tree and configuration reuse the package snapshot.

The default location comes from Go's `os.UserCacheDir`:

| OS | Default path |
| --- | --- |
| macOS | `$HOME/Library/Caches/ripples` |
| Linux | `$XDG_CACHE_HOME/ripples`, or `$HOME/.cache/ripples` when unset |
| Windows | `%LocalAppData%\ripples` |

Override it with an absolute path:

```bash
RIPPLES_CACHE=/absolute/path/to/cache ripples \
  -repo . \
  -old HEAD~1 \
  -new HEAD
```

Cache keys include:

- Git tree
- Go module path relative to the Git repository root
- ripples analysis format version
- Go toolchain version
- `GOOS`, `GOARCH`, and `CGO_ENABLED`
- `GOFLAGS` and `GOEXPERIMENT`

A snapshot contains the Go AST and type analysis for the current build, CGo and compiler directives, `go:embed` file mappings, other compiler inputs, and the declaration dependency graph. When module or workspace files change, ripples also caches a lightweight third-party module dependency graph and propagates only to local packages that actually use the relevant module.

## GitHub Actions

The following workflow downloads the latest release binary, verifies its checksum, analyzes a pull request's base and head commits, and maps `cmd/server.main` to a downstream job:

```yaml
name: Impact

on:
  pull_request:

permissions:
  contents: read

jobs:
  impact:
    runs-on: ubuntu-latest
    outputs:
      server: ${{ steps.targets.outputs.server }}
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
          path: source

      - uses: actions/cache@v4
        with:
          path: ${{ runner.temp }}/ripples-cache
          key: ripples-${{ runner.os }}-${{ runner.arch }}-${{ github.event.pull_request.head.sha }}
          restore-keys: |
            ripples-${{ runner.os }}-${{ runner.arch }}-

      - name: Install ripples
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          release_dir="$RUNNER_TEMP/ripples-release"
          mkdir -p "$release_dir"
          gh release download \
            --repo jimyag/ripples \
            --pattern ripples_linux_amd64 \
            --pattern checksums.txt \
            --dir "$release_dir"
          (
            cd "$release_dir"
            sha256sum --ignore-missing --check checksums.txt
          )
          install -m 0755 \
            "$release_dir/ripples_linux_amd64" \
            "$RUNNER_TEMP/ripples"

      - name: Analyze affected packages
        id: targets
        env:
          RIPPLES_CACHE: ${{ runner.temp }}/ripples-cache
          BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HEAD_SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          "$RUNNER_TEMP/ripples" \
            -repo source \
            -old "$BASE_SHA" \
            -new "$HEAD_SHA" |
            tee affected-packages.txt

          if grep -Fxq "cmd/server.main" affected-packages.txt; then
            echo "server=true" >> "$GITHUB_OUTPUT"
          else
            echo "server=false" >> "$GITHUB_OUTPUT"
          fi

  test-server:
    needs: impact
    if: needs.impact.outputs.server == 'true'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - run: go test ./cmd/server/... ./internal/server/...
```

`fetch-depth: 0` ensures the base commit is available on the runner. The workflow uses the latest release and verifies `checksums.txt`. `RIPPLES_CACHE` must be an absolute path.

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

### Explicit Boundaries

- `_test.go` files are not analyzed by default.
- Analysis follows the current `GOOS`, `GOARCH`, and build tags. Run ripples separately for every build configuration that needs coverage.
- CGo preambles and `//go:` compiler directives participate in semantic comparison. Declaration-level directives propagate through actual users; linker-level directives conservatively affect the package.
- Within the current module, ripples follows interface parameters and fields, variable assignments, factories and multiple returns, closures, generic forwarding, type assertions and type switches, method values and method expressions, and concrete implementations that can be statically identified through slices, maps, channels, ranges, and `append`.
- Function values propagate through parameters, returns and multiple returns, struct fields, indirect pointer assignments, slices, maps, channels, ranges, and `append`. When one static location may hold multiple runtime values, every candidate is conservatively included.
- The standard library and third-party dependencies from `go.mod` are treated as black boxes; their function bodies are not traversed. When a local concrete value is passed to an external interface, propagation continues through the interface method contract.
- Temporary worktrees preserve same-repository local `replace` directories so nested modules load correctly. The declaration graph still covers only the module selected by `-repo`; modifying another local replacement module in the same commit does not yet propagate across modules.
- Reflection, `unsafe`, plugins, runtime registration, and calls determined only by external configuration cannot be resolved completely from the Go AST. ripples does not invent call relationships without static evidence.
- Added declarations use the new dependency graph, while removed declarations use the old graph.
- Effective `go.mod` and `go.work` build configuration changes affect the relevant build. Dependency version or `replace` changes affect only local packages that transitively use that module.
- Adding or removing ordinary cache entries in `go.sum` or `go.work.sum` has no impact. A checksum change for the same module version propagates to actual users.
- DOT relationship graphs contain package nodes only, not functions, fields, or other declarations.
- Output represents Go package impact only. Consumers map packages to binaries, services, labels, or deployment units.

## Development

Install development tools and run all checks:

```bash
task deps
task ci
```

Common tasks:

```bash
task --list-all
task fmt
task lint
task test
task build
task release-snapshot
```

Local builds are written to `bin/ripples`:

```bash
task build
```

## Release

Pushing a `v*` tag runs GoReleaser and uploads raw Linux, macOS, and Windows amd64/arm64 binaries plus `checksums.txt`. Release binaries are not wrapped in tar or zip archives.

Validate the release configuration and local artifacts before publishing:

```bash
task release-snapshot
```

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
