package main

import (
	"context"
	"errors"
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

	cache, err := snapshot.DefaultCache()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "initialize cache: %v\n", err)
		return 1
	}

	started := time.Now()
	analyzer := impact.NewAnalyzer(cache)
	analysis, err := analyzer.AnalyzeDetailed(context.Background(), *repoPath, *oldCommit, *newCommit)
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
	return 0
}
