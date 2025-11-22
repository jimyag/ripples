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
- 接口方法和回调函数可能报告不准确

## 开发

```bash
# 构建
go build -o ripples

# 测试
go test ./...
```

## License

MIT
