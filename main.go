package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jimyag/ripples/internal/analyzer"
	"github.com/jimyag/ripples/internal/output"
	"github.com/jimyag/ripples/internal/parser"
)

var (
	repoPath   string
	oldCommit  string
	newCommit  string
	outputType string
	verbose    bool
)

func init() {
	flag.StringVar(&repoPath, "repo", ".", "Git 仓库路径")
	flag.StringVar(&oldCommit, "old", "", "旧 commit ID (必填)")
	flag.StringVar(&newCommit, "new", "", "新 commit ID (必填)")
	flag.StringVar(&outputType, "output", "simple", "输出格式: simple, text, json, summary")
	flag.BoolVar(&verbose, "verbose", false, "详细输出")
}

func main() {
	flag.Parse()

	// 验证必填参数
	if oldCommit == "" || newCommit == "" {
		fmt.Println("错误: 必须指定 -old 和 -new 参数")
		flag.Usage()
		os.Exit(1)
	}

	// 打印开始信息
	if verbose {
		fmt.Printf("开始分析项目: %s\n", repoPath)
		fmt.Printf("比较: %s -> %s\n", oldCommit, newCommit)
		fmt.Println()
	}

	startTime := time.Now()

	// 1. 获取变更文件列表（用于优化 Parser 加载）
	if verbose {
		fmt.Println("⏱️  步骤 1/6: 检测变更文件...")
	}
	detectFilesStart := time.Now()
	diffContent, err := analyzer.GetGitDiffContent(repoPath, oldCommit, newCommit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取 git diff 失败: %v\n", err)
		os.Exit(1)
	}
	changedFiles := analyzer.ExtractChangedGoFiles(diffContent)
	if verbose {
		fmt.Printf("   ✅ 检测到 %d 个变更文件 (耗时: %v)\n", len(changedFiles), time.Since(detectFilesStart))
	}

	// 2. 初始化 Parser（只加载变更文件相关的包）
	if verbose {
		fmt.Println("\n⏱️  步骤 2/6: 初始化 Parser (只加载变更包)...")
	}
	parseStart := time.Now()
	p := parser.NewParser()
	if err := p.LoadChangedFiles(repoPath, changedFiles); err != nil {
		// 如果加载失败，回退到加载整个项目
		if verbose {
			fmt.Printf("   ⚠️  加载变更包失败，回退到加载整个项目: %v\n", err)
		}
		if err := p.LoadProject(repoPath); err != nil {
			fmt.Fprintf(os.Stderr, "加载项目失败: %v\n", err)
			os.Exit(1)
		}
	}
	if verbose {
		fmt.Printf("   ✅ Parser 初始化完成 (耗时: %v)\n", time.Since(parseStart))
	}

	// 获取当前模块名
	currentModule := getModulePath(repoPath)
	if currentModule == "" {
		// Fallback to package info
		pkgs := p.GetPackages()
		if len(pkgs) > 0 && pkgs[0].Module != nil {
			currentModule = pkgs[0].Module.Path
		}
	}

	if verbose {
		fmt.Printf("当前模块: %s\n", currentModule)
	}

	// 3. 检测变更符号
	if verbose {
		fmt.Println("\n⏱️  步骤 3/5: 检测变更符号...")
	}
	detectStart := time.Now()
	cd := analyzer.NewChangeDetector(p, repoPath)
	changes, err := cd.DetectChanges(oldCommit, newCommit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "检测变更失败: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Printf("   ✅ 检测到 %d 个变更符号 (耗时: %v)\n", len(changes), time.Since(detectStart))
	}

	var results []analyzer.AffectedBinary
	if !analyzer.HasSupportedChanges(changes) {
		if verbose {
			fmt.Println("   ℹ️  没有可追踪的变更符号，跳过 gopls 初始化")
		}
		reporter := output.NewReporter(results)
		printReport(reporter, outputType)
		os.Exit(0)
	}

	// 4. 初始化 LSP Impact Analyzer
	if verbose {
		fmt.Println("\n⏱️  步骤 4/5: 初始化 LSP 分析器 (gopls)...")
	}
	lspStart := time.Now()
	ctx := context.Background()
	lspAnalyzer, err := analyzer.NewLSPImpactAnalyzer(ctx, repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化 LSP 分析器失败: %v\n", err)
		os.Exit(1)
	}
	defer lspAnalyzer.Close()

	if verbose {
		fmt.Printf("   ✅ LSP 分析器初始化完成 (耗时: %v)\n", time.Since(lspStart))
	}

	// 5. 分析影响
	if verbose {
		fmt.Println("\n⏱️  步骤 5/5: 追踪调用链到 main 函数...")
	}
	analyzeStart := time.Now()
	results, err = lspAnalyzer.Analyze(changes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "分析失败: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Printf("   ✅ 调用链追踪完成 (耗时: %v)\n", time.Since(analyzeStart))
		fmt.Printf("   📊 发现 %d 个受影响的服务\n", len(results))
	}

	// 6. 输出结果
	if verbose {
		fmt.Println("\n输出结果...")
	}
	reporter := output.NewReporter(results)
	printReport(reporter, outputType)

	// 如果没有发现受影响的服务，返回非0退出码
	if len(results) == 0 {
		os.Exit(0) // 无影响也算成功
	}

	// 打印总耗时
	if verbose {
		fmt.Printf("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("⏱️  总耗时: %v\n", time.Since(startTime))
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	}
}

func printReport(reporter *output.Reporter, outputType string) {
	switch outputType {
	case "json":
		if err := reporter.PrintJSON(); err != nil {
			fmt.Fprintf(os.Stderr, "输出JSON失败: %v\n", err)
			os.Exit(1)
		}

	case "summary":
		reporter.PrintSummary()

	case "text":
		reporter.PrintText()

	case "simple":
		fallthrough
	default:
		reporter.PrintSimple()
	}
}

// getModulePath 从 go.mod 文件获取模块路径
func getModulePath(repoPath string) string {
	goModPath := filepath.Join(repoPath, "go.mod")
	content, err := os.ReadFile(goModPath)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}
