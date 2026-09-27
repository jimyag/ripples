package impact

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/gcexportdata"
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

	// Reading the metadata and preparing the dependencies' export data are
	// the slowest steps, each a go list run, so they overlap: the metadata
	// load starts right away, and the export data of everything the
	// module's files import is loaded as soon as the files are parsed. The
	// metadata then confirms the dependencies, and a guess that missed one
	// falls back to an exact load.
	var (
		sizes    types.Sizes
		sizesErr error
		sizesRun sync.WaitGroup
	)
	sizesRun.Go(func() { sizes, sizesErr = targetSizes(ctx, dir) })
	defer sizesRun.Wait()

	// go list -compiled hashes every package in the graph to find the files
	// cgo generates, which made up a third of the metadata load. Only cgo
	// packages have such files, so the metadata leaves them out and is loaded
	// again with them when a file of the module imports "C". parseModule
	// reads every file go list can report for ./..., and a cgo file it missed
	// would still fail type-checking at import "C" instead of going unnoticed.
	quickCtx, cancelQuick := context.WithCancel(ctx)
	var (
		quick    []*gopackages.Package
		quickErr error
		quickRun sync.WaitGroup
	)
	quickRun.Go(func() { quick, quickErr = loadMetadata(quickCtx, dir, buildFlags, tests, false) })
	defer quickRun.Wait()
	defer cancelQuick()

	imports, cgo := checker.parseModule(dir, tests)
	guessCtx, cancelGuess := context.WithCancel(ctx)
	var (
		guessed    map[string]*gopackages.Package
		guessedRun sync.WaitGroup
	)
	if len(imports) > 0 {
		guessedRun.Go(func() {
			loaded, err := loadExportData(guessCtx, dir, buildFlags, imports)
			if err != nil {
				return
			}
			guessed = make(map[string]*gopackages.Package, len(loaded))
			for _, pkg := range loaded {
				guessed[pkg.ID] = pkg
			}
		})
	}
	defer guessedRun.Wait()
	defer cancelGuess()

	var (
		roots []*gopackages.Package
		err   error
	)
	if cgo {
		cancelQuick()
		roots, err = loadMetadata(ctx, dir, buildFlags, tests, true)
	} else {
		quickRun.Wait()
		roots, err = quick, quickErr
	}
	if err != nil {
		return nil, err
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
		if !cgo {
			pkg.CompiledGoFiles = pkg.GoFiles
		}
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

	sizesRun.Wait()
	if sizesErr != nil {
		return nil, sizesErr
	}
	checker.sizes = sizes
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

// loadMetadata lists the packages matching ./..., without test mains, and
// the metadata of everything they import. compiled adds the files cgo
// generates.
func loadMetadata(ctx context.Context, dir string, buildFlags []string, tests, compiled bool) ([]*gopackages.Package, error) {
	// NeedTypesSizes would also make go list compute the compiled files, so
	// the sizes come from targetSizes.
	mode := gopackages.NeedName |
		gopackages.NeedFiles |
		gopackages.NeedImports |
		gopackages.NeedDeps |
		gopackages.NeedModule |
		gopackages.NeedEmbedFiles |
		gopackages.NeedForTest
	if compiled {
		mode |= gopackages.NeedCompiledGoFiles
	}
	metadata, err := gopackages.Load(&gopackages.Config{
		Context: ctx,
		Dir:     dir,
		Mode:    mode,
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
	return roots, nil
}

// targetSizes returns the type sizes of the architecture the go command
// builds for in dir.
func targetSizes(ctx context.Context, dir string) (types.Sizes, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOARCH")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOARCH: %w", err)
	}
	return types.SizesFor("gc", strings.TrimSpace(string(output))), nil
}

// parseModule parses the Go files of the module in dir, which the metadata
// will list later, and returns the import paths outside the module's own
// packages and whether a file imports "C". It is a guess: files excluded by
// build constraints are included, and parse errors surface only if the
// metadata lists the file.
func (c *sourceChecker) parseModule(dir string, tests bool) (imports []string, cgo bool) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, false
	}
	modulePath := modfile.ModulePath(data)
	if modulePath == "" {
		return nil, false
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

	perFile := make([][]string, len(files))
	_ = parallelFor(len(files), func(index int) error {
		file, _ := c.parse(files[index])
		if file == nil {
			return nil
		}
		for _, spec := range file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil {
				perFile[index] = append(perFile[index], path)
			}
		}
		return nil
	})
	guessed := make(map[string]bool)
	for _, fileImports := range perFile {
		for _, path := range fileImports {
			if path == "C" {
				// cgo output imports these on behalf of the file.
				guessed["runtime/cgo"] = true
				guessed["syscall"] = true
				cgo = true
			}
			if !local[path] {
				guessed[path] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(guessed)), cgo
}

// loadExportData lists paths with go list -export and reads the types of
// every package that built. One import map serves all of them, so they share
// the objects of the packages they refer to. Asking go/packages for the
// types took about a fifth longer: it also has go list compute compiled
// files and report every transitive dependency.
func loadExportData(ctx context.Context, dir string, buildFlags, paths []string) ([]*gopackages.Package, error) {
	loaded, err := gopackages.Load(&gopackages.Config{
		Context:    ctx,
		Dir:        dir,
		Mode:       gopackages.NeedName | gopackages.NeedExportFile,
		BuildFlags: buildFlags,
	}, paths...)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	imports := make(map[string]*types.Package)
	for _, pkg := range loaded {
		if pkg.ExportFile == "" || len(pkg.Errors) > 0 {
			continue
		}
		if pkg.Types, err = readExportFile(fset, imports, pkg); err != nil {
			return nil, err
		}
	}
	return loaded, nil
}

func readExportFile(fset *token.FileSet, imports map[string]*types.Package, pkg *gopackages.Package) (_ *types.Package, returnErr error) {
	file, err := os.Open(pkg.ExportFile)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	reader, err := gcexportdata.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read export data of %s: %w", pkg.PkgPath, err)
	}
	typesPackage, err := gcexportdata.Read(reader, fset, imports, pkg.PkgPath)
	if err != nil {
		return nil, fmt.Errorf("read export data of %s: %w", pkg.PkgPath, err)
	}
	return typesPackage, nil
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
	loaded, err := loadExportData(ctx, dir, buildFlags, slices.Sorted(maps.Keys(dependencies)))
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
