# 分析能力

[简体中文](analysis.md) · [English](analysis.en.md)

本文说明 ripples 如何计算影响范围、当前覆盖哪些 Go 使用方式，以及静态分析无法可靠判断的边界。源码结构和算法细节见[实现架构](architecture.md)，安装和 CLI 说明见[安装与使用](usage.md)。

## 工作方式

给定同一仓库中的 old/new revision，ripples 会：

1. 解析 revision 对应的 commit 和 Git tree，不修改当前工作区。
2. 为两棵 tree 创建临时 detached worktree，保留 Git 仓库中的完整相对目录结构。
3. 按当前 Go 构建配置加载本地 package 的 AST 和类型信息。
4. 忽略注释和源码位置，比较函数、方法、类型、变量、常量和嵌入文件等声明的语义内容。
5. 合并 old/new 声明依赖图，从变更声明反向查找直接及间接使用者。
6. 稳定排序并输出 `<module 内相对路径>.<package 名>`。

变更所在的 package 始终返回。其他 package 只有在声明实际引用或调用了变更内容时才会传播；仅仅 import 同一个 package 不会被判定为受影响。

新增声明使用 new 依赖图，删除声明使用 old 依赖图，因此新增和删除都能沿对应 revision 的真实关系传播。

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
| 初始化 | package 变量实际引用、有运行时副作用的命名及空白初始化、多个变量声明、常量变化、`init` 新增/删除/修改和跨 package 初始化顺序；类型转换不等同于函数调用，但保留可能 panic 的转换和转换参数中的运行时效果 |
| 构建输入 | build tags、文件名构建约束、CGo preamble、`//go:` 指令、`go:embed` 和其他编译输入 |
| Module/Workspace | `go.mod`、`go.sum`、`go.work`、`go.work.sum`、dependency 版本和 `replace` 的有效变化 |
| 输出与复用 | simple、JSON、text、summary、DOT，以及按 Git tree 和构建配置复用的持久缓存 |

### Package 初始化副作用判断

ripples 使用有限且可解释的语法规则判断 package 初始化效果，不做函数纯度、完整值域或完整 panic 分析。命中下表中的运行时规则时，初始化器会保守地连接到 package-init；其他表达式只通过实际声明引用传播。

| 语法 | 判断 | 依据 |
| --- | --- | --- |
| 显式 `init()` | 传播 | Go 在 package 初始化阶段执行 `init()`，导入方无法绕过 |
| 导入本地 package | 传播其 package-init | 保留 Go 的跨 package 初始化顺序 |
| 普通函数或 builtin 调用 | 传播 | ripples 不做函数纯度分析，调用可能修改状态、阻塞或 panic |
| 仅创建函数字面量 | 不传播函数体 | 创建函数值不会执行函数体；变量被使用时仍通过声明引用传播 |
| 立即执行函数字面量 | 传播 | 外层 `CallExpr` 会立即执行函数体 |
| channel receive | 传播 | `<-ch` 会在初始化阶段接收并可能阻塞 |
| 类型转换 | 默认不传播转换本身 | Go AST 同样使用 `CallExpr` 表示转换，但转换不是函数调用；转换参数仍继续检查 |
| slice → 非空 array/array pointer | 传播 | slice 长度不足时会在运行时 panic |
| 字面量、无调用的组合字面量和普通运算 | 不传播 | 表达式本身没有上述运行时效果；其中嵌套的调用或 receive 仍会被检查 |
| 依赖运行时值才可能 panic 的其他表达式 | 不增加专门的 package-init 边 | 当前不推断索引越界、nil 解引用或运行时除零；仍保留其中的声明引用和嵌套效果 |

例如，命名和空白变量中的真实调用都会传播。即使返回值没有被其他声明使用，调用仍会在导入 package 时执行：

```go
var Registry = sets.New("a", "b") // 保守地视为可能有副作用
var _ = registerHandlers()         // 返回值被丢弃，但调用仍会执行
var Ready = <-readyCh              // 初始化时接收，可能阻塞
```

