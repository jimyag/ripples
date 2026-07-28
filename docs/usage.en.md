# Installation and Usage

[简体中文](usage.md) · [English](usage.en.md)

This guide covers installation, CLI options, output formats, and caching. See [Analysis](analysis.en.md) for behavior and boundaries, and [GitHub Actions](ci.en.md) for CI integration.

## Installation

After installation, run `ripples --version` to confirm the command is available.

### Install with Go

If a Go toolchain is already available:

```bash
go install github.com/jimyag/ripples@latest
ripples --version
```

The binary is installed into `$(go env GOPATH)/bin`. Add that directory to `PATH` if your shell cannot find `ripples`.

### Download the Latest Binary

[GitHub Releases](https://github.com/jimyag/ripples/releases/latest) provides raw amd64 and arm64 binaries for Linux, macOS, and Windows. Building ripples locally is not required.

On macOS and Linux, the following commands select the current platform:

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

## CLI

Analyze the latest commit:

```bash
ripples -repo . -old HEAD~1 -new HEAD
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

### DOT Graph

The `dot` format emits the reverse package relationship subgraph for the current change. Edges point from a dependency to the package that uses it, and a red border marks packages containing changed declarations:

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

![Example ripples package impact graph](impact-example.svg)

[View the DOT output used to generate this image](impact-example.dot)

Generating DOT text does not require Graphviz. The `dot` command is only needed to convert that text to SVG, PNG, or another image format. The graph contains only packages involved in the current impact propagation; it is not a complete import graph or function call graph.

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
