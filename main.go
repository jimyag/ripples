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
	flag.StringVar(&outputType, "output", "text", "输出格式: text, json, summary")
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

	// 1. 初始化 Parser
	p := parser.NewParser()
	if err := p.LoadProject(repoPath); err != nil {
		fmt.Fprintf(os.Stderr, "加载项目失败: %v\n", err)
		os.Exit(1)
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

	// 2. 初始化 LSP Impact Analyzer
	ctx := context.Background()
	lspAnalyzer, err := analyzer.NewLSPImpactAnalyzer(ctx, repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化 LSP 分析器失败: %v\n", err)
		os.Exit(1)
	}
	defer lspAnalyzer.Close()

	if verbose {
		fmt.Println("✅ LSP 分析器已启动")
	}

	// 3. 检测变更
	cd := analyzer.NewChangeDetector(p, repoPath)
	changes, err := cd.DetectChanges(oldCommit, newCommit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "检测变更失败: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Printf("检测到 %d 个变更符号\n", len(changes))
	}

	// 4. 分析影响
	results, err := lspAnalyzer.Analyze(changes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "分析失败: %v\n", err)
		os.Exit(1)
	}

	if verbose {
		fmt.Printf("分析完成，耗时: %v\n", time.Since(startTime))
		fmt.Println()
	}

	// 4. 输出结果
	reporter := output.NewReporter(results)

	switch outputType {
	case "json":
		if err := reporter.PrintJSON(); err != nil {
			fmt.Fprintf(os.Stderr, "输出JSON失败: %v\n", err)
			os.Exit(1)
		}

	case "summary":
		reporter.PrintSummary()

	case "text":
		fallthrough
	default:
		reporter.PrintText()
	}

	// 5. 打印总耗时
	if verbose {
		fmt.Printf("\n总耗时: %v\n", time.Since(startTime))
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
