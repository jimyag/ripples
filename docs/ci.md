# 在 GitHub Actions 中使用

[简体中文](ci.md) · [English](ci.en.md)

下面的 workflow 下载最新 Release 二进制、校验 checksum、分析 PR 的 base/head commit，并把 `cmd/server.main` 映射为下游 job：

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

## 关键配置

- `fetch-depth: 0` 确保 runner 上存在 base commit。
- `checksums.txt` 用于验证下载的 Release 二进制。
- `RIPPLES_CACHE` 必须是绝对路径；示例使用 runner 的临时目录并通过 `actions/cache` 跨任务复用。
- `simple` 输出每行一个 `<相对路径>.<package 名>`，适合用 `grep -Fxq` 映射到 binary、service、label 或测试任务。
- 示例始终下载最新 Release。如果需要完全可复现的流水线，可以把 Release tag 和 checksum 固定在仓库配置中。

更多 CLI 和缓存说明见[安装与使用](usage.md)，影响范围的语义见[分析能力](analysis.md)。
