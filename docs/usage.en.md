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

- `git`, to resolve revisions and export trees into temporary directories through a private index. No worktree is registered and no repository hook runs.
- A Go toolchain, to load the target repository according to its `go.mod`, build constraints, and current environment.
- A Go module directory passed through `-repo` where `go list ./...` succeeds; with `-tests`, `go list -test ./...` must succeed, so test files must compile too. Generate code that the repository does not commit with `-prepare`.

Even when using a prebuilt release binary, the target project still requires a compatible Go toolchain for analysis. ripples also type-checks source using the Go version built into its binary, which must support the Go version declared by both revisions. Check the binary's `goVersion` with `ripples --version`, and update the release when the target project moves to a newer Go minor version. You do not need to build the release yourself.

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

When the repository relies on generated code that it does not commit (protobuf, wire, mockgen, and so on), use `-prepare` to generate it in every exported revision first. The command runs in the exported module directory through `sh -c` (`cmd /C` on Windows), and a failure fails the analysis:

```bash
ripples -repo . -old origin/main -new HEAD -prepare 'go generate ./...'
```

`_test.go` files are not analyzed by default: builds and deployments do not need them, and cold analyses are faster. Add `-tests` when test jobs are selected by impact, so a commit that only changes tests still reports the package under test:

```bash
ripples -repo . -old origin/main -new HEAD -tests
```

### Options

| Option | Description | Default |
| --- | --- | --- |
| `-repo` | Git repository and Go module root | `.` |
| `-old` | Old commit ID or ref | required |
| `-new` | New commit ID or ref | required |
| `-output` | `simple`, `json`, `text`, `summary`, or `dot`; validated before analysis | `simple` |
| `-prepare` | Shell command run in each exported revision's module directory before analysis | empty |
| `-tests` | Also analyze `_test.go` files and report packages whose tests alone changed | `false` |
| `-verbose` | Print the affected package count and elapsed time to stderr | `false` |

## Output Formats

The default `simple` format prints one package per line and works well in shell scripts and CI:

```text
cmd/server.main
payment.payment
```

The package at the module root has the relative path `.`, for example `..main`; splitting at the last `.` yields the directory `.` and the package name `main`.

The `json` format also contains the full import path, and packages deleted by the change carry `"deleted": true`:

```json
[
  {
    "path": "cmd/server",
    "name": "main",
    "import_path": "example.com/app/cmd/server"
  },
  {
    "path": "legacy",
    "name": "legacy",
    "import_path": "example.com/app/legacy",
    "deleted": true
  }
]
```

Deleted packages cannot be built or tested, so `simple`, `text`, and `summary` omit them; their former users are still reported through the old dependency graph.

The `text` and `summary` formats print a human-readable package count:

```text
Affected packages: 2
- cmd/server.main
- payment.payment
```

### DOT Graph

The `dot` format emits the reverse package relationship subgraph for the current change. Edges point from a dependency to the package that uses it, a red border marks packages containing changed declarations, and a dashed border marks packages deleted by the change:

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

![Example ripples package impact graph](impact-example.svg)

[View the DOT output used to generate this image](impact-example.dot)

Generating DOT text does not require Graphviz. The `dot` command is only needed to convert that text to SVG, PNG, or another image format. The graph contains only packages involved in the current impact propagation; it is not a complete import graph or function call graph.

## Cache

ripples builds content-addressed cache keys from the Git tree, analysis format version, Go toolchain, and effective build configuration. Repeated analyses of the same tree and configuration reuse the package snapshot. After every analysis, entries that have not been read or written for 7 days are removed first; if the cache still exceeds its size limit, the least recently used entries are removed until it fits. Snapshots in regular use, such as the main branch, stay.

The limit defaults to 1024 MB and can be changed with `RIPPLES_CACHE_MAX_MB`. Snapshots are split into per-package chunks deduplicated by content hash and gzip-compressed, so unchanged packages share one copy across commits. For a repository with about 3,000 Go files, 50 consecutive commits take about 30 MB, compared with about 140 MB when every tree is stored whole.

ripples type-checks the current module's packages from source itself and only uses `go list -export` for the type information of standard-library and third-party dependencies. Their compiled output goes to the Go build cache (`GOCACHE`), each version is compiled once, and later analyses reuse it; Go automatically removes entries unused for 5 days. Caching `GOCACHE` in CI as well (for example with the default cache of `actions/setup-go`) saves compiling the dependencies on the first analysis.

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
- ripples analysis format version and the Go version ripples was built with (the `go/types` version)
- Effective values reported by `go env`: `GOOS`, `GOARCH`, `CGO_ENABLED`, `GOFLAGS`, `GOEXPERIMENT`, `GOVERSION`, `GOTOOLCHAIN`, `GOWORK`, and architecture levels such as `GOAMD64`; both environment variables and settings written with `go env -w` count
- The `-prepare` command and `-tests`

A snapshot contains the declaration dependency graph for the current build, package content hashes, and the mapping from local packages to third-party modules. Module information and the declaration graph come from the same export, so both always describe one Git tree.
