# 跨服务误报修复方案

## 问题描述

在真实项目测试中发现，当修改 `internal/bill` 包中的函数时，ripples 错误地报告了 rfs、compliance、daemon 等其他服务也会受到影响。

### 根本原因

Gopls 的 `IncomingCalls` API 对接口方法返回所有实现者的调用点，而不区分具体类型。

示例：

```go
// pkg/grace/grace.go
type Server interface { Run() error }
func Run(s Server) error { return s.Run() }

// internal/bill/server.go
type BillServer struct{}
func (s *BillServer) Run() error { /* bill logic */ }

// internal/rfs/server.go
type RFSServer struct{}
func (s *RFSServer) Run() error { /* rfs logic */ }
```

当查询 `BillServer.Run` 的调用者时：
1. gopls 返回 `pkg/grace.Run` (正确)
2. 追踪 `grace.Run` 的调用者，返回所有调用 `grace.Run` 的地方：
   - `cmd/bill/main` (正确)
   - `cmd/rfs/main` (错误 - 这调用的是 RFSServer)

## 解决方案：基于调用路径的过滤

### 核心思想

不尝试"推断服务边界"（需要硬编码目录结构），而是利用已经在追踪的调用路径，计算新调用者与当前路径的相关性。

### 实现逻辑

**位置**: `golang-tools/gopls/internal/ripplesapi/tracer.go:850-952`

```go
// 1. 检测接口方法
isLikelyInterfaceMethod := strings.Contains(currentItem.Name, ".") ||
    (currentItem.Kind == protocol.Method)

// 2. 检测调用者多样性
callerPackages := make(map[string]bool)
for _, call := range incomingCalls {
    pkg := extractPackageFromItem(call.From)
    callerPackages[pkg] = true
}

// 只有多个不同调用者时才需要过滤
if len(callerPackages) <= 1 {
    return incomingCalls
}

// 3. 为每个调用者打分
for _, call := range incomingCalls {
    callerPkg := extractPackageFromItem(call.From)
    score := 0

    // 分数1: 与当前路径中的包有多少共同前缀
    for _, node := range currentPath {
        prefixLen := longestCommonPrefix(node.PackagePath, callerPkg)
        score += prefixLen / 10
    }

    // 分数2: 与当前包的直接关系
    directPrefixLen := longestCommonPrefix(currentPkg, callerPkg)
    score += directPrefixLen / 5

    // 分数3: 是否已在路径中（强关联）
    for _, node := range currentPath {
        if node.PackagePath == callerPkg {
            score += 100
        }
    }
}

// 4. 过滤低分调用者
// 只保留得分在最高分 50% 以上的调用者
const SCORE_THRESHOLD_RATIO = 0.5
```

### 为什么通用

1. **基于实际数据**: 使用正在追踪的 `currentPath`
2. **纯字符串操作**: 计算包路径的前缀长度
3. **基于 Go 本质**: 同一模块的包自然共享更长的路径前缀
4. **Go 标准约定**: 依赖 Go 社区标准目录约定（`/pkg/`, `/internal/`, `/cmd/` 等）

适用于遵循 Go 标准约定的项目结构：
- 标准 Go: `repo/pkg/common`, `repo/internal/bill`
- 微服务: `repo/pkg/shared`, `repo/services/billing/internal`
- Monorepo: `repo/pkg/lib`, `repo/apps/payment/internal`

**前提条件**: 项目需遵循 Go 社区标准目录约定

## 验证结果

### 真实项目测试结果

**问题场景**: 修改服务 A 的 internal 包，错误报告服务 B、C、D 也受影响

**根本原因**: `startedInCommonPkg` 逻辑错误
- `isCommonPackage` 检查包路径是否包含 `/api/`、`/pkg/` 等
- 某些服务专有的 API 包（如 `api/serviceA/client`）被错误识别为共享包
- 当追踪经过真正的共享包（如 `pkg/grace.Run`）时，跳过了过滤
- gopls 返回所有服务的 main 函数（因为都调用共享包），全部被错误保留

**解决方案**: 修复 `isCommonPackage` + 只在当前项为共享包时跳过过滤

1. **修复 `isCommonPackage` 函数**
   - 原逻辑: 简单检查路径是否包含 `/api/`、`/pkg/` 等
   - 问题: `api/serviceA/client` 被错误识别为共享包
   - 新逻辑: 检查这些目录后是否还有 `/internal/` 或 `/cmd/`
   - 结果: `pkg/grace` → 共享 ✓，`api/serviceA/client` → 非共享 ✓

2. **智能过滤策略**
   - 检查 `currentItem`（正在追踪的函数）所在的包
   - 当前项在共享包（如 `pkg/grace.Run`）→ 不过滤，保留所有调用者
   - 当前项在服务包（如 `internal/serviceA/server.Run`）→ 应用路径评分过滤
   - 效果: 共享包正确影响所有服务 + 防止跨服务误报

### 单元测试

所有测试通过：
- TestSharedPackageChange: 共享包修改影响所有使用者
- TestInternalPackageNotCrossingShared: Internal 包不跨服务
- TestSharedPackageInternalCalls: 共享包内部调用
- TestInternalViaSharedPackage: Internal 通过共享包调用
- TestSharedInterfaceFunctionNotCrossingService: 共享包接口函数影响所有调用者
- TestPathBasedFilteringScores: 路径过滤防止跨服务误报

### Gopls API 注意事项

Gopls的 `IncomingCalls` API 对接口方法返回所有实现者的调用点：

- 接口方法（如 `service-a.Run()`）会返回所有调用接口的地方（包括其他服务）
- **重要**: gopls 返回的 `call.From.Detail` 字段对接口调用不准确
- **解决**: 使用 `call.From.URI` 通过 `snapshot.MetadataForFile` 获取准确的包路径
- 路径过滤使用准确的包路径进行评分，过滤掉不相关的服务

### 调试日志

ripples 使用 zerolog 记录调试日志。默认情况下日志是关闭的。可以通过环境变量启用：

```bash
RIPPLES_DEBUG=1 ./ripples -repo ~/project -old abc123 -new def456
```

## 工作原理示例

跨服务接口调用场景：

```
当前追踪路径:
  internal/serviceA/handler.ProcessData
  -> internal/serviceA/server.Run

gopls 返回的调用者:
  1. pkg/grace.Run (从 cmd/serviceA 调用)
  2. pkg/grace.Run (从 cmd/serviceB 调用)
```

打分结果：
- 调用者1 与路径包前缀: `example.com/project/internal/serviceA` (长前缀) -> 高分
- 调用者2 与路径包前缀: `example.com/project/` (短前缀) -> 低分

过滤掉低分的调用者2，避免误报。

## 总结

通过基于调用路径的相关性评分 + Go 标准约定，实现了通用的接口歧义消除。

核心优势：
- 基于实际追踪数据的路径评分
- 纯数学计算（包路径前缀匹配）
- 依赖 Go 社区标准约定（`/pkg/`, `/internal/`, `/cmd/`）
- 适用于遵循 Go 标准约定的项目

技术细节：
- 路径评分机制：计算调用者与当前路径的包前缀相似度
- 共享包识别：基于 Go 标准目录约定判断包是否为共享包
- 智能过滤：当前项在共享包时不过滤，在服务包时应用评分过滤
