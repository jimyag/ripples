package analyzer

import (
	"context"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/lsp"
	"github.com/jimyag/ripples/internal/parser"
)

func TestTraceInitToMain(t *testing.T) {
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
		pkgPath      string
		expectedBins []string // 期望受影响的 binary 名称
		description  string
	}{
		{
			name:    "config.init - affects all services",
			pkgPath: "example.com/init-test/pkg/config",
			expectedBins: []string{
				"server",
				"api-server",
				"worker",
			},
			description: "config.init should affect all three services",
		},
		{
			name:    "db.init - affects most services",
			pkgPath: "example.com/init-test/internal/db",
			expectedBins: []string{
				"server",
				"api-server",
				"worker",
			},
			description: "db.init should affect server, api-server, and worker",
		},
		{
			name:    "cache.init - affects some services",
			pkgPath: "example.com/init-test/internal/cache",
			expectedBins: []string{
				"server",
				"api-server",
			},
			description: "cache.init should affect server and api-server (not worker)",
		},
		{
			name:    "logger.init - affects only api-server",
			pkgPath: "example.com/init-test/internal/logger",
			expectedBins: []string{
				"api-server",
			},
			description: "logger.init should only affect api-server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 init 函数 symbol
			symbol := &parser.Symbol{
				Name:        "init",
				Kind:        parser.SymbolKindInit,
				Position:    token.Position{Filename: "dummy", Line: 1, Column: 1},
				PackagePath: tt.pkgPath,
			}

			// 追踪到 main
			paths, err := tracer.TraceToMain(symbol)
			if err != nil {
				t.Fatalf("Failed to trace init function: %v", err)
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

			// 验证没有多余的 binary
			if len(foundBins) > len(tt.expectedBins) {
				t.Logf("%s: found %d binaries, expected %d", tt.description, len(foundBins), len(tt.expectedBins))
				for bin := range foundBins {
					found := false
					for _, expected := range tt.expectedBins {
						if bin == expected {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("%s: unexpected binary found: %s", tt.description, bin)
					}
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

func TestIsSupportedSymbolKindInit(t *testing.T) {
	tests := []struct {
		kind     parser.SymbolKind
		expected bool
	}{
		{parser.SymbolKindFunction, true},
		{parser.SymbolKindConstant, true},
		{parser.SymbolKindVariable, true},
		{parser.SymbolKindInit, true},
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
