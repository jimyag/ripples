package analyzer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jimyag/ripples/internal/parser"
)

func TestAffectedBinaryJSONShape(t *testing.T) {
	data, err := json.Marshal(AffectedBinary{
		Name:      "server",
		PkgPath:   "example.com/app/cmd/server",
		TracePath: []string{"main"},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	want := `{"name":"server","package":"example.com/app/cmd/server","trace_path":["main"]}`
	if string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}

func TestExtractPkgPathFromFileURI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n"), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}

	mainFile := filepath.Join(dir, "cmd", "server", "main.go")
	analyzer := &LSPImpactAnalyzer{rootPath: dir}

	got := analyzer.extractPkgPath("file://" + mainFile)
	if got != "example.com/app/cmd/server" {
		t.Fatalf("extractPkgPath() = %q, want %q", got, "example.com/app/cmd/server")
	}
}

func TestHasSupportedChanges(t *testing.T) {
	changes := []ChangedSymbol{
		{Symbol: &parser.Symbol{Kind: parser.SymbolKindStruct}},
		{Symbol: &parser.Symbol{Kind: parser.SymbolKindFunction}},
	}

	if !HasSupportedChanges(changes) {
		t.Fatal("HasSupportedChanges() = false, want true")
	}
}

func TestIsSupportedChangeSkipsNonBlankImport(t *testing.T) {
	change := ChangedSymbol{
		Symbol: &parser.Symbol{
			Kind: parser.SymbolKindImport,
			Extra: parser.ImportExtra{
				Path: "fmt",
			},
		},
	}

	if IsSupportedChange(change) {
		t.Fatal("IsSupportedChange(non-blank import) = true, want false")
	}
}

func TestIsSupportedChangeAllowsBlankImport(t *testing.T) {
	change := ChangedSymbol{
		Symbol: &parser.Symbol{
			Kind: parser.SymbolKindImport,
			Extra: parser.ImportExtra{
				Alias: "_",
				Path:  "github.com/lib/pq",
			},
		},
	}

	if !IsSupportedChange(change) {
		t.Fatal("IsSupportedChange(blank import) = false, want true")
	}
}
