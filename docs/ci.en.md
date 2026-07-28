# GitHub Actions

[简体中文](ci.md) · [English](ci.en.md)

The workflow below downloads the latest release binary, verifies its checksum, analyzes a pull request's base and head commits, and maps `cmd/server.main` to a downstream job:

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

## Key Configuration

- `fetch-depth: 0` ensures that the base commit is available on the runner.
- `checksums.txt` verifies the downloaded release binary.
- `RIPPLES_CACHE` must be an absolute path. The example uses the runner's temporary directory and restores it through `actions/cache`.
- The `simple` output contains one `<relative path>.<package name>` per line, which can be mapped to binaries, services, labels, or test jobs with `grep -Fxq`.
- The example always downloads the latest release. For a fully reproducible pipeline, pin the release tag and checksum in repository configuration.

See [Installation and Usage](usage.en.md) for CLI and cache details, and [Analysis](analysis.en.md) for impact semantics.
