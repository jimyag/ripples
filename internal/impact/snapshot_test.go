package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestASTFileHashMatchesReparsedFile(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "example.go")
	source := []byte(`package example

// Value documents the declaration.
func Value(input int) int {
	return input + 1
}
`)
	if err := os.WriteFile(filename, source, 0o600); err != nil {
		t.Fatal(err)
	}

	loadedFset := token.NewFileSet()
	loaded, err := parser.ParseFile(loadedFset, filename, source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	reparsedFset := token.NewFileSet()
	reparsed, err := parser.ParseFile(reparsedFset, filename, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	got, err := astFileHash(loaded, loadedFset)
	if err != nil {
		t.Fatal(err)
	}
	want, err := legacyASTFileHash(reparsed, reparsedFset)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("astFileHash() = %q, want legacy hash %q", got, want)
	}
}

func legacyASTFileHash(file *ast.File, fset *token.FileSet) (string, error) {
	return astFileHash(file, fset)
}
