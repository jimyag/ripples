package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
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
	if err := os.WriteFile(filepath.Join(filepath.Dir(filename), "go.mod"), []byte("module example.com/hash\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loadedPackages, err := gopackages.Load(&gopackages.Config{
		Dir:  filepath.Dir(filename),
		Mode: gopackages.NeedName | gopackages.NeedCompiledGoFiles | gopackages.NeedSyntax,
	}, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedPackages) != 1 || len(loadedPackages[0].Syntax) != 1 {
		t.Fatalf("loaded packages = %#v", loadedPackages)
	}
	loaded := loadedPackages[0].Syntax[0]
	loadedFset := loadedPackages[0].Fset
	loadedScope := loaded.Scope
	loadedObjects := parserObjectCount(loaded)

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
	if loaded.Scope != loadedScope {
		t.Fatal("astFileHash() did not restore the loaded AST scope")
	}
	if got := parserObjectCount(loaded); got != loadedObjects {
		t.Fatalf("astFileHash() restored %d parser objects, want %d", got, loadedObjects)
	}
}

func legacyASTFileHash(file *ast.File, fset *token.FileSet) (string, error) {
	return astFileHash(file, fset)
}

func parserObjectCount(file *ast.File) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Obj != nil {
			count++
		}
		return true
	})
	return count
}
