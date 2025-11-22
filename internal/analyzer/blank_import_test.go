package analyzer

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

func TestTraceBlankImportToMain(t *testing.T) {
	ctx := context.Background()

	// 使用 testdata 中的测试项目
	testProject := filepath.Join("..", "..", "testdata", "init-test")

	// 创建 LSP tracer
	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	tests := []struct {
		name         string
		importPath   string
		expectedBins []string // 期望受影响的 binary 名称
		description  string
	}{
		{
			name:       "blank import db package",
			importPath: "example.com/init-test/internal/db",
			expectedBins: []string{
				"server",     // server 使用空导入引入 db
				"api-server", // api-server 直接导入 db
				"worker",     // worker 也导入 db
			},
			description: "blank import of db should affect all services that import it",
		},
		{
			name:       "blank import cache package",
			importPath: "example.com/init-test/internal/cache",
			expectedBins: []string{
				"server",
				"api-server",
			},
			description: "blank import of cache affects server and api-server",
		},
		{
			name:       "blank import logger package",
			importPath: "example.com/init-test/internal/logger",
			expectedBins: []string{
				"api-server",
			},
			description: "blank import of logger affects only api-server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建空导入 symbol
			symbol := &parser.Symbol{
				Name: "_",
				Kind: parser.SymbolKindImport,
				Position: token.Position{
					Filename: "dummy.go",
					Line:     1,
					Column:   1,
				},
				Extra: parser.ImportExtra{
					Alias: "_",
					Path:  tt.importPath,
				},
			}

			// 追踪到 main
			paths, err := tracer.TraceToMain(symbol)
			if err != nil {
				t.Fatalf("Failed to trace blank import: %v", err)
			}

			// 提取找到的 binary 名称
			foundBins := make(map[string]bool)
			for _, path := range paths {
				foundBins[path.BinaryName] = true
			}

			// 验证期望的 binary 都被找到
			for _, expectedBin := range tt.expectedBins {
				if !foundBins[expectedBin] {
					t.Errorf("%s: expected to find binary '%s', but it was not found", tt.description, expectedBin)
				}
			}

			// 日志输出
			t.Logf("%s: found %d affected services", tt.description, len(paths))
			for _, path := range paths {
				t.Logf("  - %s", path.BinaryName)
			}
		})
	}
}

func TestBlankImportVsNormalImport(t *testing.T) {
	ctx := context.Background()

	testProject := filepath.Join("..", "..", "testdata", "init-test")

	tracer, err := lsp.NewDirectCallTracer(ctx, testProject)
	if err != nil {
		t.Fatalf("Failed to create tracer: %v", err)
	}
	defer tracer.Close()

	// 测试空导入（应该成功）
	blankImportSymbol := &parser.Symbol{
		Name: "_",
		Kind: parser.SymbolKindImport,
		Position: token.Position{
			Filename: "dummy.go",
			Line:     1,
			Column:   1,
		},
		Extra: parser.ImportExtra{
			Alias: "_",
			Path:  "example.com/init-test/internal/db",
		},
	}

	paths, err := tracer.TraceToMain(blankImportSymbol)
	if err != nil {
		t.Errorf("Blank import should be supported, but got error: %v", err)
	}
	if len(paths) == 0 {
		t.Error("Blank import should find affected services")
	}

	// 测试普通导入（应该失败）
	normalImportSymbol := &parser.Symbol{
		Name: "cache",
		Kind: parser.SymbolKindImport,
		Position: token.Position{
			Filename: "dummy.go",
			Line:     1,
			Column:   1,
		},
		Extra: parser.ImportExtra{
			Alias: "cache",
			Path:  "example.com/init-test/internal/cache",
		},
	}

	_, err = tracer.TraceToMain(normalImportSymbol)
	if err == nil {
		t.Error("Normal import should not be supported, but no error was returned")
	}
}

func TestIsSupportedSymbolKindImport(t *testing.T) {
	tests := []struct {
		kind     parser.SymbolKind
		expected bool
	}{
		{parser.SymbolKindFunction, true},
		{parser.SymbolKindConstant, true},
		{parser.SymbolKindVariable, true},
		{parser.SymbolKindInit, true},
		{parser.SymbolKindImport, true}, // Now supported
		{parser.SymbolKindStruct, false},
		{parser.SymbolKindInterface, false},
		{parser.SymbolKindType, false},
	}

	for _, tt := range tests {
		result := isSupportedSymbolKind(tt.kind)
		if result != tt.expected {
			t.Errorf("isSupportedSymbolKind(%v) = %v, want %v",
				tt.kind, result, tt.expected)
		}
	}
}
