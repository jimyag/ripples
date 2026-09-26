# 分析能力

[简体中文](analysis.md) · [English](analysis.en.md)

本文说明 ripples 如何计算影响范围、当前覆盖哪些 Go 使用方式，以及静态分析无法可靠判断的边界。源码结构和算法细节见[实现架构](architecture.md)，安装和 CLI 说明见[安装与使用](usage.md)。

## 工作方式

给定同一仓库中的 old/new revision，ripples 会：

1. 解析 revision 对应的 commit 和 Git tree，不修改当前工作区。
2. 用私有 index 把两棵 tree 完整导出到临时目录，保留 Git 仓库中的相对目录结构；不注册 worktree、不触发 hook，也不受 sparse-checkout 影响。
3. 按实际生效的 Go 构建配置加载本地 package 的 AST 和类型信息；指定 `-tests` 时同时加载 `_test.go`。
4. 忽略注释和源码位置，比较函数、方法、类型、变量、常量和嵌入文件等声明的语义内容。
5. 合并 old/new 声明依赖图，从变更声明反向查找直接及间接使用者。
6. 稳定排序并输出 `<module 内相对路径>.<package 名>`。

变更所在的 package 始终返回。其他 package 只有在声明实际引用了变更内容，或把相关类型转换为接口、用它实例化泛型时才会传播；仅仅 import 同一个 package 不会被判定为受影响。

新增声明使用 new 依赖图，删除声明使用 old 依赖图，因此新增和删除都能沿对应 revision 的真实关系传播。

## 支持范围

| 类别 | 支持的变化和使用方式 |
| --- | --- |
| 声明变化 | 函数、方法、类型、interface 方法、struct 字段、package 变量、常量和 `init` |
| 变更类型 | 新增、删除和修改；删除使用 old 依赖图，新增使用 new 依赖图 |
| 依赖传播 | 直接引用、间接引用、函数调用、方法调用和跨 package 传递；经嵌入字段提升的字段和方法同时依赖路径上的嵌入字段 |
| 测试 | 默认不分析 `_test.go`，只改测试的提交不报出任何 package；指定 `-tests` 后覆盖 `_test.go`、外部测试 package（归到被测 package）和只有测试文件的 package，测试文件里的 `init` 不影响导入方 |
| 接口 | 把具体值转换为接口的声明依赖该类型的契约：转换为 `any` 或 `error` 时依赖全部方法，转换为其他接口时只依赖可能被动态调用的方法，两者都包括字段布局和嵌套命名类型；只经本地接口调用的方法只影响同时到达转换点和调用点的 package；构造函数、setter、functional options、注册表、嵌入接口字段和 `fmt`/`encoding/json` 等外部参数都能传播 |
| 函数值 | 引用函数（参数、字段、容器、返回值、方法值、方法表达式）即依赖该函数 |
| 泛型 | 泛型函数、泛型类型的方法和字段；调用泛型函数或泛型类型实例的方法时依赖类型实参的契约 |
| 字段布局 | 无键 struct 字面量依赖字段顺序、类型和 tag |
| 初始化 | package 变量实际引用、有运行时副作用的命名及空白初始化、多个变量声明、常量变化、`init` 新增/删除/修改和跨 package 初始化顺序；类型转换不等同于函数调用，但保留可能 panic 的转换和转换参数中的运行时效果 |
| 构建输入 | build tags、文件名构建约束、CGo preamble、`//go:` 指令、`go:embed`，以及汇编、C/C++、头文件和 syso 等非 Go 源文件 |
| Module/Workspace | `go.mod`、`go.sum`、实际生效的 `go.work`（包括父目录中的 go.work）、`go.work.sum`、dependency 版本和 `replace` 的有效变化 |
| 输出与复用 | simple、JSON、text、summary、DOT，以及按 Git tree 和实际生效的构建配置复用的持久缓存 |

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

## 接口、泛型与函数值

具体值一旦转换为接口，之后无论流向哪里，都可能通过动态派发、类型断言或反射调用它的方法。因此 ripples 不追踪接口值的流向，而是让执行转换的声明依赖该类型的契约。契约分两种：

- 完整契约：方法集中的全部方法（含经嵌入提升的方法）、字段布局，以及字段、元素和类型实参中本地命名类型的完整契约。转换为 `any` 时使用，因为 `fmt`、`encoding/json`、模板等可以通过反射访问任意方法和字段；转换为 `error`（以及包含 `Error() string` 的接口）时也使用，因为很多库通过匿名接口检查错误，例如 `pkg/errors` 的 `Cause()` 和 gRPC 的 `GRPCStatus()`，依赖的导出类型信息里看不到这些检查。
- 分派契约：字段布局、可能被动态调用的方法，以及嵌套命名类型的分派契约。转换为其他非空接口时使用。

经接口 J 的调用只会分派到实现了 J 的类型，所以只考虑类型实现了的接口。一个方法在以下情况下可能被“任意位置”调用：

- 本地代码用类型断言或 type switch 检查过包含该方法的接口 J，例如 `v.(interface{ Flush() })`。
- 标准库或第三方依赖导出的接口 J 声明了该方法，例如 `fmt.Stringer`、`json.Marshaler`、`http.Flusher`，因为依赖库的函数体可能调用它。
- 方法名是 `Unwrap`、`Is`、`As`、`Timeout` 或 `Temporary`，标准库通过匿名接口检查这些方法。

