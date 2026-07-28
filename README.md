<h1 align="center">ripples</h1>

<p align="center">
  <strong>Find the Go packages affected by a Git change.</strong>
</p>

<p align="center">
  <a href="https://github.com/jimyag/ripples/actions/workflows/check.yaml"><img src="https://github.com/jimyag/ripples/actions/workflows/check.yaml/badge.svg" alt="Check"></a>
  <a href="https://github.com/jimyag/ripples/actions/workflows/release.yaml"><img src="https://github.com/jimyag/ripples/actions/workflows/release.yaml/badge.svg" alt="Release"></a>
</p>

<p align="center">
  <a href="#安装">安装</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#支持范围">支持范围</a> ·
  <a href="#在-github-actions-中使用">GitHub Actions</a>
</p>

---

ripples 基于 Go AST、类型信息和声明依赖图，分析两个 Git revision 之间受直接或间接影响的 Go package。它关注代码是否实际引用了变更声明，而不是简单返回所有 import 变更 package 的调用方。

ripples 的稳定输出是 package。binary、service、构建任务和部署单元可以在 CI 中继续映射：

```text
cmd/server.main
internal/order.order
payment.payment
```

## 核心能力

- **声明级影响分析**：识别新增、删除和修改的函数、方法、类型、字段、变量、常量和 `init`。
- **直接与间接传播**：沿实际声明引用和调用关系反向查找所有受影响 package。
- **接口实现解析**：根据调用点和值流定位实际接口实现，不把使用同一接口的其他实现混在一起。
- **函数值和值流**：覆盖参数、返回值、闭包、方法值、struct 字段、容器、多返回值、类型断言和泛型透传。
- **构建输入感知**：识别 build tags、CGo、`//go:` 指令、`go:embed`、`go.mod` 和 `go.work` 的有效变化。
- **适合 CI**：输出稳定排序的 package 列表，支持持久缓存、JSON、摘要和 DOT 关系图。

## 安装

选择下面任意一种方式。安装后运行 `ripples --version` 确认命令可用。

### 使用 Go 安装

如果本机已有 Go toolchain，这是最简单的安装方式：

```bash
go install github.com/jimyag/ripples@latest
ripples --version
```

二进制会安装到 `$(go env GOPATH)/bin`。如果 shell 找不到 `ripples`，请把该目录加入 `PATH`。

### 下载最新二进制

