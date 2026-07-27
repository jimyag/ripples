# ripples

[![Check](https://github.com/jimyag/ripples/actions/workflows/check.yaml/badge.svg)](https://github.com/jimyag/ripples/actions/workflows/check.yaml)
[![Release](https://github.com/jimyag/ripples/actions/workflows/release.yaml/badge.svg)](https://github.com/jimyag/ripples/actions/workflows/release.yaml)

基于 Go AST、类型信息和声明依赖图，分析两个 Git revision 之间受直接或间接影响的 Go package。

ripples 的稳定输出是 package，而不是 binary 或 service。调用方可以继续把 `cmd/server.main` 映射为构建任务、服务名或部署单元。

```text
cmd/server.main
internal/order.order
payment.payment
```

## 工作方式

给定同一仓库中的 old/new revision，ripples 会：

1. 解析 revision 对应的 commit 和 Git tree，不修改当前工作区。
2. 为两棵 tree 创建临时 detached worktree，保留 Git 仓库中的完整相对目录结构。
3. 按当前 Go 构建配置加载本地 package 的 AST 和类型信息。
4. 忽略注释和源码位置，比较函数、方法、类型、变量、常量和嵌入文件等声明的语义内容。
5. 合并 old/new 声明依赖图，从变更声明反向查找直接及间接使用者。
6. 稳定排序并输出 `<module 内相对路径>.<package 名>`。

变更所在的 package 始终返回。其他 package 只有在声明实际引用或调用了变更内容时才会传播；仅仅 import 同一个 package 不会被判定为受影响。

## 安装

可以直接下载最新的 [GitHub Release](https://github.com/jimyag/ripples/releases/latest) 二进制，或者使用 `go install`。

### 下载二进制

| 系统 | 架构 | Release asset |
| --- | --- | --- |
| Linux | amd64 | `ripples_linux_amd64` |
| Linux | arm64 | `ripples_linux_arm64` |
| macOS | amd64 | `ripples_darwin_amd64` |
| macOS | arm64 | `ripples_darwin_arm64` |
| Windows | amd64 | `ripples_windows_amd64.exe` |
| Windows | arm64 | `ripples_windows_arm64.exe` |

例如，在 macOS arm64 上使用 GitHub CLI 安装：

```bash
gh release download \
  --repo jimyag/ripples \
  --pattern ripples_darwin_arm64 \
  --dir /tmp/ripples-release
install -m 0755 /tmp/ripples-release/ripples_darwin_arm64 /usr/local/bin/ripples
```

省略 tag 时，`gh release download` 会下载最新 Release。

### 使用 go install

```bash
go install github.com/jimyag/ripples@latest
```

安装结果位于 `$(go env GOPATH)/bin/ripples`。

运行时还需要：

- `git`，用于解析 revision 和创建临时 worktree。
- Go toolchain，用于按照目标仓库的 `go.mod`、构建约束和当前环境加载 package。
- `-repo` 指定的 Go module 目录可以执行 `go list ./...`。

## 快速使用

```bash
ripples \
  -repo /path/to/repository \
  -old <base-commit-or-ref> \
  -new <head-commit-or-ref>
```

例如分析最近一次提交：

```bash
ripples -repo . -old HEAD~1 -new HEAD -verbose
```

`-repo` 应指向待分析的 Go module，可以是 Git 仓库根目录，也可以是 monorepo 中的子目录。ripples 会自动找到 Git 根目录，临时 worktree 会保留同仓库 `replace` 所需的相对路径。例如：

```bash
ripples \
  -repo /path/to/monorepo/services/api \
  -old HEAD~1 \
  -new HEAD
```

查看当前版本和构建信息：

```bash
ripples --version
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

### 输出格式

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

## 分析边界

- 默认不分析 `_test.go`。
- 只分析当前 `GOOS`、`GOARCH` 和 build tags 对应的构建结果；需要覆盖多种构建配置时，应分别执行。
- CGo preamble 和 `//go:` 编译指令会参与语义比较；声明级指令沿实际使用者传播，链接级指令按 package 保守传播。
- 当前 module 内会追踪接口参数和字段、变量赋值、工厂及多返回值、闭包、泛型透传、类型断言和 type switch、方法值和方法表达式，以及 slice、map、channel、range 和 `append` 中可由 AST 与类型信息确定的具体实现。
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
