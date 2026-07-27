package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	_ "github.com/jimmicro/version"

	"github.com/jimyag/ripples/internal/impact"
	"github.com/jimyag/ripples/internal/output"
	"github.com/jimyag/ripples/internal/snapshot"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ripples", flag.ContinueOnError)
	flags.SetOutput(stderr)

	repoPath := flags.String("repo", ".", "Git 仓库路径")
	oldCommit := flags.String("old", "", "旧 commit ID 或 ref（必填）")
	newCommit := flags.String("new", "", "新 commit ID 或 ref（必填）")
	outputType := flags.String("output", "simple", "输出格式: simple, text, json, summary")
	verbose := flags.Bool("verbose", false, "显示分析耗时")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if *oldCommit == "" || *newCommit == "" {
		fmt.Fprintln(stderr, "错误: 必须指定 -old 和 -new 参数")
		flags.Usage()
		return 1
	}

	cache, err := snapshot.DefaultCache()
	if err != nil {
		fmt.Fprintf(stderr, "初始化缓存失败: %v\n", err)
		return 1
	}

	started := time.Now()
	analyzer := impact.NewAnalyzer(cache)
	results, err := analyzer.Analyze(context.Background(), *repoPath, *oldCommit, *newCommit)
	if err != nil {
		fmt.Fprintf(stderr, "分析失败: %v\n", err)
		return 1
	}

	reporter := output.NewReporter(stdout, results)
	if err := reporter.Print(*outputType); err != nil {
		fmt.Fprintf(stderr, "输出失败: %v\n", err)
		return 1
	}
	if *verbose {
		fmt.Fprintf(stderr, "分析完成: %d 个受影响包，耗时 %s\n", len(results), time.Since(started))
	}
	return 0
}