[GitHub Release](https://github.com/jimyag/ripples/releases/latest) 提供 Linux、macOS 和 Windows 的 amd64/arm64 原始二进制，不需要本地 Go 环境。

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

## 快速开始

分析最近一次提交：

```bash
ripples -repo . -old HEAD~1 -new HEAD
```

输出示例：

```text
internal/order.order
cmd/server.main
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

## 工作方式

给定同一仓库中的 old/new revision，ripples 会：

1. 解析 revision 对应的 commit 和 Git tree，不修改当前工作区。
2. 为两棵 tree 创建临时 detached worktree，保留 Git 仓库中的完整相对目录结构。
3. 按当前 Go 构建配置加载本地 package 的 AST 和类型信息。
4. 忽略注释和源码位置，比较函数、方法、类型、变量、常量和嵌入文件等声明的语义内容。
5. 合并 old/new 声明依赖图，从变更声明反向查找直接及间接使用者。
6. 稳定排序并输出 `<module 内相对路径>.<package 名>`。

变更所在的 package 始终返回。其他 package 只有在声明实际引用或调用了变更内容时才会传播；仅仅 import 同一个 package 不会被判定为受影响。

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

`dot` 输出本次影响的 package 反向关系子图。边从被依赖的 package 指向使用它的 package，红色边框表示包含变更声明的 package：

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

生成 DOT 文本不依赖 Graphviz；只有转换成 SVG、PNG 等图片时才需要安装 `dot`。图中只包含本次变更涉及的 package，关系来自同一次声明级影响传播，而不是单独生成的 import 图。

DOT 图的展示粒度固定为 package。ripples 内部仍通过函数、方法、字段、类型、变量和常量等声明关系计算影响范围，但不会把这些声明画成独立节点，也不输出完整的函数调用图。这可以保留 package 之间的传播路径，同时避免大型仓库的图因声明节点过多而难以阅读。

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

## 在 GitHub Actions 中使用

下面的示例下载最新 Release 二进制，校验 checksum，分析 PR 的 base/head commit，并把 `cmd/server.main` 映射为下游 job：

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

`fetch-depth: 0` 用于确保 runner 上存在 base commit。CI 会使用最新 Release，并校验其中的 `checksums.txt`；`RIPPLES_CACHE` 必须配置为绝对路径。

## 支持范围

| 类别 | 支持的变化和使用方式 |
| --- | --- |
| 声明变化 | 函数、方法、类型、interface 方法、struct 字段、package 变量、常量和 `init` |
| 变更类型 | 新增、删除和修改；删除使用 old 依赖图，新增使用 new 依赖图 |
| 依赖传播 | 直接引用、间接引用、函数调用、方法调用和跨 package 传递 |
| 接口调用 | 接口参数、返回值、字段、类型断言、type switch，以及调用点可确定的具体实现 |
| 函数值 | 参数、返回值、多返回值、闭包捕获、函数类型转换、方法值、方法表达式和指针间接赋值 |
| 构造与容器 | struct 构造器字段、slice、array、map、channel、range、`append`、容器返回值和静态可确定的索引/key |
| 调用语法 | 普通调用、`go`、`defer`、variadic 调用、泛型函数和泛型透传 |
| 初始化 | package 变量初始化、多个变量声明、常量变化、`init` 新增/删除/修改和跨 package 初始化顺序 |
| 构建输入 | build tags、文件名构建约束、CGo preamble、`//go:` 指令、`go:embed` 和其他编译输入 |
| Module/Workspace | `go.mod`、`go.sum`、`go.work`、`go.work.sum`、dependency 版本和 `replace` 的有效变化 |
| 输出与复用 | simple、JSON、text、summary、DOT，以及按 Git tree 和构建配置复用的持久缓存 |

### 明确边界

- 默认不分析 `_test.go`。
- 只分析当前 `GOOS`、`GOARCH` 和 build tags 对应的构建结果；需要覆盖多种构建配置时，应分别执行。
- CGo preamble 和 `//go:` 编译指令会参与语义比较；声明级指令沿实际使用者传播，链接级指令按 package 保守传播。
- 当前 module 内会追踪接口参数和字段、变量赋值、工厂及多返回值、闭包、泛型透传、类型断言和 type switch、方法值和方法表达式，以及 slice、map、channel、range 和 `append` 中可由 AST 与类型信息确定的具体实现。
- 函数值可通过参数、返回值及多返回值、struct 字段、指针间接赋值、slice、map、channel、range 和 `append` 继续传播；同一静态位置存在多个运行时可能值时，结果会保守地包含全部候选。
- 标准库和 `go.mod` 中的第三方依赖按黑盒处理，不遍历其函数体；本地具体值传入外部接口时，会按接口方法契约继续传播。
- 临时 worktree 会保留同仓库本地 `replace` 的目录，使嵌套 module 可以正确加载；当前声明图仍只覆盖 `-repo` 指定的 module，同一提交直接修改其他本地 replacement module 时，尚不会跨 module 传播到调用方。
- 反射、`unsafe`、`plugin`、运行时注册和只由外部配置决定的动态调用无法由 Go AST 完整确定，ripples 不猜测缺少静态证据的调用关系。
- 新增声明使用 new 依赖图，删除声明使用 old 依赖图。
- `go.mod`、`go.work` 的有效构建配置变化会影响对应构建；dependency 版本或 replace 变化只影响实际传递依赖该 module 的本地 package。
- `go.sum`、`go.work.sum` 新增或删除普通缓存记录不会产生影响；同一 module 版本的 checksum 改变会传播到实际使用者。
- DOT 关系图只展示 package 节点，不展示函数、字段或其他声明节点。
- 输出只表示 Go package 影响；binary、service、label 和部署单元由调用方映射。

## 开发

安装开发工具并执行检查：

```bash
task deps
task ci
```

常用任务：

```bash
task --list-all
task fmt
task lint
task test
task build
task release-snapshot
```

本地构建结果位于 `bin/ripples`：

```bash
task build
```

## 发布

推送 `v*` tag 后，Release workflow 会通过 GoReleaser 上传 Linux、macOS 和 Windows 的 amd64/arm64 原始二进制及 `checksums.txt`，不会打包为 tar 或 zip。

发布前可以验证配置和本地产物：

```bash
task release-snapshot
```

## License

本项目基于 [GNU General Public License v3.0](LICENSE) 发布。
