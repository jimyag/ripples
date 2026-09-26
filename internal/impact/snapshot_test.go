package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
)

func TestASTHashMatchesReparsedFile(t *testing.T) {
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
	loadedScope := loaded.Scope
	loadedObjects := parserObjectCount(loaded)

	reparsed, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	got, err := astHash(loaded)
	if err != nil {
		t.Fatal(err)
	}
	want, err := astHash(reparsed)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("astHash(loaded) = %q, want the hash of the reparsed file %q", got, want)
	}
	if loaded.Scope != loadedScope {
		t.Fatal("astHash() changed the loaded AST scope")
	}
	if got := parserObjectCount(loaded); got != loadedObjects {
		t.Fatalf("astHash() left %d parser objects, want %d", got, loadedObjects)
	}
}

func TestASTHashIgnoresLayoutButNotSemantics(t *testing.T) {
	hash := func(source string) string {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), "example.go", "package example\n\n"+source, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		got, err := astHash(file)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if hash("func F(a int) int { return a + 1 }") !=
		hash("// F documents itself.\nfunc F(a int) int {\n\t// One more.\n\treturn a +\n\t\t1\n}\n") {
		t.Error("comments and layout changed the hash")
	}
	for _, pair := range [][2]string{
		{"func F(xs []any) { G(xs...) }", "func F(xs []any) { G(xs) }"},
		{"func F() { type T = int }", "func F() { type T int }"},
		{"func F(a []int, x int) []int { return a[:x] }", "func F(a []int, x int) []int { return a[x:] }"},
		{"func F(p *int) any { return *p }", "func F(p *int) any { return (p) }"},
		{"func F(a, b int) int { return a + b }", "func F(a, b int) int { return a - b }"},
	} {
		if hash(pair[0]) == hash(pair[1]) {
			t.Errorf("%q and %q have the same hash", pair[0], pair[1])
		}
	}
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
	want, err := astHash(field)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 100
	results := make(chan string, workers)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			got, hashErr := astHash(field)
			if hashErr != nil {
				results <- hashErr.Error()
				return
			}
			results <- got
		})
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
