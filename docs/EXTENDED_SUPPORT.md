# 扩展符号类型支持设计文档

## 当前支持状态

### ✅ 已实现
- **函数**: 使用 `golang.PrepareCallHierarchy`
- **常量**: 使用 `golang.References` API (2025-11-22 实现)
- **全局变量**: 使用 `golang.References` API (2025-11-22 实现)
- **init 函数**: 使用 workspace 包导入分析 (2025-11-22 实现)
- **空导入 (_ import)**: 复用 init 函数追踪逻辑 (2025-11-22 实现)

### ⏳ 待实现
- 结构体字段变更
- 接口方法变更
- 类型变更

## 实现方案

### 1. 结构体字段变更追踪

#### 原理
当结构体字段变更时，需要找到所有访问该字段的代码位置，然后追踪这些位置所在的函数。

#### 实现步骤

1. 检测到字段变更（例如：`User.Name` 字段）
2. 使用 gopls 的 `References` API 查找字段的所有引用
3. 对每个引用位置，确定所在的函数
4. 使用现有的 `IncomingCalls` 机制追踪到 main

#### gopls API

```go
// 在 golang-tools fork 中需要添加的 API
type DirectTracer struct {
    // 现有字段...
}

// FindReferences 查找符号的所有引用位置
func (t *DirectTracer) FindReferences(pos Position, symbolName string) ([]Reference, error) {
    // 调用 gopls internal 的 References 功能
    // golang.References(snapshot, fileHandle, position)
}

type Reference struct {
    URI      string
    Range    protocol.Range
    InFunc   string // 所在的函数名
}
```

#### 示例场景

```go
// 变更：User.Name 字段类型从 string 改为 int

// user/user.go
type User struct {
    Name string // 变更了这个字段
}

// service/service.go
func GetUserName(u User) string {
    return u.Name // 引用点 1
}

// handler/handler.go
func HandleUser() {
    u := GetUser()
    fmt.Println(u.Name) // 引用点 2
}

// main.go
func main() {
    HandleUser() // 最终追踪到这里
}
```

追踪路径：
```
User.Name (字段)
  -> GetUserName (引用点1所在函数)
  -> HandleUser (调用 GetUserName)
  -> main (调用 HandleUser)
```

### 2. 常量/变量变更追踪

#### 原理
与字段类似，常量和全局变量的变更也需要找到所有使用点。

#### 实现步骤

1. 检测到常量/变量变更
2. 使用 `References` API 查找所有使用该常量/变量的位置
3. 追踪使用位置所在的函数到 main

#### 示例场景

```go
// config/config.go
const MaxRetries = 3 // 变更了这个常量

// service/retry.go
func DoWithRetry() {
    for i := 0; i < MaxRetries; i++ { // 使用点
        // ...
    }
}

// main.go
func main() {
    DoWithRetry()
}
```

### 3. init 函数追踪 ✅ **已实现 (2025-11-22)**

#### 实现方式
使用 gopls 的 `snapshot.LoadMetadataGraph()` 加载完整的 workspace 包图，然后递归检查包依赖关系。

#### 实现位置
- `golang-tools/gopls/internal/ripplesapi/tracer.go`: `FindMainPackagesImporting` 方法
- `internal/lsp/direct_tracer.go`: 处理 `SymbolKindInit`
- `internal/analyzer/lsp_analyzer.go`: `isSupportedSymbolKind` 支持 init

#### 工作原理
1. 检测到 init 函数变更，获取其包路径
2. 使用 `snapshot.LoadMetadataGraph(ctx)` 加载所有包的元数据
3. 遍历所有 main 包
4. 对每个 main 包递归检查是否导入目标包（直接或间接）
5. 返回所有导入该包的 main 包

#### 核心代码
```go
func (t *DirectTracer) FindMainPackagesImporting(targetPkgPath string) ([]CallPath, error) {
    // 加载完整的 workspace 元数据图
    graph, err := t.snapshot.LoadMetadataGraph(t.ctx)

    // 遍历所有包，找出 main 包
    for _, meta := range graph.Packages {
        if meta.Name != "main" {
            continue
        }

        // 递归检查是否导入目标包
        if t.importsPackage(graph, meta, targetPkgPath) {
            // 添加到结果
        }
    }
}
```

#### 测试覆盖
见 `internal/analyzer/init_trace_test.go`:
- 多服务场景测试
- 不同依赖层级测试
- 直接和间接导入测试

