# init 函数和空导入追踪功能实现总结

## 实现概述

成功扩展 ripples 以支持 init 函数和空导入（_ import）的变更追踪，通过使用 gopls 的 workspace 包导入分析。

## 实现的功能

### 1. golang-tools 新增 API

在 `golang-tools/gopls/internal/ripplesapi/tracer.go` 中添加了以下方法：

#### FindMainPackagesImporting
```go
func (t *DirectTracer) FindMainPackagesImporting(targetPkgPath string) ([]CallPath, error)
```
- 查找所有导入指定包的 main 包（直接或间接）
- 使用 `snapshot.LoadMetadataGraph(ctx)` 加载完整的 workspace 包图
- 递归检查包依赖关系
- 返回所有受影响的 main 包（服务）

#### importsPackage
```go
func (t *DirectTracer) importsPackage(graph *metadata.Graph, meta *metadata.Package, targetPkgPath string) bool
```
- 检查一个包是否导入了目标包
- 使用递归算法检查直接和间接依赖

#### importsPackageRecursive
```go
func (t *DirectTracer) importsPackageRecursive(graph *metadata.Graph, meta *metadata.Package, targetPkgPath string, visited map[metadata.PackageID]bool) bool
```
- 递归检查包导入关系
- 使用 visited map 避免循环依赖导致的无限递归

### 2. ripples 集成

#### 更新 direct_tracer.go
```go
// TraceToMain 现在支持 init 函数和空导入
func (t *DirectCallTracer) TraceToMain(symbol *parser.Symbol) ([]CallPath, error) {
    switch symbol.Kind {
    case parser.SymbolKindInit:
        // Init 函数: 查找所有导入该包的 main 包
        apiPaths, err = t.tracer.FindMainPackagesImporting(symbol.PackagePath)

    case parser.SymbolKindImport:
        // 空导入: 提取导入的包路径，然后查找所有导入它的 main 包
        if importExtra, ok := symbol.Extra.(parser.ImportExtra); ok {
            if importExtra.IsBlankImport() {
                apiPaths, err = t.tracer.FindMainPackagesImporting(importExtra.Path)
            }
        }
    }
}
```

#### 更新 lsp_analyzer.go
```go
// 添加了 isSupportedSymbolKind 支持
func isSupportedSymbolKind(kind parser.SymbolKind) bool {
    switch kind {
    case parser.SymbolKindInit,
         parser.SymbolKindImport:
        return true
    }
}
```

## 工作原理

### init 函数追踪流程

```
1. 用户修改 init 函数
   ↓
2. ripples 检测到 init 函数变更，获取其包路径
   ↓
3. 调用 FindMainPackagesImporting(packagePath)
   ↓
4. gopls 加载完整的 workspace 元数据图
   ↓
5. 遍历所有 main 包
   ↓
6. 对每个 main 包递归检查是否导入目标包
   ↓
7. 返回所有导入该包的 main 包
   ↓
8. 报告: 所有这些服务受影响
```

### 空导入追踪流程

```
1. 用户修改空导入 (如 _ "database/sql/driver")
   ↓
2. ripples 检测到空导入变更
   ↓
3. 验证是空导入 (alias == "_")
   ↓
4. 提取导入的包路径
   ↓
5. 调用 FindMainPackagesImporting(importedPackagePath)
   ↓
6. 后续流程与 init 函数相同
```

## 测试结果

### 测试项目结构

```
testdata/init-test/
├── cmd/
│   ├── server/         # 使用空导入引入 db
│   ├── api-server/     # 直接使用 db, cache, logger
│   └── worker/         # 只使用 db 和 config
├── internal/
│   ├── db/            # 依赖 config, 有 init 函数
│   ├── cache/         # 依赖 config, 有 init 函数
│   └── logger/        # 独立, 有 init 函数
└── pkg/
    └── config/        # 被多个包依赖, 有 init 函数
```

### init 函数测试输出

```
=== 测试 config.init ===
找到 3 个 main 包:
- server
- api-server
- worker
(所有服务都导入 config)

=== 测试 db.init ===
找到 3 个 main 包:
- server (通过空导入)
- api-server (直接导入)
- worker (直接导入)

=== 测试 cache.init ===
找到 2 个 main 包:
- server (直接导入)
- api-server (直接导入)
(worker 不导入 cache)

=== 测试 logger.init ===
找到 1 个 main 包:
- api-server
(只有 api-server 使用 logger)
```

