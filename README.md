# ripples

基于 Go AST、类型信息和声明依赖图的代码变更影响分析工具。

给定同一个 Git 仓库中的两个提交，ripples 会：

1. 将 old/new 提交分别解压到临时目录，不修改当前工作区。
2. 解析当前构建配置下各 package 的 Go AST。
3. 忽略注释和源码位置，比较 package 的语义内容。
4. 合并 old/new 两版声明依赖图，从变更函数、方法、类型、变量、常量和嵌入文件反向查找真实使用者。
5. 稳定排序并输出 `<模块内路径>.<package 名>`。

分析采用声明级策略：变更 package 本身始终返回；其他 package 只有真实引用或调用了变更声明时才视为受影响，单纯 import 同一个 package 不会传播。当前 module 内的接口值会按调用点记录已知的具体实现，不会把同一接口在其他调用点的实现混入当前调用链。

## 安装

```bash
go build -o ripples .
```

## 使用

```bash
./ripples -repo <仓库路径> -old <旧提交> -new <新提交>
```

参数：

| 参数 | 说明 | 默认值 |
| --- | --- | --- |
| `-repo` | Git 仓库及 Go module 路径 | `.` |
| `-old` | 旧 commit ID 或 ref | 必填 |
| `-new` | 新 commit ID 或 ref | 必填 |
| `-output` | `simple`、`json`、`text`、`summary` | `simple` |
| `-verbose` | 在 stderr 显示数量和耗时 | `false` |

### simple

```text
cmd/server.main
internal/order.order
payment.payment
```

### json

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

## 缓存

package snapshot 使用 Git tree、Go 版本和构建配置生成内容寻址缓存键。相同提交和构建配置的重复分析不需要再次解压和解析。

默认缓存目录：

```text
<os.UserCacheDir>/ripples
```

可通过绝对路径覆盖：

```bash
RIPPLES_CACHE=/absolute/path/to/cache ./ripples ...
```

缓存键包含：

- Git tree
- ripples 分析格式版本
- Go toolchain 版本
- `GOOS`、`GOARCH`、`CGO_ENABLED`
- `GOFLAGS`、`GOEXPERIMENT`

声明摘要还包含当前构建中的 Go AST、类型解析结果、`go:embed` 文件到变量声明的映射、其他编译输入、声明依赖图，以及 `go.mod`/`go.work`。

## 在 CI 中使用

下面的 GitHub Actions 示例会分析一个 PR 相对 base commit 影响的 package，并把 `cmd/server.main` 映射为后续 job 的开关。其他 binary 或 service 可以用相同方式继续映射。

```yaml
name: Impact

on:
  pull_request:

permissions:
  contents: read

env:
  # 替换为要使用的已发布 tag。
  RIPPLES_VERSION: vX.Y.Z

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
          key: ripples-${{ env.RIPPLES_VERSION }}-${{ runner.os }}-${{ runner.arch }}-${{ github.event.pull_request.head.sha }}
          restore-keys: |
            ripples-${{ env.RIPPLES_VERSION }}-${{ runner.os }}-${{ runner.arch }}-
      - name: Install ripples release
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          release_dir="$RUNNER_TEMP/ripples-release"
          mkdir -p "$release_dir"
          gh release download "$RIPPLES_VERSION" \
            --repo jimyag/ripples \
            --pattern "ripples_linux_amd64" \
            --pattern "checksums.txt" \
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

`fetch-depth: 0` 是必需的，否则 runner 上可能没有 base commit。`RIPPLES_CACHE` 必须是绝对路径；缓存内部已经包含 Git tree、Go toolchain 和构建配置，可以跨 PR 增量复用。CI 应固定 `RIPPLES_VERSION`，并使用 Release 中的 `checksums.txt` 校验下载的二进制。

## 行为边界

- 默认不分析 `_test.go`。
- 只分析当前 `GOOS`、`GOARCH` 和 build tags 对应的构建配置。
- 注释-only 变更不会产生受影响 package。
- 当前 module 内会追踪接口参数和字段、变量赋值、工厂及多返回值、闭包、泛型透传、类型断言和 type switch、方法值和方法表达式，以及 slice、map、channel、range 和 `append` 中可由 AST 与类型信息确定的具体实现。
- 标准库和 `go.mod` 依赖按黑盒处理，不遍历第三方函数体；本地具体值传入外部接口时，按接口方法契约传播，避免遗漏 binary/main。
- 反射、`unsafe`、`plugin`、运行时注册和仅由外部配置决定的动态调用无法由 Go AST 完整确定，不猜测不存在静态证据的调用关系。
- 新增和删除声明分别使用 new 和 old 依赖图。
- `go.mod` 或 `go.work` 变化按保守策略影响所有本地 package。
- 输出只表示 Go package 影响；binary、service 和部署单元应由调用方继续映射。

## 开发

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

## 发布

推送 `v*` tag 后，GitHub Actions 会通过 GoReleaser 创建 Release，并上传 Linux、macOS 和 Windows 的 amd64/arm64 二进制及校验文件。

发布前可以在本地验证：

```bash
task release-snapshot
```