这些方法进入分派契约，做转换的声明直接依赖它们。只由本地代码经接口调用的方法（接口可以是本地声明的、结构体字段上的 `interface{ List() }` 这类匿名接口，或函数内定义的接口）则要同时满足两边：一个 binary 只有既把该类型转换成接口、又执行了其中某个调用，才可能运行它。ripples 记录发起调用的声明；包初始化和 `main` 是同一个 binary 里互不引用的声明，所以两边按 package 汇合：方法变化时，只报出同时有声明到达转换点、也有声明到达调用点的 package。接口里声明了、但没有任何代码经接口调用的方法，不影响做转换的代码。

因此服务端接口 `OrderService.Cancel(ctx, id)` 的调用不会让签名相同的客户端方法 `OrderClient.Cancel` 变成可达，只要 `OrderClient` 没有实现 `OrderService`。泛型类型的方法和含类型参数的接口无法在实例化前判断实现关系，按方法名匹配。

其他规则：

- 转换点由官方 `golang.org/x/tools/go/ssa` 确定。SSA 把所有隐式转换显式化为 `MakeInterface`，覆盖赋值、调用参数、返回值、组合字面量、channel 发送、map 写入和 package 变量初始化。
- 调用泛型函数、或调用泛型类型实例的方法时，依赖类型实参的完整契约，因为泛型代码会通过类型参数调用这些方法。只在签名或变量类型里写出实例化类型（例如 `Page[Order]`）不执行泛型代码，不产生这项依赖。
- 函数值不做流向追踪：引用函数即依赖它。

例如多个服务都通过 `orders.New()` 拿到同一个客户端，只有 reporting 服务调用了 `Export`：

```go
// orders 包新增或修改 Client.Export。
// 没有代码经接口调用 Export 时，只报出 orders；
// cmd/reporting 调用了 api.Export(...)，它同时到达转换点和调用点，被报出；
// 只调用 api.Get() 的 cmd/billing 不受影响。
func New() API { return &Client{} }
```

影响落在同时包含转换和调用的代码上，而不是只做接口调用、或只做转换的代码上。例如：

```go
// main 把 FileStore 注入 service 并运行；FileStore.Save 变化时 main 受影响，
// 只调用 s.store.Save() 的 service 包，以及只构造 FileStore 的工厂包都不会被报出。
func main() { service.New(store.FileStore{}).Run() }
```

这种方式在两个方向上和旧版值流分析不同：

- 更保守：同一个 package 里分别到达转换点和调用点的两段无关代码也算汇合；引用了函数值但没有调用也算依赖。
- 更完整：setter、functional options、本地注册表、包初始化时注册、嵌入接口字段、`fmt.Stringer`、`json.Marshaler` 等动态调用都能传播，分析时间与代码规模线性相关。

不同转换点互不混合：两个 binary 分别注入同一接口的不同实现时，修改其中一个实现只影响注入它的那个 binary。

## 构建和 module 变化

- 只分析实际生效的 `GOOS`、`GOARCH` 和 build tags（包括 `go env -w` 写入的设置）对应的构建结果。需要覆盖多种构建配置时，应分别执行。
- CGo preamble 和 `//go:` 编译指令参与语义比较；声明级指令沿实际使用者传播，链接级指令按 package 保守传播。
- 汇编、C/C++、头文件和 syso 等非 Go 源文件变化时，没有 Go 函数体的声明（由汇编实现）和经 cgo 处理的文件中的声明会传播到它们的调用方。
- `go.mod` 和实际生效的 `go.work` 的有效构建配置变化会影响对应构建；go.work 按 go 命令的规则定位，可以位于 `-repo` 的父目录。
- dependency 版本或 `replace` 变化只影响实际传递依赖该 module 的本地 package。
- `go.sum`、`go.work.sum` 新增或删除普通缓存记录不会产生影响；同一 module 版本的 checksum 改变会传播到实际使用者。

## 明确边界

- 标准库和 `go.mod` 中的第三方依赖按黑盒处理，不遍历其函数体。
- 临时导出目录保留同仓库本地 `replace` 的目录，使嵌套 module 可以正确加载；当前声明图仍只覆盖 `-repo` 指定的 module，同一提交直接修改其他本地 replacement module 时，尚不会跨 module 传播到调用方。
- 转换为 `any` 或 `error` 后的反射和匿名接口检查由完整契约覆盖。值先转换为其他非空接口，之后再按方法名反射调用（例如把接口字段交给模板执行 `{{.Method}}`），或被依赖库用未导出、匿名接口检查上述名单以外的方法时，这些方法不在分派契约内。
- `unsafe`、`plugin`、`//go:linkname` 和只由外部配置决定的动态行为无法由 Go 源码确定。
- 默认不分析测试文件；需要按测试影响选择测试任务时指定 `-tests`。
- 仓库不提交的生成代码需要通过 `-prepare` 在导出目录中生成，否则加载会失败。
- DOT 关系图只展示 package 节点，不展示函数、字段或其他声明节点。
- 输出只表示 Go package 影响；binary、service、label 和部署单元由调用方映射。
