# 常量和变量追踪功能实现总结

## 实现概述

成功扩展 ripples 以支持常量和全局变量的变更追踪，通过使用 gopls 的 References API。

## 实现的功能

### 1. golang-tools 新增 API

在 `golang-tools/gopls/internal/ripplesapi/tracer.go` 中添加了以下方法：

#### FindReferences
```go
func (t *DirectTracer) FindReferences(pos Position, symbolName string) ([]Reference, error)
```
- 查找符号的所有引用位置
- 返回每个引用所在的函数名

#### findContainingFunction
```go
func (t *DirectTracer) findContainingFunction(uri protocol.DocumentURI, position protocol.Position) (string, error)
```
- 找到给定位置所在的函数
- 使用 PrepareCallHierarchy 和 AST 遍历两种方式

#### findFunctionDeclaration
```go
func (t *DirectTracer) findFunctionDeclaration(fh file.Handle, funcName string) (Position, error)
```
- 通过函数名找到函数声明的位置
- 用于后续的调用链追踪

#### TraceReferencesToMain
```go
func (t *DirectTracer) TraceReferencesToMain(pos Position, symbolName string) ([]CallPath, error)
```
- 主入口函数，追踪符号的所有引用到 main 函数
- 对每个引用找到包含函数，然后追踪到 main

### 2. ripples 集成

#### 更新 direct_tracer.go
```go
// TraceToMain 现在支持多种符号类型
func (t *DirectCallTracer) TraceToMain(symbol *parser.Symbol) ([]CallPath, error) {
    switch symbol.Kind {
    case parser.SymbolKindFunction:
        apiPaths, err = t.tracer.TraceToMain(pos, symbol.Name)
    case parser.SymbolKindConstant, parser.SymbolKindVariable:
        apiPaths, err = t.tracer.TraceReferencesToMain(pos, symbol.Name)
    }
}
```

#### 更新 lsp_analyzer.go
```go
// 添加了 isSupportedSymbolKind 函数
func isSupportedSymbolKind(kind parser.SymbolKind) bool {
    switch kind {
    case parser.SymbolKindFunction,
         parser.SymbolKindConstant,
         parser.SymbolKindVariable:
        return true
    }
}
```

## 工作原理

### 常量/变量追踪流程

```
1. 用户修改常量 MaxRetries 的值
   ↓
2. ripples 检测到常量变更
   ↓
3. 调用 FindReferences(MaxRetries位置, "MaxRetries")
   ↓
4. gopls 返回所有引用位置：
   - config.go:4 (定义处)
   - retry.go:7 (使用处)
   ↓
5. 对每个引用，调用 findContainingFunction
   ↓
6. 找到包含函数: DoWithRetry
   ↓
7. 对 DoWithRetry 调用 TraceToMain
   ↓
8. 追踪调用链:
   main() -> DoWithRetry()
   ↓
9. 报告: server 服务受影响
```

## 测试结果

测试项目结构：
```
testdata/constant-test/
├── cmd/server/main.go         # main() 调用 service.DoWithRetry()
├── internal/service/retry.go  # DoWithRetry() 使用 config.MaxRetries
└── internal/config/config.go  # const MaxRetries = 5
```

测试输出：
```
=== 测试常量引用查找 ===
找到 2 个引用:
[1] config.go (定义处)
[2] retry.go:7 (使用处)
    所在函数: DoWithRetry

=== 测试追踪到 main 函数 ===
找到 1 条路径到 main 函数:
Binary: server
调用链:
  -> main
  -> DoWithRetry
```

## 支持的符号类型

### 当前已支持
- ✅ 函数 (原有功能)
- ✅ 常量 (新增)
- ✅ 全局变量 (新增)

### 待支持
- ⏳ 结构体字段
- ⏳ 接口方法
- ⏳ init 函数
- ⏳ 空导入 (_ import)

## 性能考虑

### References API 的性能
- 对于大型项目，查找所有引用可能需要几秒钟
- gopls 内部会缓存类型信息，重复查询会更快

### 优化建议
1. 并行处理多个符号的引用查找
2. 缓存已找到的函数声明位置
3. 对于同一个包内的多个符号变更，只加载一次 AST

## 局限性

### 当前限制
1. **不支持动态引用**
   - reflect.ValueOf(config.MaxRetries) 无法追踪

2. **不支持字符串引用**
   - fmt.Sprintf("retry=%d", config.MaxRetries) 可以追踪
   - json.Marshal(config) 中的字段引用无法追踪

3. **接口实现**
   - 如果常量通过接口方法返回，可能无法准确追踪所有调用者

## 下一步计划

### 短期
1. 优化路径显示（修复 main 函数的包路径显示）
2. 添加更多测试用例
3. 支持结构体字段变更

### 中期
1. 实现 init 函数追踪
2. 实现空导入追踪
3. 添加性能优化

### 长期
1. 支持更复杂的影响分析
2. 提供影响程度评估（高/中/低）
3. 生成可视化的依赖图

## 使用示例

### 在 ripples 中使用
```bash
# 分析常量变更的影响
./ripples -repo ~/project -old HEAD~1 -new HEAD -verbose

# 输出会包含常量和变量的影响
受影响的服务: 2
服务 1: api-server
调用链:
  -> main (main)
  -> HandleRequest
  -> ValidateConfig (使用了 MaxRetries 常量)
```

### 编程方式使用
```go
ctx := context.Background()
tracer, _ := ripplesapi.NewDirectTracer(ctx, "./project")
defer tracer.Close()

// 追踪常量到 main
pos := ripplesapi.Position{
    Filename: "config/const.go",
    Line:     10,
    Column:   7,
}

paths, _ := tracer.TraceReferencesToMain(pos, "MaxRetries")
for _, path := range paths {
    fmt.Printf("Binary: %s\n", path.BinaryName)
    for _, node := range path.Path {
        fmt.Printf("  -> %s.%s\n", node.PackagePath, node.FunctionName)
    }
}
```

## 技术要点

### 为什么需要 findContainingFunction
gopls 的 References API 返回引用位置，但不直接返回包含函数。我们需要：
1. 先用 PrepareCallHierarchy 尝试（快速，但只在函数声明处有效）
2. 如果失败，解析 AST 遍历查找包含的函数声明

### 为什么需要 findFunctionDeclaration
找到引用所在函数后，需要该函数的声明位置才能调用 TraceToMain，因为：
1. TraceToMain 需要在函数名处调用 PrepareCallHierarchy
2. 引用位置在函数体内，不是函数声明处

## 贡献者

- 实现日期: 2025-11-22
- 主要改动:
  - golang-tools: +160 行
  - ripples: +20 行
  - 测试代码: +80 行
