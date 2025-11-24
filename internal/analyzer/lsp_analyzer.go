package analyzer

import (
	"context"
	"fmt"
	"sync"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

// LSPImpactAnalyzer uses LSP client to analyze impact
type LSPImpactAnalyzer struct {
	tracer   *lsp.DirectCallTracer
	rootPath string
}

// NewLSPImpactAnalyzer creates a new LSP-based impact analyzer
func NewLSPImpactAnalyzer(ctx context.Context, rootPath string) (*LSPImpactAnalyzer, error) {
	tracer, err := lsp.NewDirectCallTracer(ctx, rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create LSP tracer: %w", err)
	}

	return &LSPImpactAnalyzer{
		tracer:   tracer,
		rootPath: rootPath,
	}, nil
}

// Close closes the analyzer
func (a *LSPImpactAnalyzer) Close() error {
	return a.tracer.Close()
}

// Analyze analyzes the impact of changed symbols
func (a *LSPImpactAnalyzer) Analyze(changes []ChangedSymbol) ([]AffectedBinary, error) {
	// Filter out unsupported symbols first
	var supportedChanges []ChangedSymbol
	for _, change := range changes {
		if !isSupportedSymbolKind(change.Symbol.Kind) {
			if change.Symbol.Kind != parser.SymbolKindStruct &&
				change.Symbol.Kind != parser.SymbolKindInterface &&
				change.Symbol.Kind != parser.SymbolKindType {
				fmt.Printf("Info: symbol kind %v not yet supported, skipping %s\n",
					change.Symbol.Kind, change.Symbol.Name)
			}
			continue
		}
		supportedChanges = append(supportedChanges, change)
	}

	if len(supportedChanges) == 0 {
		return nil, nil
	}

	// Concurrent processing
	type traceResult struct {
		paths []lsp.CallPath
		err   error
	}

	results := make(chan traceResult, len(supportedChanges))
	var wg sync.WaitGroup

	// Process symbols concurrently
	for _, change := range supportedChanges {
		wg.Add(1)
		go func(ch ChangedSymbol) {
			defer wg.Done()

			// Convert ChangedSymbol to parser.Symbol
			symbol := &parser.Symbol{
				Name:        ch.Symbol.Name,
				Kind:        ch.Symbol.Kind,
				Position:    ch.Symbol.Position,
				PackagePath: ch.Symbol.PackagePath,
				Extra:       ch.Symbol.Extra,
			}

			// Trace to main functions
			paths, err := a.tracer.TraceToMain(symbol)
			results <- traceResult{paths: paths, err: err}
		}(change)
	}

	// Close results channel when all goroutines complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	var affectedBinaries []AffectedBinary
	seenBinaries := make(map[string]bool)

	for res := range results {
		if res.err != nil {
			fmt.Printf("Warning: failed to trace symbol: %v\n", res.err)
			continue
		}

		for _, path := range res.paths {
			if seenBinaries[path.BinaryName] {
				continue
			}
			seenBinaries[path.BinaryName] = true

			// Format path strings
			var pathStrs []string
			for i, node := range path.Path {
				var formatted string
				if node.PackagePath != "" {
					formatted = fmt.Sprintf("%s.%s", node.PackagePath, node.FunctionName)
				} else {
					formatted = node.FunctionName
				}

				if i == 0 {
					pathStrs = append(pathStrs, fmt.Sprintf("%s (main)", formatted))
				} else if i == len(path.Path)-1 {
					pathStrs = append(pathStrs, fmt.Sprintf("%s (Changed)", formatted))
				} else {
					pathStrs = append(pathStrs, formatted)
				}
			}

			affectedBinaries = append(affectedBinaries, AffectedBinary{
				Name:      path.BinaryName,
				PkgPath:   extractPkgPath(path.MainURI),
				TracePath: pathStrs,
			})
		}
	}

	return affectedBinaries, nil
}

// extractPkgPath extracts package path from URI
func extractPkgPath(uri string) string {
	return uri // TODO: implement proper extraction
}

// isSupportedSymbolKind checks if a symbol kind is supported for tracing
func isSupportedSymbolKind(kind parser.SymbolKind) bool {
	switch kind {
	case parser.SymbolKindFunction,
		parser.SymbolKindConstant,
		parser.SymbolKindVariable,
		parser.SymbolKindInit,
		parser.SymbolKindImport:
		return true
	default:
		return false
	}
}
