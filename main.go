package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jimmicro/version"

	"github.com/jimyag/ripples/internal/impact"
	"github.com/jimyag/ripples/internal/output"
	"github.com/jimyag/ripples/internal/snapshot"
)

func main() {
	// Cancel on Ctrl-C or CI job cancellation so temporary exports are removed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ripples", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: ripples -old <ref> -new <ref> [options]")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Analyze affected packages:")
		_, _ = fmt.Fprintln(stderr, "  ripples -repo . -old HEAD~1 -new HEAD")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Export the package impact graph:")
		_, _ = fmt.Fprintln(stderr, "  ripples -repo . -old origin/main -new HEAD -output dot > impact.dot")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Options:")
		flags.PrintDefaults()
	}

	repoPath := flags.String("repo", ".", "Go module directory inside a Git repository")
	oldCommit := flags.String("old", "", "old commit ID or ref (required)")
	newCommit := flags.String("new", "", "new commit ID or ref (required)")
	outputType := flags.String("output", "simple", "output format: simple, text, json, summary, or dot")
	prepare := flags.String("prepare", "", "shell command run in each exported revision before analysis, such as \"go generate ./...\"")
	tests := flags.Bool("tests", false, "also analyze _test.go files so test-only changes are reported")
	verbose := flags.Bool("verbose", false, "show analysis duration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if *oldCommit == "" || *newCommit == "" {
		_, _ = fmt.Fprintln(stderr, "error: -old and -new are required")
		flags.Usage()
		return 1
	}
	if err := output.CheckFormat(*outputType); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	cache, err := snapshot.DefaultCache()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "initialize cache: %v\n", err)
		return 1
	}

	started := time.Now()
	analyzer := impact.NewAnalyzer(cache)
	analyzer.Prepare = *prepare
	analyzer.Tests = *tests
	analysis, err := analyzer.AnalyzeDetailed(ctx, *repoPath, *oldCommit, *newCommit)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "analyze impact: %v\n", err)
		return 1
	}

	reporter := output.NewAnalysisReporter(stdout, analysis)
	if err := reporter.Print(*outputType); err != nil {
		_, _ = fmt.Fprintf(stderr, "write output: %v\n", err)
		return 1
	}
	if *verbose {
		_, _ = fmt.Fprintf(
			stderr,
			"analysis complete: %d affected packages in %s\n",
			len(analysis.Packages),
			time.Since(started),
		)
	}
	// The result is already complete; a failed cleanup only leaves old entries.
	if err := cache.Prune(cacheMaxAge, impact.CacheNamespaces()...); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: prune cache: %v\n", err)
	}
	return 0
}

// cacheMaxAge keeps snapshots that CI reads regularly, such as the main
// branch, while dropping those of short-lived revisions.
const cacheMaxAge = 7 * 24 * time.Hour
