package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jimyag/ripples/internal/parser"
)

// CallChainTracer traces call chains using LSP
type CallChainTracer struct {
	client    *Client
	rootPath  string
	mainFuncs map[string]bool // URI -> is main function
}

// NewCallChainTracer creates a new call chain tracer
func NewCallChainTracer(ctx context.Context, rootPath string) (*CallChainTracer, error) {
	client, err := NewClient(ctx, rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create LSP client: %w", err)
	}

	if err := client.Initialize(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to initialize LSP client: %w", err)
	}

	return &CallChainTracer{
		client:    client,
		rootPath:  rootPath,
		mainFuncs: make(map[string]bool),
	}, nil
}

// Close closes the tracer
func (t *CallChainTracer) Close() error {
	return t.client.Close()
}

// CallPath represents a call path from a changed symbol to a main function
type CallPath struct {
	BinaryName string
	MainURI    string
	Path       []CallNode // Changed from []string to []CallNode
}

// CallNode represents a node in the call chain
type CallNode struct {
	FunctionName string
	PackagePath  string
}

// TraceToMain traces a symbol to all main functions that call it
func (t *CallChainTracer) TraceToMain(symbol *parser.Symbol) ([]CallPath, error) {
	// Skip non-function symbols
	if symbol.Kind != parser.SymbolKindFunction {
		return nil, fmt.Errorf("symbol %s is not a function (kind: %v), skipping", symbol.Name, symbol.Kind)
	}

	// Convert file path to URI
	uri := "file://" + symbol.Position.Filename

	// Read file content
	content, err := os.ReadFile(symbol.Position.Filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Find the exact position of the function name
	// The symbol.Position might point to "func" keyword, we need to find the function name
	lines := strings.Split(string(content), "\n")
	funcLine := symbol.Position.Line - 1 // 0-based for array indexing
	funcCol := symbol.Position.Column - 1

	if funcLine >= 0 && funcLine < len(lines) {
		line := lines[funcLine]
		// Look for the function name in this line
		nameIndex := strings.Index(line, symbol.Name)
		if nameIndex >= 0 {
			// Found the function name, use its position
			funcCol = nameIndex
		}
	}

	// Open document in gopls
	if err := t.client.DidOpen(uri, "go", string(content)); err != nil {
		return nil, fmt.Errorf("failed to open document: %w", err)
	}

	// Prepare call hierarchy with the corrected position
	pos := Position{
		Line:      funcLine, // Already 0-based
		Character: funcCol,  // Already 0-based
	}

	items, err := t.client.PrepareCallHierarchy(uri, pos)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare call hierarchy: %w", err)
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("no call hierarchy items found for symbol %s at %s:%d:%d",
			symbol.Name, symbol.Position.Filename, symbol.Position.Line, funcCol+1)
	}

	// Trace incoming calls recursively
	var paths []CallPath
	visited := make(map[string]bool)
	seenBinaries := make(map[string]bool)

	for _, item := range items {
		initialNode := CallNode{
			FunctionName: item.Name,
			PackagePath:  extractPackageFromURI(item.URI),
		}
		t.traceIncomingCalls(item, []CallNode{initialNode}, visited, &paths, seenBinaries)
	}

	return paths, nil
}

// extractPackageFromURI extracts package path from file URI
func extractPackageFromURI(uri string) string {
	// file:///path/to/project/internal/bill/server/service/file.go
	// -> github.com/qbox/las/internal/bill/server/service
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	filePath := strings.TrimPrefix(uri, "file://")
	dir := filepath.Dir(filePath)

	// Find the module root and extract relative path
	// This is a simplified version - assumes standard Go project structure
	parts := strings.Split(dir, "/")
	for i, part := range parts {
		if part == "internal" || part == "cmd" || part == "pkg" || part == "api" {
			// Found a standard Go directory, construct package path
			return strings.Join(parts[i:], "/")
		}
	}
	return filepath.Base(dir)
}

