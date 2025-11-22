package analyzer

import (
	"context"
	"fmt"

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
	var results []AffectedBinary
	seenBinaries := make(map[string]bool)

	for _, change := range changes {
		// Skip non-function symbols (types, constants, variables, packages)
		// LSP call hierarchy only works for functions
		if change.Symbol.Kind != parser.SymbolKindFunction {
			continue
		}

		// Convert ChangedSymbol to parser.Symbol
		symbol := &parser.Symbol{
			Name:        change.Symbol.Name,
			Kind:        change.Symbol.Kind,
			Position:    change.Symbol.Position,
			PackagePath: change.Symbol.PackagePath,
		}

		// Trace to main functions
		paths, err := a.tracer.TraceToMain(symbol)
		if err != nil {
			fmt.Printf("Warning: failed to trace symbol %s: %v\n", symbol.Name, err)
			continue
		}

		// Convert paths to AffectedBinary
		for _, path := range paths {
			if seenBinaries[path.BinaryName] {
				continue
			}
			seenBinaries[path.BinaryName] = true

			// Format path strings with package information
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

			results = append(results, AffectedBinary{
				Name:      path.BinaryName,
				PkgPath:   extractPkgPath(path.MainURI),
				TracePath: pathStrs,
			})
		}
	}

	return results, nil
}

// extractPkgPath extracts package path from URI
func extractPkgPath(uri string) string {
	// URI format: file:///path/to/project/cmd/servicename/main.go
	// Extract: github.com/qbox/las/cmd/servicename
	// This is a simplified implementation
	return uri // TODO: implement proper extraction
}
