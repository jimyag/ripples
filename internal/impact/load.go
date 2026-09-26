package impact

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
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
	checker := &sourceChecker{
		fset:  token.NewFileSet(),
		files: make(map[string]*parsedFile),
	}

	// Reading the metadata and preparing the dependencies' export data each
	// take go list about a second. Both run at once: the module's files are
	// parsed right away and the export data of everything they import is
	// loaded while the metadata is read. The metadata then confirms the
	// dependencies, and a guess that missed one falls back to an exact load.
	guessCtx, cancelGuess := context.WithCancel(ctx)
	var (
		guessed    map[string]*gopackages.Package
		guessedRun sync.WaitGroup
	)
	guessedRun.Go(func() {
		if imports := checker.parseModule(dir, tests); len(imports) > 0 {
			guessed = loadExportData(guessCtx, dir, buildFlags, imports)
		}
	})
	defer guessedRun.Wait()
	defer cancelGuess()

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
	guessedRun.Wait()
	if !useExportData(guessed, dependencies) {
		if err := loadDependencyTypes(ctx, dir, buildFlags, dependencies); err != nil {
			return nil, err
		}
	}

	checker.sizes = roots[0].TypesSizes
	if checker.sizes == nil {
		checker.sizes = types.SizesFor("gc", runtime.GOARCH)
	}
	checker.isRoot = isRoot
	checker.states = make(map[string]*checkState, len(source))
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

// parseModule parses the Go files of the module in dir, which the metadata
// will list later, and returns the import paths outside the module's own
// packages. It is a guess: files excluded by build constraints are included,
// and parse errors surface only if the metadata lists the file.
func (c *sourceChecker) parseModule(dir string, tests bool) []string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil
	}
	modulePath := modfile.ModulePath(data)
	if modulePath == "" {
		return nil
	}
	var files []string
	local := map[string]bool{"C": true, "unsafe": true}
	// A walk error only leaves the guess incomplete, which the exact load
	// catches, so it just stops the walk.
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == dir {
				return nil
			}
			// The go command skips these directories, and a go.mod starts
			// another module whose packages are dependencies.
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || !tests && strings.HasSuffix(name, "_test.go") {
			return nil
		}
		files = append(files, path)
		relative, err := filepath.Rel(dir, filepath.Dir(path))
		if err == nil {
			local[pathpkg.Join(modulePath, filepath.ToSlash(relative))] = true
		}
		return nil
	})

	imports := make([][]string, len(files))
	_ = parallelFor(len(files), func(index int) error {
		file, _ := c.parse(files[index])
		if file == nil {
			return nil
		}
		for _, spec := range file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil {
				imports[index] = append(imports[index], path)
			}
		}
		return nil
	})
	guessed := make(map[string]bool)
	for _, fileImports := range imports {
		for _, path := range fileImports {
			if path == "C" {
				// cgo output imports these on behalf of the file.
				guessed["runtime/cgo"] = true
				guessed["syscall"] = true
			}
			if !local[path] {
				guessed[path] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(guessed))
}

// loadExportData loads the types of paths from export data, keyed by package
// ID, or returns nil if go list fails.
func loadExportData(ctx context.Context, dir string, buildFlags, paths []string) map[string]*gopackages.Package {
	loaded, err := gopackages.Load(&gopackages.Config{
		Context:    ctx,
		Dir:        dir,
		Mode:       gopackages.NeedName | gopackages.NeedTypes,
		BuildFlags: buildFlags,
	}, paths...)
	if err != nil {
		return nil
	}
	byID := make(map[string]*gopackages.Package, len(loaded))
	for _, pkg := range loaded {
		byID[pkg.ID] = pkg
	}
	return byID
}

// useExportData takes the dependencies' types from a guessed load if it
// loaded every one of them without errors. All types then come from one load
// and share their objects; otherwise nothing is taken.
func useExportData(loaded map[string]*gopackages.Package, dependencies map[string]*gopackages.Package) bool {
	for id := range dependencies {
		pkg := loaded[id]
		if pkg == nil || pkg.Types == nil || len(pkg.Errors) > 0 {
			return false
		}
	}
	for id, dependency := range dependencies {
		dependency.Types = loaded[id].Types
	}
	return true
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
