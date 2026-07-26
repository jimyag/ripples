# ripples

基于 Go AST、类型信息和声明依赖图的代码变更影响分析工具。

给定同一个 Git 仓库中的两个提交，ripples 会：

1. 将 old/new 提交分别解压到临时目录，不修改当前工作区。
2. 解析当前构建配置下各 package 的 Go AST。
3. 忽略注释和源码位置，比较 package 的语义内容。
4. 合并 old/new 两版声明依赖图，从变更函数、方法、类型、变量和常量反向查找真实使用者。
5. 稳定排序并输出 `<模块内路径>.<package 名>`。

分析采用声明级策略：变更 package 本身始终返回；其他 package 只有真实引用或调用了变更声明时才视为受影响，单纯 import 同一个 package 不会传播。

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

package 内容摘要还包含已参与当前构建的 Go AST、embed 文件、其他编译输入、import graph，以及 `go.mod`/`go.work`。

## 行为边界

- 默认不分析 `_test.go`。
- 只分析当前 `GOOS`、`GOARCH` 和 build tags 对应的构建配置。
- 注释-only 变更不会产生受影响 package。
- 无法由 AST 和类型信息唯一确定具体实现的接口、反射和高阶动态调用不会猜测传播。
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

推送 `v*` tag 后，GitHub Actions 会通过 GoReleaser 创建 Release，并上传 Linux、macOS 和 Windows 的 amd64/arm64 二进制归档及校验文件。

发布前可以在本地验证：

```bash
task release-snapshot
```
