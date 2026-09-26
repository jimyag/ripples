package impact

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	gopackages "golang.org/x/tools/go/packages"
)

// loadPackages returns the packages matching ./... in dir, with syntax and
// type information, and the metadata of every package they depend on.
//
// go/packages can type-check them too, but it then runs go list -export,
// which compiles every listed package, including the module's own packages
// that are type-checked from source anyway. Compiling the module dominated
// the analysis time and filled the Go build cache, so go/packages only
// reads metadata here, dependencies come from export data, and the module's
// packages and their test variants are type-checked from source.
func loadPackages(ctx context.Context, dir string, tests bool) ([]*gopackages.Package, error) {
	// -trimpath keeps cgo output and the compiled export data of
	// dependencies independent of the export directory, so the build
	// cache reuses them across runs and trees.
	buildFlags := []string{"-trimpath"}
	metadata, err := gopackages.Load(&gopackages.Config{
		Context: ctx,
		Dir:     dir,
		Mode: gopackages.NeedName |
			gopackages.NeedFiles |
			gopackages.NeedCompiledGoFiles |
			gopackages.NeedImports |
			gopackages.NeedDeps |
			gopackages.NeedModule |
			gopackages.NeedEmbedFiles |
			gopackages.NeedForTest |
			gopackages.NeedTypesSizes,
		// With tests, test variants and external test packages are analyzed so
		// test-only changes and declarations used by tests reach their packages.
		Tests:      tests,
		BuildFlags: buildFlags,
	}, "./...")
	if err != nil {
		return nil, fmt.Errorf("load package graph: %w", err)
	}
	roots := slices.DeleteFunc(metadata, isTestMain)
	if len(roots) == 0 {
		return nil, fmt.Errorf("no Go packages found")
	}

	// Roots and the test variants they reach are type-checked from source;
	// everything else is a dependency read from export data.
	isRoot := make(map[string]bool, len(roots))
	for _, pkg := range roots {
		isRoot[pkg.ID] = true
	}
	var source []*gopackages.Package
	sourceIDs := make(map[string]bool)
	dependencies := make(map[string]*gopackages.Package)
	var visit func(*gopackages.Package)
	visit = func(pkg *gopackages.Package) {
		if sourceIDs[pkg.ID] || dependencies[pkg.ID] != nil || pkg.ID == "unsafe" {
			return
		}
		if !isRoot[pkg.ID] && pkg.ForTest == "" {
			dependencies[pkg.ID] = pkg
			return
		}
		sourceIDs[pkg.ID] = true
		source = append(source, pkg)
		for _, imported := range pkg.Imports {
			visit(imported)
		}
	}
	for _, pkg := range roots {
		visit(pkg)
	}
	if err := packageErrors(source); err != nil {
		return nil, err
	}
	if err := loadDependencyTypes(ctx, dir, buildFlags, dependencies); err != nil {
		return nil, err
	}

	checker := &sourceChecker{
		fset:   token.NewFileSet(),
		sizes:  roots[0].TypesSizes,
		isRoot: isRoot,
		states: make(map[string]*checkState, len(source)),
		files:  make(map[string]*parsedFile),
	}
	if checker.sizes == nil {
		checker.sizes = types.SizesFor("gc", runtime.GOARCH)
	}
	for _, pkg := range source {
		checker.states[pkg.ID] = &checkState{}
	}
	if err := parallelFor(len(source), func(index int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return checker.check(source[index])
	}); err != nil {
		return nil, err
	}
	return roots, nil
}

// loadDependencyTypes fills the Types of the dependency packages from export
// data. One load shares type objects across all of them.
func loadDependencyTypes(ctx context.Context, dir string, buildFlags []string, dependencies map[string]*gopackages.Package) error {
	if len(dependencies) == 0 {
		return nil
	}
	loaded, err := gopackages.Load(&gopackages.Config{
		Context:    ctx,
		Dir:        dir,
		Mode:       gopackages.NeedName | gopackages.NeedTypes,
		BuildFlags: buildFlags,
	}, slices.Sorted(func(yield func(string) bool) {
		for id := range dependencies {
			if !yield(id) {
				return
			}
		}
	})...)
	if err != nil {
		return fmt.Errorf("load dependency types: %w", err)
	}
	if err := packageErrors(loaded); err != nil {
		return err
	}
	for _, pkg := range loaded {
		if dependency, ok := dependencies[pkg.ID]; ok {
			dependency.Types = pkg.Types
		}
	}
	for id, dependency := range dependencies {
		if dependency.Types == nil {
			return fmt.Errorf("load dependency types: no export data for %s", id)
		}
	}
	return nil
}

