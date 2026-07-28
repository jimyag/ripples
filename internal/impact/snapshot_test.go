package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sync"
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

func TestASTHashIsDeterministicWhenCalledConcurrently(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "example.go", `package example

type Config struct {
	First, Second int
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	field := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List[0]
	want, err := astHash(field, fset)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 100
	results := make(chan string, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			got, hashErr := astHash(field, fset)
			if hashErr != nil {
				results <- hashErr.Error()
				return
			}
			results <- got
		}()
	}
	group.Wait()
	close(results)
	for got := range results {
		if got != want {
			t.Fatalf("concurrent astHash() = %q, want %q", got, want)
		}
	}
	if got := parserObjectCount(file); got == 0 {
		t.Fatal("concurrent astHash() mutated parser object links")
	}
}

func TestParseAnalysisFileSkipsParserObjectsAndKeepsComments(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parseAnalysisFile(fset, "example.go", []byte(`package example

// Value documents the declaration.
var Value = 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parserObjectCount(file); got != 0 {
		t.Fatalf("parseAnalysisFile() parser objects = %d, want 0", got)
	}
	if len(file.Comments) != 1 {
		t.Fatalf("parseAnalysisFile() comments = %d, want 1", len(file.Comments))
	}
}

func TestTrimUnusedTypeInfoKeepsRequiredMaps(t *testing.T) {
	info := &types.Info{
		Types:        make(map[ast.Expr]types.TypeAndValue),
		Instances:    make(map[*ast.Ident]types.Instance),
		Defs:         make(map[*ast.Ident]types.Object),
		Uses:         make(map[*ast.Ident]types.Object),
		Implicits:    make(map[ast.Node]types.Object),
		Selections:   make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:       make(map[ast.Node]*types.Scope),
		InitOrder:    []*types.Initializer{},
		FileVersions: make(map[*ast.File]string),
	}

	trimUnusedTypeInfo(info)

	if info.Types == nil || info.Defs == nil || info.Uses == nil ||
		info.Implicits == nil || info.Selections == nil {
		t.Fatal("trimUnusedTypeInfo() removed required type information")
	}
	if info.Instances != nil || info.Scopes != nil ||
		info.InitOrder != nil || info.FileVersions != nil {
		t.Fatal("trimUnusedTypeInfo() retained unused type information")
	}
}
