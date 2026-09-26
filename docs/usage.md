# 安装与使用

[简体中文](usage.md) · [English](usage.en.md)

本文介绍 ripples 的安装方式、CLI 参数、输出格式和缓存配置。分析原理与支持边界见[分析能力](analysis.md)，CI 集成见 [GitHub Actions](ci.md)。

## 安装

安装后运行 `ripples --version` 确认命令可用。

### 使用 Go 安装

如果本机已有 Go toolchain：

```bash
go install github.com/jimyag/ripples@latest
ripples --version
```

二进制会安装到 `$(go env GOPATH)/bin`。如果 shell 找不到 `ripples`，请把该目录加入 `PATH`。

### 下载最新二进制

[GitHub Release](https://github.com/jimyag/ripples/releases/latest) 提供 Linux、macOS 和 Windows 的 amd64/arm64 原始二进制，不需要在本地编译 ripples。

macOS 和 Linux 可以使用下面的命令自动选择当前平台：

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

该示例需要 [GitHub CLI](https://cli.github.com/)，并始终下载最新 Release。确保 `$HOME/.local/bin` 已加入 `PATH`。

也可以按平台手动下载：

| 系统 | amd64 | arm64 |
| --- | --- | --- |
| Linux | `ripples_linux_amd64` | `ripples_linux_arm64` |
| macOS | `ripples_darwin_amd64` | `ripples_darwin_arm64` |
| Windows | `ripples_windows_amd64.exe` | `ripples_windows_arm64.exe` |

### 运行要求

运行时还需要：

- `git`，用于解析 revision，并通过私有 index 把 tree 导出到临时目录；不会注册 worktree、不会触发仓库 hook。
- Go toolchain，用于按照目标仓库的 `go.mod`、构建约束和当前环境加载 package。
- `-repo` 指定的 Go module 目录可以执行 `go list -test ./...`，也就是测试文件也需要能编译。仓库不提交的生成代码可以用 `-prepare` 生成。

即使通过 Release 安装了预编译二进制，分析目标 Go 项目时仍需要匹配该项目的 Go toolchain。ripples 也会使用编译进二进制的 Go 版本检查源码类型；该版本必须支持待分析的两个 revision 声明的 Go 版本。可以用 `ripples --version` 查看二进制的 `goVersion`。目标项目升级 Go 次版本时，应更新 ripples Release，无需自行编译。

## CLI

分析最近一次提交：

```bash
ripples -repo . -old HEAD~1 -new HEAD
```

增加 `-verbose` 可以在 stderr 查看 package 数量和分析耗时：

```bash
ripples -repo . -old HEAD~1 -new HEAD -verbose
```

`-repo` 应指向待分析的 Go module，可以是 Git 仓库根目录，也可以是 monorepo 中的 module 子目录。ripples 会自动找到 Git 根目录，并保留同仓库 `replace` 所需的相对路径：

```bash
ripples \
  -repo /path/to/monorepo/services/api \
  -old HEAD~1 \
  -new HEAD
```

`-old` 和 `-new` 必须能够解析为 commit。ripples 分析的是已提交的 Git tree，不包含工作区中未提交的修改。

仓库依赖不提交的生成代码（protobuf、wire、mockgen 等）时，用 `-prepare` 在每个导出的 revision 中先生成代码。命令在导出的 module 目录中通过 `sh -c`（Windows 为 `cmd /C`）执行，失败时分析失败：

```bash
ripples -repo . -old origin/main -new HEAD -prepare 'go generate ./...'
```

### 参数

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-repo` | Git 仓库及 Go module 根目录 | `.` |
| `-old` | 旧 commit ID 或 ref | 必填 |
| `-new` | 新 commit ID 或 ref | 必填 |
| `-output` | `simple`、`json`、`text`、`summary` 或 `dot`；在分析前校验 | `simple` |
| `-prepare` | 分析前在每个导出 revision 的 module 目录中执行的 shell 命令 | 空 |
| `-verbose` | 在 stderr 输出受影响 package 数量和耗时 | `false` |

## 输出格式

默认的 `simple` 格式每行输出一个 package，适合 shell 和 CI：

```text
cmd/server.main
payment.payment
```

module 根目录的 package 相对路径是 `.`，例如 `..main`；按最后一个 `.` 拆分即可得到目录 `.` 和 package 名 `main`。

`json` 格式额外包含完整 import path；本次删除的 package 带 `"deleted": true`：

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

被删除的 package 无法构建或测试，`simple`、`text` 和 `summary` 不输出它们；它们的旧使用者仍会按 old 依赖图输出。

`text` 和 `summary` 输出带数量的可读摘要：

```text
Affected packages: 2
- cmd/server.main
- payment.payment
```

### DOT 关系图

`dot` 输出本次影响的 package 反向关系子图。边从被依赖的 package 指向使用它的 package，红色边框表示包含变更声明的 package，虚线边框表示本次删除的 package：

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

![ripples package 影响关系图示例](impact-example.svg)

[查看生成该图片的 DOT 输出](impact-example.dot)

生成 DOT 文本不依赖 Graphviz；只有转换成 SVG、PNG 等图片时才需要安装 `dot`。图中只包含本次变更涉及的 package，不是完整的 import 图或函数调用图。

## 缓存

ripples 使用 Git tree、分析格式版本、Go toolchain 和实际生效的构建配置生成内容寻址缓存键。相同 tree 和构建配置的重复分析可以直接复用 package snapshot。每次分析结束后，会删除 7 天内没有被读写过的条目；持续被使用的 snapshot（例如 main 分支）会一直保留。

默认目录来自 Go 的 `os.UserCacheDir`：

| 系统 | 默认目录 |
| --- | --- |
| macOS | `$HOME/Library/Caches/ripples` |
| Linux | `$XDG_CACHE_HOME/ripples`，未设置时为 `$HOME/.cache/ripples` |
| Windows | `%LocalAppData%\ripples` |

可以通过绝对路径覆盖：

```bash
RIPPLES_CACHE=/absolute/path/to/cache ripples \
  -repo . \
  -old HEAD~1 \
  -new HEAD
```

缓存键包含：

- Git tree
- Go module 在 Git 仓库中的相对目录
- ripples 分析格式版本和编译 ripples 的 Go 版本（`go/types` 版本）
- `go env` 报告的实际生效值：`GOOS`、`GOARCH`、`CGO_ENABLED`、`GOFLAGS`、`GOEXPERIMENT`、`GOVERSION`、`GOTOOLCHAIN`、`GOWORK` 和 `GOAMD64` 等架构级别；环境变量和 `go env -w` 写入的设置都会生效
- `-prepare` 命令

snapshot 包含当前构建中的声明依赖图、package 内容哈希，以及本地 package 到第三方 module 的依赖关系。module 信息和声明图来自同一次导出，保证两者描述同一个 Git tree。
