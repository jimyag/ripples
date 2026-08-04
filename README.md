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
  <a href="#影响关系图">关系图</a> ·
  <a href="#文档">文档</a>
</p>

<p align="center">
  <strong>简体中文</strong> · <a href="./README.en.md">English</a>
</p>

---

ripples 基于 Go AST、类型信息和声明依赖图，分析两个 Git revision 之间受直接或间接影响的 Go package。它关注代码是否实际引用了变更声明，而不是简单返回所有 import 变更 package 的调用方。

稳定输出是 `<module 内相对路径>.<package 名>`：

```text
cmd/server.main
internal/order.order
payment.payment
```

binary、service、构建任务、label 和部署单元可以在 CI 中基于 package 输出继续映射。

## 核心能力

- **声明级分析**：识别函数、方法、类型、字段、变量、常量和 `init` 的新增、删除与修改。
- **直接与间接传播**：沿实际声明引用和调用关系反向查找受影响 package。
- **接口实现解析**：根据调用点和值流定位具体实现，不混入同一接口的其他实现。
- **Go 常见语法覆盖**：支持函数值、闭包、容器、类型断言、泛型、`go`、`defer` 和初始化关系。
- **构建输入感知**：识别 build tags、CGo、`//go:` 指令、`go:embed`、`go.mod` 和 `go.work` 的有效变化。
- **适合 CI**：稳定排序输出，支持持久缓存、JSON、摘要和 DOT 关系图。

完整覆盖范围和静态分析边界见[分析能力](docs/analysis.md)。

## 安装

使用 Go 安装最新版：

```bash
go install github.com/jimyag/ripples@latest
ripples --version
```

也可以从 [GitHub Release](https://github.com/jimyag/ripples/releases/latest) 下载 Linux、macOS 和 Windows 的 amd64/arm64 原始二进制。

分析目标项目时仍需要 `git`、匹配项目的 Go toolchain，以及能够执行 `go list ./...` 的 Go module。平台二进制下载命令和完整运行要求见[安装与使用](docs/usage.md)。

## 快速开始

分析最近一次提交：

```bash
ripples -repo . -old HEAD~1 -new HEAD
```

示例输出：

```text
cmd/server.main
internal/order.order
payment.payment
```

`-repo` 指向待分析的 Go module，可以是仓库根目录或 monorepo 中的 module 子目录。`-old` 和 `-new` 必须能够解析为 commit；ripples 分析已提交的 Git tree，不包含工作区中未提交的修改。

参数、输出格式和缓存配置见[安装与使用](docs/usage.md)。

## 影响关系图

使用 `dot` 输出 package 反向关系图，再通过 Graphviz 转换为 SVG：

```bash
ripples -repo . -old HEAD~1 -new HEAD -output dot > impact.dot
dot -Tsvg impact.dot -o impact.svg
```

红框表示包含变更声明的 package，箭头指向使用它的 package：

![ripples package 影响关系图示例](docs/impact-example.svg)

[查看 DOT 源文件](docs/impact-example.dot)

## 文档

| 文档 | 内容 |
| --- | --- |
| [安装与使用](docs/usage.md) | 安装方式、CLI 参数、输出格式、DOT 和缓存 |
| [分析能力](docs/analysis.md) | 分析原理、支持的 Go 使用方式和明确边界 |
| [实现架构](docs/architecture.md) | revision 快照、声明图、值流、反向传播、缓存和并发实现 |
| [GitHub Actions](docs/ci.md) | Release 下载、checksum、缓存和下游任务映射 |

## 开发

```bash
task deps
task ci
```

`task lint` 使用 golangci-lint 检查全部 Go package 和测试文件。常用任务可以通过 `task --list-all` 查看，本地构建结果位于 `bin/ripples`。

## 发布

推送 `v*` tag 后，Release workflow 会通过 GoReleaser 上传各平台原始二进制及 `checksums.txt`，不会打包为 tar 或 zip。发布前运行 `task release-snapshot` 验证配置和本地产物。

## License

本项目基于 [GNU General Public License v3.0](LICENSE) 发布。
