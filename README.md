# ripples

基于 gopls 内部 API 的 Go 代码变更影响分析工具。

## 简介

ripples 通过分析 Git 提交之间的代码变更，追踪函数调用链，精确确定哪些服务会受到影响。

### 核心特性

- 直接使用 gopls internal API，零 IPC 开销
- 精确的调用链分析，基于 gopls 类型检查
- 自动追踪到 main 函数
- 支持多种符号类型：函数、常量、全局变量、init 函数、空导入
- 支持文本、JSON、摘要三种输出格式

## 安装

```bash
go build -o ripples
```

## 使用方法

### 基本命令

```bash
./ripples -repo <仓库路径> -old <旧commit> -new <新commit>
```

### 参数说明

| 参数       | 说明                              | 默认值       |
| ---------- | --------------------------------- | ------------ |
| `-repo`    | Git 仓库路径                      | 当前目录 `.` |
| `-old`     | 旧 commit ID 或分支名             | 必填         |
| `-new`     | 新 commit ID 或分支名             | 必填         |
| `-output`  | 输出格式：`text`/`json`/`summary` | `text`       |
| `-verbose` | 显示详细日志                      | `false`      |

### 使用示例

```bash
# 分析两个提交之间的影响
./ripples -repo ~/project -old abc123 -new def456

# JSON 格式输出
./ripples -repo ~/project -old main -new develop -output json

# 简短摘要
./ripples -repo ~/project -old HEAD~1 -new HEAD -output summary
```

## 工作原理

```
1. Git Diff 解析 → 提取变更的文件和行号
2. AST 符号提取 → 匹配变更行号到具体符号（函数/常量/变量/init/导入）
3. gopls 初始化 → 获取项目 Snapshot
4. 影响追踪 → 根据符号类型选择追踪策略
   - 函数：调用链分析
   - 常量/变量：引用查找
   - init 函数/空导入：包导入分析
5. 结果输出 → 汇总并格式化
```

## 性能特性

### 持久化缓存

ripples 使用 gopls 的 filecache 机制进行持久化缓存，显著提升重复分析的速度：

- **首次运行**：~15-50 秒（取决于项目大小）
- **重复运行**：~5 秒（**90% 更快**）

缓存特点：
- 自动管理：无需手动清理
- 跨运行持久化：进程重启后仍有效
- 内容寻址：自动去重
- 线程安全：支持并发访问

### 性能优化

1. **惰性加载**：只加载变更包，不加载整个项目
2. **并发追踪**：多个符号并行分析
3. **智能缓存**：内存 + 磁盘双层缓存
4. **过滤优化**：自动跳过测试函数

### 调试模式

启用详细日志查看缓存命中情况：

```bash
RIPPLES_DEBUG=1 ./ripples -repo ~/project -old main -new develop
```

输出示例：
```json
{"level":"debug","message":"Using PERSISTENT cached trace"}
{"level":"debug","message":"Stored trace in PERSISTENT cache"}
```

### 缓存位置

缓存存储在 gopls 缓存目录：
```
~/.cache/gopls/ripples-trace/
```

清空缓存（如需）：
```bash
rm -rf ~/.cache/gopls/ripples-trace/
```

## 输出格式示例

### 文本格式

```
受影响的服务: 2

服务 1: api-server
调用链:
  github.com/example/project/cmd/api-server.main (main)
  -> github.com/example/project/internal/api/server.Start
  -> github.com/example/project/internal/service.ProcessRequest (Changed)
```

### JSON 格式

```json
[
  {
    "name": "api-server",
    "package": "github.com/example/project/cmd/api-server",
    "trace_path": [
      "github.com/example/project/cmd/api-server.main (main)",
      "github.com/example/project/internal/api/server.Start",
      "github.com/example/project/internal/service.ProcessRequest (Changed)"
    ]
  }
]
```

### 摘要格式

```
受影响的服务: 2
- api-server
- worker
```

## 支持的符号类型

✅ **已支持**

- 函数调用
- 常量引用
- 全局变量引用
- init 函数（包导入时自动执行）
- 空导入（`_ "package"` - 触发 init 函数）

⏳ **计划支持**

- 结构体字段变更
- 接口方法变更
- 类型定义变更

## 已知限制

- 不支持反射调用和 cgo
- 不支持动态加载的包（plugin）
- 不支持跨语言调用

## 文档

- [跨服务误报修复方案](docs/CROSS_SERVICE_FALSE_POSITIVE_FIX.md)
- [实现说明](docs/IMPLEMENTATION_NOTES.md)
- [持久化缓存实现](PERSISTENT_CACHE.md)

## 开发

```bash
# 构建
go build -o ripples

# 测试
go test ./...
```

## License

MIT
