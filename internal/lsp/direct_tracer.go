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
	// Skip non-function symbols
	if symbol.Kind != parser.SymbolKindFunction {
		return nil, fmt.Errorf("symbol %s is not a function (kind: %v), skipping", symbol.Name, symbol.Kind)
	}

	// Convert position
	pos := ripplesapi.Position{
		Filename: symbol.Position.Filename,
		Line:     symbol.Position.Line,
		Column:   symbol.Position.Column,
	}

	// Call API
	apiPaths, err := t.tracer.TraceToMain(pos, symbol.Name)
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