// traceIncomingCalls recursively traces incoming calls
func (t *CallChainTracer) traceIncomingCalls(item CallHierarchyItem, currentPath []CallNode, visited map[string]bool, paths *[]CallPath, seenBinaries map[string]bool) {
	// Create a unique key for this item
	key := fmt.Sprintf("%s:%d:%d", item.URI, item.Range.Start.Line, item.Range.Start.Character)

	if visited[key] {
		return
	}
	visited[key] = true

	// Check if this is a main function
	if t.isMainFunction(item) {
		binaryName := t.GetBinaryName(item)

		// Deduplicate by binary name
		if seenBinaries[binaryName] {
			return
		}
		seenBinaries[binaryName] = true

		// Found a path to main
		completePath := make([]CallNode, len(currentPath))
		copy(completePath, currentPath)

		*paths = append(*paths, CallPath{
			BinaryName: binaryName,
			MainURI:    item.URI,
			Path:       completePath,
		})
		return
	}

	// Get incoming calls
	incomingCalls, err := t.client.IncomingCalls(item)
	if err != nil {
		fmt.Printf("Warning: failed to get incoming calls for %s: %v\n", item.Name, err)
		return
	}

	if len(incomingCalls) == 0 {
		// Dead end - no callers found
		return
	}

	// Recursively trace each caller
	for _, call := range incomingCalls {
		callerNode := CallNode{
			FunctionName: call.From.Name,
			PackagePath:  extractPackageFromURI(call.From.URI),
		}

		// Check for cross-service calls
		// If the caller is in a different service (cmd/xxx or internal/xxx),
		// and the current function is in a specific service package,
		// skip this path as it's likely a false positive from interface calls
		if t.isCrossServiceCall(callerNode.PackagePath, currentPath) {
			continue
		}

		newPath := append([]CallNode{callerNode}, currentPath...)
		t.traceIncomingCalls(call.From, newPath, visited, paths, seenBinaries)
	}
}

// isCrossServiceCall checks if a call crosses service boundaries
func (t *CallChainTracer) isCrossServiceCall(callerPkg string, currentPath []CallNode) bool {
	if len(currentPath) == 0 {
		return false
	}

	// Extract service name from caller package
	// e.g., "cmd/rfsworker" -> "rfsworker"
	// e.g., "internal/bill/server" -> "bill"
	callerService := extractServiceName(callerPkg)

	// Check if any node in the current path belongs to a different service
	for _, node := range currentPath {
		nodeService := extractServiceName(node.PackagePath)

		// If both are in specific services and they're different, it's a cross-service call
		if callerService != "" && nodeService != "" && callerService != nodeService {
			// Exception: allow calls through common packages like pkg/grace
			if !isCommonPackage(node.PackagePath) {
				return true
			}
		}
	}

	return false
}

// extractServiceName extracts the service name from a package path
func extractServiceName(pkgPath string) string {
	// cmd/servicename -> servicename
	if strings.HasPrefix(pkgPath, "cmd/") {
		parts := strings.Split(pkgPath, "/")
		if len(parts) >= 2 {
			return parts[1]
		}
	}

	// internal/servicename/... -> servicename
	if strings.HasPrefix(pkgPath, "internal/") {
		parts := strings.Split(pkgPath, "/")
		if len(parts) >= 2 {
			return parts[1]
		}
	}

	return ""
}

// isCommonPackage checks if a package is a common/shared package
func isCommonPackage(pkgPath string) bool {
	commonPrefixes := []string{
		"pkg/",
		"api/",
		"common/",
		"shared/",
		"lib/",
	}

	for _, prefix := range commonPrefixes {
		if strings.HasPrefix(pkgPath, prefix) {
			return true
		}
	}

	return false
}

// isMainFunction checks if an item is a main function
func (t *CallChainTracer) isMainFunction(item CallHierarchyItem) bool {
	// Check if function name is "main"
	if item.Name != "main" {
		return false
	}

	// Check if it's in a main package
	// Extract package name from URI
	uri := item.URI
	if !strings.HasPrefix(uri, "file://") {
		return false
	}

	filePath := strings.TrimPrefix(uri, "file://")
	dir := filepath.Dir(filePath)

	// Check if directory contains "cmd/" or is named "main"
	return strings.Contains(dir, "/cmd/") || filepath.Base(dir) == "main"
}

// GetBinaryName extracts the binary name from a main function's URI
func (t *CallChainTracer) GetBinaryName(item CallHierarchyItem) string {
	uri := item.URI
	if !strings.HasPrefix(uri, "file://") {
		return "unknown"
	}

	filePath := strings.TrimPrefix(uri, "file://")
	dir := filepath.Dir(filePath)

	// Extract binary name from path like /path/to/cmd/servicename/main.go
	parts := strings.Split(dir, "/cmd/")
	if len(parts) == 2 {
		return filepath.Base(parts[1])
	}

	return filepath.Base(dir)
}
