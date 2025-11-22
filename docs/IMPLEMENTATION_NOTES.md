# 实现说明

## 核心功能

Ripples 通过 LSP (Language Server Protocol) 分析 Go 代码变更的影响范围，追踪从变更点到 main 函数的调用链。

## 主要组件

### 1. Git Diff 分析 (`internal/git`)

解析 git diff 输出，识别变更的文件和行号。

### 2. 符号解析 (`internal/parser`)

将变更的代码位置映射到具体的 Go 符号（函数、方法、常量、变量、init 函数）。

支持的符号类型：
- 函数和方法
- 常量和变量
- Init 函数（自动执行）
- 空白导入（blank import）

### 3. 调用链追踪 (`internal/lsp`, `golang-tools/gopls/internal/ripplesapi`)

使用 gopls 内部 API 追踪符号的调用链：
- `PrepareCallHierarchy`: 准备调用层次结构
- `IncomingCalls`: 查找调用者
- `References`: 查找引用（用于常量/变量）

#### 关键实现：接口歧义过滤

**文件**: `golang-tools/gopls/internal/ripplesapi/tracer.go`

**问题**: gopls 对接口方法返回所有实现者的调用点，导致跨服务误报。

**解决**: 基于调用路径的相关性评分过滤不相关的调用者。

```go
func (t *DirectTracer) filterAmbiguousInterfaceCalls(...) {
    // 只对接口方法且有多个调用者时进行过滤

    // 为每个调用者计算相关性得分：
    // 1. 与当前路径包的前缀匹配长度
    // 2. 与当前包的直接关系
    // 3. 是否已在追踪路径中

    // 只保留高分调用者（最高分的 50% 以上）
}
```

优势：
- 完全不依赖目录结构（无硬编码）
- 基于实际追踪的调用路径
- 适用于任何项目组织方式

### 4. 特殊场景处理

#### Init 函数

Init 函数在包被导入时自动执行，无显式调用链。

处理方式：
- 找到包含 init 的包
- 查找所有（直接或间接）导入该包的 main 包
- 使用 gopls 的元数据图遍历依赖关系

#### 空白导入

```go
import _ "database/sql/driver"
```

处理方式：
- 检测空白导入语句
- 视为对该包 init 函数的隐式依赖
- 追踪到所有导入该包的 main 包

#### 常量和变量

常量和变量没有调用链，只有引用关系。

处理方式：
- 使用 `gopls.References` 找到所有引用
- 对每个引用点，找到包含该引用的函数
- 从该函数继续追踪调用链到 main

### 5. 去重和输出 (`internal/output`)

按二进制名称去重，格式化输出调用链。

## Case 1 和 Case 2 检查

**文件**: `golang-tools/gopls/internal/ripplesapi/tracer.go:271-295`

```go
// Case 1: 从 internal 开始，到达 common 包时停止
if callerIsCommon && !startedInCommonPkg {
    continue
}

// Case 2: 从 common 开始，经过 internal，又回到 common 时停止
if callerIsCommon && startedInCommonPkg {
    hasInternal := false
    for _, node := range currentPath {
        if strings.Contains(node.PackagePath, "/internal/") {
            hasInternal = true
            break
        }
    }
    if hasInternal {
        continue
    }
}
```

这两个检查配合接口歧义过滤，共同防止跨服务误报。

## 性能考虑

1. **访问标记**: 使用 visited map 防止循环追踪
2. **二进制去重**: 使用 seenBinaries map 避免重复报告
3. **增量分析**: 只分析变更的符号

## 限制

1. **反射调用**: 无法追踪通过反射的调用
2. **跨语言调用**: 只支持纯 Go 代码
3. **动态导入**: 不支持运行时动态加载
4. **复杂控制流**: 静态分析有局限

## 测试

测试数据位于 `testdata/` 目录：
- `constant-test/`: 常量变更测试
- `init-test/`: Init 函数测试
- `shared-package-test/`: 跨服务场景测试

运行测试：
```bash
go test ./internal/analyzer/...
```