func packageErrors(packages []*gopackages.Package) error {
	var messages []string
	for _, pkg := range packages {
		for _, packageErr := range pkg.Errors {
			messages = append(messages, packageErr.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	sort.Strings(messages)
	return fmt.Errorf("load package graph: %s", strings.Join(messages, "; "))
}

// sourceChecker type-checks packages after their imports, as go/packages
// does. Files shared by a package and its test variants are parsed once.
type sourceChecker struct {
	fset   *token.FileSet
	sizes  types.Sizes
	isRoot map[string]bool
	states map[string]*checkState

	filesMu sync.Mutex
	files   map[string]*parsedFile
}

type checkState struct {
	once sync.Once
	err  error
}

type parsedFile struct {
	once sync.Once
	file *ast.File
	err  error
}

func (c *sourceChecker) check(pkg *gopackages.Package) error {
	state := c.states[pkg.ID]
	state.once.Do(func() {
		for _, imported := range pkg.Imports {
			if c.states[imported.ID] == nil {
				continue
			}
			if err := c.check(imported); err != nil {
				state.err = err
				return
			}
		}
		state.err = c.typeCheck(pkg)
	})
	return state.err
}

func (c *sourceChecker) typeCheck(pkg *gopackages.Package) error {
	files := make([]*ast.File, len(pkg.CompiledGoFiles))
	for index, filename := range pkg.CompiledGoFiles {
		file, err := c.parse(filename)
		if err != nil {
			return fmt.Errorf("load package graph: %w", err)
		}
		files[index] = file
	}

	info := &types.Info{
		Types:        make(map[ast.Expr]types.TypeAndValue),
		Defs:         make(map[*ast.Ident]types.Object),
		Uses:         make(map[*ast.Ident]types.Object),
		Implicits:    make(map[ast.Node]types.Object),
		Instances:    make(map[*ast.Ident]types.Instance),
		Scopes:       make(map[ast.Node]*types.Scope),
		Selections:   make(map[*ast.SelectorExpr]*types.Selection),
		FileVersions: make(map[*ast.File]string),
	}
	var typeErrors []string
	config := &types.Config{
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if path == "unsafe" {
				return types.Unsafe, nil
			}
			imported := pkg.Imports[path]
			if imported == nil || imported.Types == nil {
				return nil, fmt.Errorf("no type information for %s", path)
			}
			return imported.Types, nil
		}),
		// Only the packages that are analyzed need their function bodies.
		IgnoreFuncBodies: !c.isRoot[pkg.ID],
		Error: func(err error) {
			typeErrors = append(typeErrors, err.Error())
		},
		Sizes: c.sizes,
	}
	if pkg.Module != nil && pkg.Module.GoVersion != "" {
		config.GoVersion = "go" + pkg.Module.GoVersion
	}
	checked := types.NewPackage(pkg.PkgPath, pkg.Name)
	_ = types.NewChecker(config, c.fset, checked, info).Files(files)
	if len(typeErrors) > 0 {
		sort.Strings(typeErrors)
		return fmt.Errorf("load package graph: %s", strings.Join(typeErrors, "; "))
	}
	pkg.Fset = c.fset
	pkg.Syntax = files
	pkg.Types = checked
	pkg.TypesInfo = info
	pkg.TypesSizes = c.sizes
	return nil
}

func (c *sourceChecker) parse(filename string) (*ast.File, error) {
	c.filesMu.Lock()
	parsed := c.files[filename]
	if parsed == nil {
		parsed = &parsedFile{}
		c.files[filename] = parsed
	}
	c.filesMu.Unlock()
	parsed.once.Do(func() {
		var content []byte
		content, parsed.err = os.ReadFile(filename)
		if parsed.err == nil {
			parsed.file, parsed.err = parseAnalysisFile(c.fset, filename, content)
		}
	})
	return parsed.file, parsed.err
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
