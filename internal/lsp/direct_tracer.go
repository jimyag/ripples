package lsp

import (
	"context"
	"fmt"

	"github.com/jimyag/ripples/internal/parser"
	"golang.org/x/tools/gopls/pkg/ripplesapi"
)

// DirectCallTracer uses gopls internal packages via API for call hierarchy analysis
type DirectCallTracer struct {
	tracer   *ripplesapi.DirectTracer
	rootPath string
}

// NewDirectCallTracer creates a new DirectCallTracer
func NewDirectCallTracer(ctx context.Context, rootPath string) (*DirectCallTracer, error) {
	tracer, err := ripplesapi.NewDirectTracer(ctx, rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create direct tracer: %w", err)
	}

	return &DirectCallTracer{
		tracer:   tracer,
		rootPath: rootPath,
	}, nil
}

// Close releases resources
func (t *DirectCallTracer) Close() error {
	return t.tracer.Close()
}

// TraceToMain traces a symbol to all main functions that call it
func (t *DirectCallTracer) TraceToMain(symbol *parser.Symbol) ([]CallPath, error) {
	// Convert position
	pos := ripplesapi.Position{
		Filename: symbol.Position.Filename,
		Line:     symbol.Position.Line,
		Column:   symbol.Position.Column,
	}

	var apiPaths []ripplesapi.CallPath
	var err error

	// Handle different symbol kinds
	switch symbol.Kind {
	case parser.SymbolKindFunction:
		// Function: use existing TraceToMain
		apiPaths, err = t.tracer.TraceToMain(pos, symbol.Name)

	case parser.SymbolKindConstant, parser.SymbolKindVariable:
		// Constant/Variable: find references and trace containing functions
		apiPaths, err = t.tracer.TraceReferencesToMain(pos, symbol.Name)

	case parser.SymbolKindInit:
		// Init function: find all main packages that import this package
		// Init functions are automatically executed when a package is imported
		apiPaths, err = t.tracer.FindMainPackagesImporting(symbol.PackagePath)

	case parser.SymbolKindImport:
		// Blank import: same as init function - find all main packages that import this package
		// Blank imports are used to trigger init functions (e.g., database driver registration)
		// Get the imported package path from Extra
		if importExtra, ok := symbol.Extra.(parser.ImportExtra); ok {
			if importExtra.IsBlankImport() {
				// For blank imports, trace which main packages import this
				apiPaths, err = t.tracer.FindMainPackagesImporting(importExtra.Path)
			} else {
				// Non-blank imports are not tracked (they don't affect runtime behavior by themselves)
				return nil, fmt.Errorf("only blank imports (_ import) are supported for tracing")
			}
		} else {
			return nil, fmt.Errorf("import symbol missing ImportExtra information")
		}

	default:
		return nil, fmt.Errorf("symbol kind %v not yet supported for tracing", symbol.Kind)
	}

	if err != nil {
		return nil, err
	}

	// Convert results
	var paths []CallPath
	for _, ap := range apiPaths {
		var nodes []CallNode
		for _, an := range ap.Path {
			nodes = append(nodes, CallNode{
				FunctionName: an.FunctionName,
				PackagePath:  an.PackagePath,
			})
		}

		paths = append(paths, CallPath{
			BinaryName: ap.BinaryName,
			MainURI:    ap.MainURI,
			Path:       nodes,
		})
	}

	return paths, nil
}
