package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"go/token"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

// 这个示例演示如何使用 LSP 客户端查找函数的调用链
func main() {
	ctx := context.Background()

	// 1. 配置要分析的项目路径
	projectPath := "/Users/jimyag/src/work/github/las"

	// 2. 创建 LSP tracer
	fmt.Printf("正在启动 gopls LSP 服务器...\n")
	tracer, err := lsp.NewCallChainTracer(ctx, projectPath)
	if err != nil {
		log.Fatalf("创建 tracer 失败: %v", err)
	}
	defer tracer.Close()

	fmt.Println("✅ gopls 已启动并初始化")

	// 3. 读取文件内容并查找函数位置
	targetFile := projectPath + "/internal/bill/server/service/resource_collector.go"
	content, err := os.ReadFile(targetFile)
	if err != nil {
		log.Fatalf("读取文件失败: %v", err)
	}

	// 查找函数定义的位置
	lines := strings.Split(string(content), "\n")
	var funcLine, funcCol int
	for i, line := range lines {
		if strings.Contains(line, "func collectSnapshotRecords") ||
			strings.Contains(line, "func (") && strings.Contains(line, "collectSnapshotRecords") {
			funcLine = i + 1 // 1-based
			// 找到 "collectSnapshotRecords" 在行中的位置
			funcCol = strings.Index(line, "collectSnapshotRecords") + 1 // 1-based
			fmt.Printf("找到函数定义: Line %d, Col %d\n", funcLine, funcCol)
			break
		}
	}

	if funcLine == 0 {
		log.Fatal("未找到函数定义")
	}

	// 4. 创建符号
	symbol := &parser.Symbol{
		Name: "collectSnapshotRecords",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: targetFile,
			Line:     funcLine,
			Column:   funcCol,
		},
		PackagePath: "github.com/qbox/las/internal/bill/server/service",
	}

	// 5. 追踪调用链
	fmt.Printf("\n正在追踪 '%s' 的调用链...\n", symbol.Name)
	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		log.Fatalf("追踪失败: %v", err)
	}

	// 6. 显示结果
	fmt.Printf("\n找到 %d 个受影响的服务:\n", len(paths))
	for i, callPath := range paths {
		fmt.Printf("\n服务 %d: %s\n", i+1, callPath.BinaryName)
		fmt.Printf("调用链:\n")
		for j, funcName := range callPath.Path {
			if j == 0 {
				fmt.Printf("  🏁 %s (main)\n", funcName)
			} else if j == len(callPath.Path)-1 {
				fmt.Printf("  🚀 %s (Changed)\n", funcName)
			} else {
				fmt.Printf("  ⬇️  %s\n", funcName)
			}
		}
	}

	fmt.Println("\n✅ LSP 客户端测试成功!")
}
