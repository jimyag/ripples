package analyzer

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
	seenChanges := make(map[string]bool)
	for _, change := range changes {
		if !IsSupportedChange(change) {
			if change.Symbol.Kind != parser.SymbolKindStruct &&
				change.Symbol.Kind != parser.SymbolKindInterface &&
				change.Symbol.Kind != parser.SymbolKindType &&
				change.Symbol.Kind != parser.SymbolKindImport {
				fmt.Fprintf(os.Stderr, "Info: symbol kind %v not yet supported, skipping %s\n",
					change.Symbol.Kind, change.Symbol.Name)
			}
			continue
		}
		key := fmt.Sprintf("%s:%d:%d:%s:%s",
			change.Symbol.Position.Filename,
			change.Symbol.Position.Line,
			change.Symbol.Position.Column,
			change.Symbol.Kind,
			change.Symbol.Name,
		)
		if seenChanges[key] {
			continue
		}
		seenChanges[key] = true
		supportedChanges = append(supportedChanges, change)
	}

	if len(supportedChanges) == 0 {
		return nil, nil
	}

	// Collect results
	var affectedBinaries []AffectedBinary
	seenBinaries := make(map[string]bool)

	for _, change := range supportedChanges {
		symbol := &parser.Symbol{
			Name:        change.Symbol.Name,
			Kind:        change.Symbol.Kind,
			Position:    change.Symbol.Position,
			PackagePath: change.Symbol.PackagePath,
			Extra:       change.Symbol.Extra,
		}
		serviceID := serviceIdentifier(symbol.PackagePath)
		if serviceID != "" && seenBinaries[serviceID] {
			continue
		}
		if os.Getenv("RIPPLES_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "Debug: tracing %s %s at %s:%d:%d\n",
				symbol.Kind, symbol.Name, symbol.Position.Filename, symbol.Position.Line, symbol.Position.Column)
		}

		paths, err := a.tracer.TraceToMain(symbol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to trace symbol %s: %v\n", symbol.Name, err)
			continue
		}

		for _, path := range paths {
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
				PkgPath:   a.extractPkgPath(path.MainURI),
				TracePath: pathStrs,
			})
		}
	}

	return affectedBinaries, nil
}

func serviceIdentifier(pkgPath string) string {
	parts := strings.Split(pkgPath, "/")
	for i, part := range parts {
		if part == "internal" || part == "services" || part == "apps" || part == "api" {
			if i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}
	return ""
}

// extractPkgPath extracts package path from a main file URI.
func (a *LSPImpactAnalyzer) extractPkgPath(uri string) string {
	filename := uri
	if strings.HasPrefix(uri, "file://") {
		parsed, err := url.Parse(uri)
		if err == nil {
			filename = parsed.Path
		}
	}

	absRoot, err := filepath.Abs(a.rootPath)
	if err != nil {
		return uri
	}

	absFile, err := filepath.Abs(filename)
	if err != nil {
		return uri
	}

	rel, err := filepath.Rel(absRoot, filepath.Dir(absFile))
	if err != nil || strings.HasPrefix(rel, "..") {
		return uri
	}

	modulePath := readModulePath(absRoot)
	if modulePath == "" {
		return filepath.ToSlash(rel)
	}
	if rel == "." {
		return modulePath
	}
	return modulePath + "/" + filepath.ToSlash(rel)
}

// HasSupportedChanges reports whether at least one change can be traced.
func HasSupportedChanges(changes []ChangedSymbol) bool {
	for _, change := range changes {
		if IsSupportedChange(change) {
			return true
		}
	}
	return false
}

// IsSupportedChange checks whether this concrete change can be traced.
func IsSupportedChange(change ChangedSymbol) bool {
	if change.Symbol.Kind == parser.SymbolKindImport {
		extra, ok := change.Symbol.Extra.(parser.ImportExtra)
		return ok && extra.IsBlankImport()
	}
	return IsSupportedSymbolKind(change.Symbol.Kind)
}

// IsSupportedSymbolKind checks if a symbol kind is supported for tracing.
func IsSupportedSymbolKind(kind parser.SymbolKind) bool {
	switch kind {
	case parser.SymbolKindFunction,
		parser.SymbolKindConstant,
		parser.SymbolKindVariable,
		parser.SymbolKindType,
		parser.SymbolKindTypeAlias,
		parser.SymbolKindStruct,
		parser.SymbolKindStructField,
		parser.SymbolKindInterface,
		parser.SymbolKindInit,
		parser.SymbolKindImport:
		return true
	default:
		return false
	}
}

func isSupportedSymbolKind(kind parser.SymbolKind) bool {
	return IsSupportedSymbolKind(kind)
}

func readModulePath(rootPath string) string {
	content, err := os.ReadFile(filepath.Join(rootPath, "go.mod"))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}
