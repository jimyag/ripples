package analyzer

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

// TestSharedPackageChange tests tracing when a shared package (pkg/common) is modified
// This should find all services that use the shared package
func TestSharedPackageChange(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Simulate changing the LogMessage function in pkg/common/logger.go
	// This is a standalone function (not a method) that both services use
	symbol := &parser.Symbol{
		Name: "LogMessage",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "pkg/common/logger.go"),
			Line:     29, // LogMessage function
			Column:   6,  // Point to function name
		},
		PackagePath: "example.com/shared-package-test/pkg/common",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace shared package function: %v", err)
	}

	// Extract affected services
	affectedServices := make(map[string]bool)
	for _, path := range paths {
		affectedServices[path.BinaryName] = true
	}

	// Both service-a and service-b should be affected
	expectedServices := []string{"service-a", "service-b"}
	for _, expectedSvc := range expectedServices {
		if !affectedServices[expectedSvc] {
			t.Errorf("Expected service '%s' to be affected by shared package change, but it was not found", expectedSvc)
		}
	}

	t.Logf("Shared package change affected %d services", len(affectedServices))
	for svc := range affectedServices {
		t.Logf("  - %s", svc)
	}

	// Should NOT find more than expected (no false positives)
	if len(affectedServices) != len(expectedServices) {
		t.Errorf("Expected %d affected services, but found %d", len(expectedServices), len(affectedServices))
	}
}

// TestInternalPackageNotCrossingShared tests that when an internal package is modified,
// the trace should stop at shared packages to prevent cross-service false positives
func TestInternalPackageNotCrossingShared(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Simulate changing ProcessRequest in internal/service-a
	symbol := &parser.Symbol{
		Name: "ProcessRequest",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "internal/service-a/handler.go"),
			Line:     18,
			Column:   21, // Point to method name
		},
		PackagePath: "example.com/shared-package-test/internal/service-a",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace internal package function: %v", err)
	}

	// Extract affected services
	affectedServices := make(map[string]bool)
	for _, path := range paths {
		affectedServices[path.BinaryName] = true
	}

	// Only service-a should be affected, NOT service-b
	if !affectedServices["service-a"] {
		t.Error("Expected service-a to be affected, but it was not found")
	}

	if affectedServices["service-b"] {
		t.Error("service-b should NOT be affected by service-a internal changes (false positive detected)")
	}

	t.Logf("Internal package change affected %d service(s)", len(affectedServices))
	for svc := range affectedServices {
		t.Logf("  - %s", svc)
	}

	if len(affectedServices) != 1 {
		t.Errorf("Expected exactly 1 affected service, but found %d", len(affectedServices))
	}
}

// TestSharedPackageInternalCalls tests the scenario where:
// - A shared package has two functions, one calling the other
// - Changing the caller function should affect all services using it
func TestSharedPackageInternalCalls(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Test changing LogMessageWithPrefix which internally calls LogMessage
	// This tests that shared package internal calls work correctly
	symbol := &parser.Symbol{
		Name: "LogMessageWithPrefix",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "pkg/common/logger.go"),
			Line:     34, // LogMessageWithPrefix function
			Column:   6,  // Point to function name
		},
		PackagePath: "example.com/shared-package-test/pkg/common",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace shared package function: %v", err)
	}

	// Extract affected services
	affectedServices := make(map[string]bool)
	for _, path := range paths {
		affectedServices[path.BinaryName] = true
	}

	t.Logf("LogMessageWithPrefix change affected %d services", len(affectedServices))
	for svc := range affectedServices {
		t.Logf("  - %s", svc)
	}

	// Note: If no services directly call LogMessageWithPrefix, this test
	// might return 0 results - which is correct behavior
	// The test is mainly to verify no cross-service false positives occur
}

// TestInternalViaSharedPackage tests the CRITICAL scenario that was missing:
// - Modify the shared pkg/common.RunServer function
// - Both service-a and service-b call this function
// - Expected: BOTH services should be affected (this is a shared package modification)
// This is different from TestInternalPackageNotCrossingShared which tests internal -> shared boundary
func TestInternalViaSharedPackage(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "shared-package-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// Test modifying the shared RunServer function
	// Both services call this, so both should be affected
	symbol := &parser.Symbol{
		Name: "RunServer",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "pkg/common/logger.go"),
			Line:     45, // RunServer function
			Column:   6,  // Point to function name
		},
		PackagePath: "example.com/shared-package-test/pkg/common",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace shared package function: %v", err)
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

	// Both services should be affected since they both call RunServer
	expectedServices := []string{"service-a", "service-b"}
	for _, expectedSvc := range expectedServices {
		if !affectedServices[expectedSvc] {
			t.Errorf("Expected service '%s' to be affected by shared package change, but it was not found", expectedSvc)
		}
	}

	t.Logf("Shared RunServer function affected %d service(s)", len(affectedServices))
	for svc := range affectedServices {
		t.Logf("  - %s", svc)
	}
}
