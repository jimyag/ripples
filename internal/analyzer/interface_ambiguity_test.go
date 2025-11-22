package analyzer

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

// TestSharedInterfaceFunctionNotCrossingService tests the critical scenario:
// When modifying a shared package function that accepts an interface,
// it should affect ALL services that call it, but the interface ambiguity
// filter should prevent false positives when tracing further up the call chain.
//
// Real-world scenario:
//   - pkg/common.RunServer(r Runner) is a shared function
//   - Both service-a and service-b call it with their own Server implementations
//   - When we modify RunServer, BOTH services should be affected (correct)
//   - The path-based filtering prevents false cross-service contamination
func TestSharedInterfaceFunctionNotCrossingService(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Scenario: Modify pkg/common.RunServer
	// This function is called by both service-a and service-b
	symbol := &parser.Symbol{
		Name: "RunServer",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "pkg/common/logger.go"),
			Line:     45, // func RunServer(r Runner) error
			Column:   6,
		},
		PackagePath: "example.com/shared-package-test/pkg/common",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace shared interface function: %v", err)
	}

	// Extract affected services
	affectedServices := make(map[string]bool)
	for _, path := range paths {
		affectedServices[path.BinaryName] = true
		t.Logf("Found path to: %s", path.BinaryName)
		for i, node := range path.Path {
			t.Logf("  [%d] %s.%s", i, node.PackagePath, node.FunctionName)
		}
	}

	// Both services should be affected since both call RunServer
	if !affectedServices["service-a"] {
		t.Error("Expected service-a to be affected by RunServer change")
	}

	if !affectedServices["service-b"] {
		t.Error("Expected service-b to be affected by RunServer change")
	}

	// Should affect exactly 2 services
	if len(affectedServices) != 2 {
		t.Errorf("Expected exactly 2 affected services, but found %d", len(affectedServices))
	}

	t.Logf("Shared interface function change affected %d service(s)", len(affectedServices))
	for svc := range affectedServices {
		t.Logf("  - %s", svc)
	}
}

// TestPathBasedFilteringScores tests that the path-based filtering
// correctly identifies callers based on their relationship to the current path.
//
// This test verifies that when tracing from an internal function,
// the path-based scoring prevents false positives from other services.
func TestPathBasedFilteringScores(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Scenario: Modify ProcessRequest in service-a
	// This should ONLY affect service-a, not service-b
	symbol := &parser.Symbol{
		Name: "ProcessRequest",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "internal/service-a/handler.go"),
			Line:     18,
			Column:   21,
		},
		PackagePath: "example.com/shared-package-test/internal/service-a",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace: %v", err)
	}

	affectedServices := make(map[string]bool)
	for _, path := range paths {
		affectedServices[path.BinaryName] = true

		// Verify the path contains service-a packages
		hasServiceA := false
		for _, node := range path.Path {
			if node.PackagePath == "example.com/shared-package-test/internal/service-a" {
				hasServiceA = true
			}
			// Should NOT contain service-b packages
			if node.PackagePath == "example.com/shared-package-test/internal/service-b" {
				t.Errorf("Path incorrectly contains service-b package: %v", path.Path)
			}
		}

		if !hasServiceA && path.BinaryName == "service-a" {
			t.Error("Path to service-a should contain service-a packages")
		}
	}

	if !affectedServices["service-a"] {
		t.Error("service-a should be affected")
	}

	if affectedServices["service-b"] {
		t.Error("service-b should NOT be affected (path-based filtering failed)")
	}

	if len(affectedServices) != 1 {
		t.Errorf("Expected 1 affected service, got %d", len(affectedServices))
	}
}