### 空导入测试输出

```
=== 测试空导入 db 包 ===
找到 3 个 main 包:
- server
- api-server
- worker

=== 测试空导入 vs 普通导入 ===
✓ 空导入成功追踪
✗ 普通导入被拒绝 (符合预期)
```

## 支持的符号类型

### 当前已支持
- ✅ 函数 (原有功能)
- ✅ 常量 (2025-11-22)
- ✅ 全局变量 (2025-11-22)
- ✅ init 函数 (2025-11-22)
- ✅ 空导入 (2025-11-22)

## 性能考虑

### LoadMetadataGraph 的性能
- gopls 内部会缓存元数据图
- 第一次加载可能需要几秒钟（取决于项目大小）
- 后续查询几乎是即时的

### 优化点
1. 使用 `LoadMetadataGraph` 而不是 `MetadataGraph` 确保所有包都被加载
2. 递归检查使用 visited map 避免重复检查
3. 复用同一个 snapshot，避免重复创建

## 局限性

### 当前限制
1. **只检测包级别的导入关系**
   - 无法检测运行时动态加载的包
   - 无法检测通过 plugin 加载的包

2. **空导入限制**
   - 只支持空导入 (`_`)
   - 普通导入不会被追踪（它们不影响运行时行为）

3. **间接影响**
   - 如果 A 导入 B，B 导入 C，当 C 的 init 变更时，A 和 B 都会被报告为受影响
   - 这是保守但正确的做法

## 下一步计划

### 短期
1. ✅ 完成 init 函数追踪
2. ✅ 完成空导入追踪
3. 优化性能（如果发现瓶颈）

### 中期
1. 支持结构体字段变更
2. 支持接口方法变更

### 长期
1. 提供影响程度评估（高/中/低）
2. 生成可视化的依赖图

## 使用示例

### 在 ripples 中使用

```bash
# 分析 init 函数和空导入变更的影响
./ripples -repo ~/project -old HEAD~1 -new HEAD -verbose

# 输出会包含 init 函数和空导入的影响
受影响的服务: 3
服务 1: api-server
  原因: 导入了变更的包 (直接或间接)

服务 2: worker
  原因: 导入了变更的包 (直接或间接)

服务 3: server
  原因: 通过空导入引入了变更的包
```

### 编程方式使用

```go
ctx := context.Background()
tracer, _ := ripplesapi.NewDirectTracer(ctx, "./project")
defer tracer.Close()

// 追踪 init 函数
paths, _ := tracer.FindMainPackagesImporting("example.com/myproject/pkg/config")
for _, path := range paths {
    fmt.Printf("Service: %s\n", path.BinaryName)
}
```

## 技术要点

### 为什么使用 LoadMetadataGraph

初始实现使用 `MetadataGraph()` 返回空结果，因为 gopls 的 snapshot 在创建时不会自动加载所有包。
使用 `LoadMetadataGraph(ctx)` 会触发完整的 workspace 加载，确保所有包都被正确加载。

### 为什么 init 函数和空导入使用相同的逻辑

空导入的主要（几乎唯一）用途是触发包的 init 函数。因此：
- 当 init 函数变更 → 影响所有导入该包的服务
- 当空导入变更 → 影响所有导入该包的服务（因为会影响 init 执行）

两者在影响分析上是等价的，所以复用同一个实现。

### 为什么不支持普通导入

普通导入本身不会影响运行时行为，只有在：
1. 使用导入包的函数/常量/变量时
2. 导入包有 init 函数时

才会产生影响。这些情况已经被其他追踪类型覆盖了。

## 贡献者

- 实现日期: 2025-11-22
- 主要改动:
  - golang-tools: +100 行 (FindMainPackagesImporting 相关)
  - ripples: +30 行 (集成代码)
  - 测试代码: +200 行 (两个测试套件)
  - 文档: +400 行

## 参考文档

- [EXTENDED_SUPPORT.md](EXTENDED_SUPPORT.md) - 扩展支持设计文档
- [CONSTANT_VARIABLE_SUPPORT.md](CONSTANT_VARIABLE_SUPPORT.md) - 常量变量追踪实现
- [CLAUDE.md](../CLAUDE.md) - 项目整体架构文档