#### 优势
- 使用 gopls 的缓存元数据，性能高效
- 支持间接导入检测
- 与现有架构无缝集成

### 4. 空导入 (_ import) 追踪 ✅ **已实现 (2025-11-22)**

#### 实现方式
复用 init 函数的 `FindMainPackagesImporting` 实现，因为空导入的主要目的就是触发 init 函数。

#### 实现位置
- `internal/lsp/direct_tracer.go`: 处理 `SymbolKindImport`
- 验证导入是空导入（`alias == "_"`）
- 提取导入的包路径并调用 `FindMainPackagesImporting`

#### 工作原理
1. 检测到空导入变更（如 `_ "database/sql/driver"`）
2. 从 `ImportExtra` 中提取导入的包路径
3. 验证是空导入（非普通导入）
4. 使用 `FindMainPackagesImporting` 找到所有导入该包的 main 包
5. 返回受影响的服务

#### 示例场景
```go
// driver/mysql/mysql.go
func init() {
    sql.Register("mysql", &Driver{})
}

// main.go
import _ "project/driver/mysql" // 空导入变更会影响 main

func main() {
    db, _ := sql.Open("mysql", "...")
}
```

#### 测试覆盖
见 `internal/analyzer/blank_import_test.go`:
- `TestTraceBlankImportToMain` - 验证空导入追踪
- `TestBlankImportVsNormalImport` - 验证只追踪空导入
- 测试多种导入场景

#### 限制
只支持空导入（`_`），普通导入不会被追踪（因为它们本身不影响运行时行为）。

## 实现优先级

### 已完成 ✅
1. **常量/变量追踪** - 2025-11-22 实现
   - 使用 `golang.References` API
   - 测试覆盖完整

2. **init 函数追踪** - 2025-11-22 实现
   - 使用 workspace 包导入分析
   - 支持间接依赖检测

3. **空导入 (_ import) 追踪** - 2025-11-22 实现
   - 复用 init 函数实现
   - 验证空导入并追踪影响

### 下一步建议
1. **结构体字段追踪**（优先级：高）
   - 实现较复杂
   - 需要处理嵌入字段、匿名字段等特殊情况
   - 但实用价值很高

2. **接口方法变更追踪**（优先级：中）
   - 使用 gopls `Implementation` API
   - 找到所有实现该接口的类型

## 技术挑战

### 1. References API 性能
对于大型项目，查找所有引用可能较慢。需要考虑：
- 缓存 References 结果
- 并行查询多个符号
- 只查询变更包及其依赖包

### 2. 间接影响判断
字段变更可能不直接影响代码，例如：
```go
type User struct {
    Name string // 从 string 改为 int
}

// 这个函数受影响吗？
func GetUser() User {
    return User{Name: "test"} // 这里会编译错误，所以受影响
}

// 这个函数受影响吗？
func SaveUser(u User) {
    // 只保存，不访问 Name 字段，但类型定义变了
}
```

建议策略：如果结构体字段类型变更，所有使用该结构体的函数都标记为受影响。

### 3. 接口实现检测
当接口方法签名变更时，需要找到所有实现该接口的类型。gopls 提供了 `Implementation` API。

## 测试策略

### 单元测试
为每种符号类型创建独立的测试用例：
- 创建最小化的测试项目
- 明确定义变更点和预期的追踪结果
- 验证追踪的完整性和准确性

### 集成测试
使用真实项目结构测试：
- 复杂的导入关系
- 多层级的函数调用
- 混合多种符号类型的变更

### 性能测试
测试大型项目的追踪性能：
- 1000+ 文件的项目
- 深层次的调用链
- 大量的字段引用

## 后续优化

1. **智能过滤**
   - 只报告"实际受影响"的代码（如字段被读写，而非仅仅是传递）

2. **影响程度分级**
   - 高影响：直接使用变更的符号
   - 中影响：间接使用（通过函数参数传递）
   - 低影响：仅类型依赖

3. **增量分析**
   - 缓存上次分析结果
   - 只重新分析变更部分

## 总结

扩展支持这些符号类型是可行的，核心思路是：
1. 使用 gopls 的 `References` API 找到使用点
2. 使用 `go/packages` 分析包导入关系
3. 复用现有的 `IncomingCalls` 机制追踪到 main

建议先实现常量/变量追踪和 init 函数追踪，验证方案可行性后再扩展到更复杂的场景。
