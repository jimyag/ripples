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

- `git`，用于解析 revision 和创建临时 worktree。
- Go toolchain，用于按照目标仓库的 `go.mod`、构建约束和当前环境加载 package。
- `-repo` 指定的 Go module 目录可以执行 `go list ./...`。

即使通过 Release 安装了预编译二进制，分析目标 Go 项目时仍需要匹配该项目的 Go toolchain。

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

### 参数

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-repo` | Git 仓库及 Go module 根目录 | `.` |
| `-old` | 旧 commit ID 或 ref | 必填 |
| `-new` | 新 commit ID 或 ref | 必填 |
| `-output` | `simple`、`json`、`text`、`summary` 或 `dot` | `simple` |
| `-verbose` | 在 stderr 输出受影响 package 数量和耗时 | `false` |

## 输出格式

默认的 `simple` 格式每行输出一个 package，适合 shell 和 CI：

```text
cmd/server.main
payment.payment
```

`json` 格式：

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

`text` 和 `summary` 输出带数量的可读摘要：

```text
受影响的包: 2 个
- cmd/server.main
- payment.payment
```

### DOT 关系图

`dot` 输出本次影响的 package 反向关系子图。边从被依赖的 package 指向使用它的 package，红色边框表示包含变更声明的 package：

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

![ripples package 影响关系图示例](impact-example.svg)

[查看生成该图片的 DOT 输出](impact-example.dot)

生成 DOT 文本不依赖 Graphviz；只有转换成 SVG、PNG 等图片时才需要安装 `dot`。图中只包含本次变更涉及的 package，不是完整的 import 图或函数调用图。

## 缓存

ripples 使用 Git tree、分析格式版本、Go toolchain 和构建配置生成内容寻址缓存键。相同 tree 和构建配置的重复分析可以直接复用 package snapshot。

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
- ripples 分析格式版本
- Go toolchain 版本
- `GOOS`、`GOARCH`、`CGO_ENABLED`
- `GOFLAGS`、`GOEXPERIMENT`

snapshot 包含当前构建中的 Go AST、类型解析结果、CGo/编译指令、`go:embed` 文件映射、其他编译输入和声明依赖图。module/workspace 文件变化时，ripples 会额外缓存轻量的第三方 module 依赖图，只传播到实际使用相关 module 的本地 package。