创建函数值不会执行函数体，但立即调用会执行：

```go
var Handler = func() { registerHandlers() } // 不因函数体连接到 package-init
var _ = func() bool {                       // 立即调用，连接到 package-init
	registerHandlers()
	return true
}()
```

Go 使用同一个 `CallExpr` AST 节点表示函数调用和类型转换。ripples 使用 `types.Info` 区分两者，避免把没有运行时效果的编译期接口断言当成 package 初始化副作用：

```go
var _ API = (*Client)(nil) // 仅检查 *Client 是否实现 API，不影响导入方
```

判断为类型转换后仍会继续检查参数。转换参数中的真实调用仍会执行，因此需要传播：

```go
var _ = ID(load()) // load() 的变化影响所有导入方
```

slice 转换为 array 或 array pointer 时，如果 slice 长度小于 array 长度，Go 会在运行时 panic。这类转换本身属于初始化期运行时效果，同样需要传播：

```go
var data = []byte{1}
var _ = [2]byte(data)    // package 初始化时 panic
var _ = (*[2]byte)(data) // package 初始化时 panic
```

命名变量也会在 package 被导入时初始化。只要初始化器包含上述运行时效果，即使变量没有被引用，也会连接到 package-init；纯字面量等没有运行时效果且未被引用的变量不会扩大影响范围：

```go
var Version = "v1"          // 未被引用时不影响导入方
var Ports = []int{80, 443}  // 没有嵌套运行时效果时不影响导入方
```

当前判断不是完整的 Go panic 分析。例如 `items[index]`、`*pointer` 和 `value/divisor` 是否 panic 取决于运行时值，ripples 目前不会仅因这些表达式扩大到所有导入方。它们引用的声明以及表达式内部的函数调用、channel receive 和上述 slice 转换仍会正常传播。

## 接口与函数值传播

- 接口参数和字段、变量赋值、工厂及多返回值、闭包、泛型透传、类型断言和 type switch、方法值和方法表达式都会进入值流分析。
- slice、array、map、channel、range 和 `append` 中的具体实现会在 AST 与类型信息能够确定时继续传播。
- 同一静态位置存在多个运行时可能值时，结果会保守地包含全部候选。
- 本地具体值传入外部接口时，会按接口方法契约继续传播，但不会遍历第三方库的函数体。

接口实现解析以调用点和值流为依据。修改某个具体实现时，不会仅因其他类型实现了同一个接口就把它们及其调用方一起返回。

## 构建和 module 变化

- 只分析当前 `GOOS`、`GOARCH` 和 build tags 对应的构建结果。需要覆盖多种构建配置时，应分别执行。
- CGo preamble 和 `//go:` 编译指令参与语义比较；声明级指令沿实际使用者传播，链接级指令按 package 保守传播。
- `go.mod`、`go.work` 的有效构建配置变化会影响对应构建。
- dependency 版本或 `replace` 变化只影响实际传递依赖该 module 的本地 package。
- `go.sum`、`go.work.sum` 新增或删除普通缓存记录不会产生影响；同一 module 版本的 checksum 改变会传播到实际使用者。

## 明确边界

- 默认不分析 `_test.go`。
- 标准库和 `go.mod` 中的第三方依赖按黑盒处理，不遍历其函数体。
- 临时 worktree 会保留同仓库本地 `replace` 的目录，使嵌套 module 可以正确加载；当前声明图仍只覆盖 `-repo` 指定的 module，同一提交直接修改其他本地 replacement module 时，尚不会跨 module 传播到调用方。
- 反射、`unsafe`、`plugin`、运行时注册和只由外部配置决定的动态调用无法由 Go AST 完整确定，ripples 不猜测缺少静态证据的调用关系。
- DOT 关系图只展示 package 节点，不展示函数、字段或其他声明节点。
- 输出只表示 Go package 影响；binary、service、label 和部署单元由调用方映射。
