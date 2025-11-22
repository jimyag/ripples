package analyzer

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

func TestTraceConstantToMain(t *testing.T) {
	ctx := context.Background()

	// 使用 testdata 中的测试项目
	testProject := filepath.Join("..", "..", "testdata", "constant-test")

	// 创建 LSP tracer
	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// 测试追踪常量 MaxRetries
	symbol := &parser.Symbol{
		Name: "MaxRetries",
		Kind: parser.SymbolKindConstant,
		Position: token.Position{
			Filename: filepath.Join(testProject, "internal/config/config.go"),
			Line:     4,
			Column:   7,
		},
		PackagePath: "example.com/constant-test/internal/config",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace constant: %v", err)
	}

	// 验证结果
	if len(paths) == 0 {
		t.Error("Expected at least one path to main, got none")
	}

	// 验证找到了 server binary
	foundServer := false
	for _, path := range paths {
		if path.BinaryName == "server" {
			foundServer = true

			// 验证调用链包含 main 和 DoWithRetry
			hasMain := false
			hasDoWithRetry := false

			for _, node := range path.Path {
				if node.FunctionName == "main" {
					hasMain = true
				}
				if node.FunctionName == "DoWithRetry" {
					hasDoWithRetry = true
				}
			}

			if !hasMain {
				t.Error("Call path should include main function")
			}
			if !hasDoWithRetry {
				t.Error("Call path should include DoWithRetry function")
			}

			// 验证路径顺序：main -> DoWithRetry
			if len(path.Path) >= 2 {
				if path.Path[0].FunctionName != "main" {
					t.Errorf("First function in path should be main, got %s", path.Path[0].FunctionName)
				}
				if path.Path[len(path.Path)-1].FunctionName != "DoWithRetry" {
					t.Errorf("Last function should be DoWithRetry, got %s", path.Path[len(path.Path)-1].FunctionName)
				}
			}
		}
	}

	if !foundServer {
		t.Error("Expected to find 'server' binary in results")
	}
}

func TestTraceVariableToMain(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "constant-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// 测试追踪变量 DefaultTimeout
	symbol := &parser.Symbol{
		Name: "DefaultTimeout",
		Kind: parser.SymbolKindVariable,
		Position: token.Position{
			Filename: filepath.Join(testProject, "internal/config/config.go"),
			Line:     7,
			Column:   5,
		},
		PackagePath: "example.com/constant-test/internal/config",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace variable: %v", err)
	}

	// 变量目前没有被使用，所以应该返回空路径
	// 这是预期行为
	if len(paths) > 0 {
		t.Logf("Found %d paths for unused variable (this is ok if the variable is used)", len(paths))
	}
}

func TestTraceFunctionToMain(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "constant-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// 测试追踪函数 DoWithRetry
	symbol := &parser.Symbol{
		Name: "DoWithRetry",
		Kind: parser.SymbolKindFunction,
		Position: token.Position{
			Filename: filepath.Join(testProject, "internal/service/retry.go"),
			Line:     6,
			Column:   6,
		},
		PackagePath: "example.com/constant-test/internal/service",
	}

	paths, err := tracer.TraceToMain(symbol)
	if err != nil {
		t.Fatalf("Failed to trace function: %v", err)
	}

	if len(paths) == 0 {
		t.Error("Expected at least one path to main for DoWithRetry")
	}

	// 验证找到了 server binary
	foundServer := false
	for _, path := range paths {
		if path.BinaryName == "server" {
			foundServer = true

			// DoWithRetry 应该在路径的末尾（作为被调用的函数）
			if len(path.Path) > 0 {
				lastFunc := path.Path[len(path.Path)-1].FunctionName
				if lastFunc != "DoWithRetry" {
					t.Errorf("Expected last function to be DoWithRetry, got %s", lastFunc)
				}
			}
		}
	}

	if !foundServer {
		t.Error("Expected to find 'server' binary")
	}
}

func TestIsSupportedSymbolKind(t *testing.T) {
	tests := []struct {
		kind     parser.SymbolKind
		expected bool
	}{
		{parser.SymbolKindFunction, true},
		{parser.SymbolKindConstant, true},
		{parser.SymbolKindVariable, true},
		{parser.SymbolKindStruct, false},
		{parser.SymbolKindInterface, false},
		{parser.SymbolKindType, false},
		{parser.SymbolKindImport, true}, // Now supported (blank imports)
	}

	for _, tt := range tests {
		result := isSupportedSymbolKind(tt.kind)
		if result != tt.expected {
			t.Errorf("isSupportedSymbolKind(%v) = %v, want %v",
				tt.kind, result, tt.expected)
		}
	}
}
