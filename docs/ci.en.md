# GitHub Actions

[简体中文](ci.md) · [English](ci.en.md)

## Use ripples-action

[ripples-action](https://github.com/jimyag/ripples-action) handles release downloads, checksum verification, and JSON outputs. Save this workflow as `.github/workflows/impact.yml`:

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

`packages` and `mains` are JSON string arrays in `<module-relative path>.<package name>` form; a root main package is `..main`. `has-changes` is the string `true` or `false`. Deleted packages are omitted; a deletion with no affected surviving callers produces empty arrays and `has-changes=false`. These values are neither import paths nor service names: map them to your tests, builds, or labels. The Action does not create labels or expose the CLI's `-tests` and `-prepare` options; use the CLI example below when those are needed.

For a nested module, set the Action's `repo-path` and adjust `setup-go`'s `go-version-file`. The Action defaults to v0.3.1, which includes the fix to select the binary's build toolchain. The Action supports Linux amd64/arm64; use the CLI on other platforms.

## PR comments, including fork PRs

Save this separately as `.github/workflows/impact-comment.yml` and merge both workflows into the base repository's default branch:

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

`workflows: [Impact]` matches the source workflow's `name`, not its filename; you may also listen to existing PR CI. The separate workflow finds an open PR by its head repository and SHA, repeats analysis with a read-only token, and updates one bot comment in another job. Ordinary fork `pull_request` runs cannot write comments directly; first-time contributors may also need a maintainer to approve the source workflow. Fork authors need no additional secrets. Repository Actions policies must allow the Actions and reusable workflow.

For a nested module, add `with: {repo-path: path/to/module}` to the comment job. The reusable workflow defaults to v0.3.1 and accepts `ripples-version`; the source workflow's version is not inherited, so set both when switching releases. Internal Ripples Action references use full SHAs, so pinning the outer workflow also fixes those scripts. See the [ripples-action documentation](https://github.com/jimyag/ripples-action#comments-on-fork-prs) for inputs, permissions, and current limitations.

## Call the CLI directly

Use the CLI for `-tests`, generated code, or custom caching. This workflow downloads the latest release, verifies its checksum, and maps `cmd/server.main` to a downstream job:

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

### Key Configuration

- `fetch-depth: 0` ensures that the base commit is available on the runner.
- `checksums.txt` verifies the downloaded release binary.
- `RIPPLES_CACHE` must be an absolute path. The example uses the runner's temporary directory and restores it through `actions/cache`. `RIPPLES_CACHE_MAX_MB` (1024 by default) bounds the total cache size and therefore what `actions/cache` saves.
- The `simple` output contains one `<relative path>.<package name>` per line, which can be mapped to binaries, services, labels, or test jobs with `grep -Fxq`; deleted packages never appear in `simple` output. `_test.go` files are not analyzed by default; add `-tests` when the result selects test jobs, so a pull request that only changes tests still reports its package.
- When the repository relies on generated code that it does not commit, add `-prepare 'go generate ./...'` or the matching generator command to the analysis step, and install the generators on the runner.
- Starting with v0.3.1, ripples runs `go` commands with the Go version built into the release binary (`goVersion` in `ripples --version`) and downloads it when that version is missing; it must support both revisions' `go.mod` requirements. To avoid downloading it on every run, cache the `go env GOMODCACHE` directory with `actions/cache`.
- The example always downloads the latest release. For a fully reproducible pipeline, pin the release tag and checksum in repository configuration.

See [Installation and Usage](usage.en.md) for CLI and cache details, and [Analysis](analysis.en.md) for impact semantics.
