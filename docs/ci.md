# 在 GitHub Actions 中使用

[简体中文](ci.md) · [English](ci.en.md)

## 使用 ripples-action

[ripples-action](https://github.com/jimyag/ripples-action) 封装 Release 下载、checksum 校验和 JSON 输出。将下面的 workflow 保存为 `.github/workflows/impact.yml`：

```yaml
name: Impact
on: pull_request

permissions:
  contents: read

jobs:
  impact:
    runs-on: ubuntu-latest
    outputs:
      mains: ${{ steps.impact.outputs.mains }}
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - id: impact
        uses: jimyag/ripples-action@v0.1.0
        with:
          base-sha: ${{ github.event.pull_request.base.sha }}
          head-sha: ${{ github.event.pull_request.head.sha }}
          ripples-version: v0.3.1

  test-server:
    needs: impact
    if: contains(fromJSON(needs.impact.outputs.mains), 'cmd/server.main')
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
      - run: go test ./cmd/server/... ./internal/server/...
```

`packages` 和 `mains` 是 JSON 字符串数组，值为 `<module 内相对路径>.<package 名>`；根目录的 main 为 `..main`。`has-changes` 是字符串 `true` 或 `false`。被删除的 package 不在数组中；仅删除且没有存活调用方受影响时，数组为空，`has-changes` 为 `false`。这些值不是 import path 或服务名，需要自行映射到测试、构建或 label。Action 当前不创建 label，也未提供 CLI 的 `-tests` 和 `-prepare` 参数；需要这些参数时用下方的 CLI 示例。

子目录 module 在 Action 上设置 `repo-path`，同时调整 `setup-go` 的 `go-version-file`。Action 默认使用 v0.3.1，包含与二进制构建版本一致的 Go 工具链选择修复。支持 Linux amd64/arm64，其他平台使用 CLI。

## 在 PR 中评论，包括 fork PR

另存为 `.github/workflows/impact-comment.yml`，并把两个 workflow 合入基准仓库的默认分支：

```yaml
name: Impact comment
on:
  workflow_run:
    workflows: [Impact]
    types: [completed]

jobs:
  comment:
    if: github.event.workflow_run.event == 'pull_request' && github.event.workflow_run.conclusion == 'success'
    permissions:
      contents: read
      pull-requests: write
    uses: jimyag/ripples-action/.github/workflows/comment.yml@v0.1.0
```

`workflows: [Impact]` 匹配源 workflow 的 `name`，不是文件名；也可以监听已有的 PR CI。独立 workflow 按来源仓库和 head SHA 查找开放 PR，用只读 token 重新分析，再由另一个 job 更新同一条 bot 评论。普通 fork `pull_request` 无法直接写评论；首次贡献者还可能需要维护者批准源 workflow。无需让 fork 作者配置额外 secret。仓库的 Actions 策略需要允许这些 Action 和可复用 workflow。

子目录 module 在评论 job 上加 `with: {repo-path: path/to/module}`。可复用 workflow 默认使用 v0.3.1，也接受 `ripples-version`；源 workflow 的版本设置不会自动传入，切换版本时需分别设置。内部 Ripples Action 引用已固定到完整 SHA，调用方固定外层 SHA 即可固定这些脚本。详细输入、权限和当前限制见 [ripples-action 文档](https://github.com/jimyag/ripples-action#comments-on-fork-prs)。

## 直接调用 CLI

需要 `-tests`、生成代码或自定义缓存时，可以直接调用 CLI。下面下载最新 Release、校验 checksum，并把 `cmd/server.main` 映射为下游 job：

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

### 关键配置

- `fetch-depth: 0` 确保 runner 上存在 base commit。
- `checksums.txt` 用于验证下载的 Release 二进制。
- `RIPPLES_CACHE` 必须是绝对路径；示例使用 runner 的临时目录并通过 `actions/cache` 跨任务复用。`RIPPLES_CACHE_MAX_MB`（默认 1024）限制缓存总大小，也就限制了 `actions/cache` 保存的体积。
- `simple` 输出每行一个 `<相对路径>.<package 名>`，适合用 `grep -Fxq` 映射到 binary、service、label 或测试任务；被删除的 package 不会出现在 `simple` 输出中。默认不分析 `_test.go`；用结果选择测试任务时加 `-tests`，只修改测试的 PR 也会报出对应 package。
- 仓库依赖不提交的生成代码时，在分析命令中加上 `-prepare 'go generate ./...'` 或对应的生成命令，runner 上需要安装生成工具。
- 从 v0.3.1 起，ripples 用 Release 二进制内置的 Go 版本（`ripples --version` 的 `goVersion`）运行 `go` 命令，runner 上没有该版本时会自动下载它；该版本必须支持两个 revision 的 `go.mod`。想避免每次下载，可以用 `actions/cache` 缓存 `go env GOMODCACHE` 目录。
- 示例始终下载最新 Release。如果需要完全可复现的流水线，可以把 Release tag 和 checksum 固定在仓库配置中。

更多 CLI 和缓存说明见[安装与使用](usage.md)，影响范围的语义见[分析能力](analysis.md)。
