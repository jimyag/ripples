package impact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"

	gopackages "golang.org/x/tools/go/packages"

	"github.com/jimyag/ripples/internal/snapshot"
)

// Package is the stable identity and content hash of a Go package.
type Package struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	RelativePath string `json:"relative_path"`
	Hash         string `json:"hash"`
	// Deleted marks an affected package that exists only in the old revision.
	Deleted bool `json:"deleted,omitempty"`
}

// PackageSnapshot is the cached package graph for one Git tree.
type PackageSnapshot struct {
	Tree       string             `json:"tree"`
	ModulePath string             `json:"module_path"`
	Modules    moduleSnapshot     `json:"modules"`
	Packages   map[string]Package `json:"packages"`
	Symbols    map[string]Symbol  `json:"symbols"`
	Cached     bool               `json:"-"`
}

// Symbol is one package-level declaration and the declarations it uses.
type Symbol struct {
	ID           string   `json:"id"`
	PackagePath  string   `json:"package_path"`
	Hash         string   `json:"hash"`
	Dependencies []string `json:"dependencies,omitempty"`
}

func buildPackageSnapshot(ctx context.Context, source *snapshot.Source, prepare string) (PackageSnapshot, error) {
	if err := runPrepare(ctx, source.Dir, prepare); err != nil {
		return PackageSnapshot{}, err
	}
	// ./... already makes every local package an initial package. Omitting
	// NeedDeps keeps dependency function bodies as black boxes instead of
	// retaining syntax and type information for the full transitive graph.
	cfg := &gopackages.Config{
		Context: ctx,
		Dir:     source.Dir,
		Mode: gopackages.NeedName |
			gopackages.NeedFiles |
			gopackages.NeedCompiledGoFiles |
			gopackages.NeedImports |
			gopackages.NeedModule |
			gopackages.NeedEmbedFiles |
			gopackages.NeedSyntax |
			gopackages.NeedTypes |
			gopackages.NeedTypesInfo |
			gopackages.NeedForTest,
		// Test variants and external test packages are analyzed so test-only
		// changes and declarations used by tests reach their packages.
		Tests:     true,
		ParseFile: parseAnalysisFile,
	}
	loaded, err := gopackages.Load(cfg, "./...")
	if err != nil {
		return PackageSnapshot{}, fmt.Errorf("load package graph: %w", err)
	}
	loaded = slices.DeleteFunc(loaded, isTestMain)
	if len(loaded) == 0 {
		return PackageSnapshot{}, fmt.Errorf("no Go packages found")
	}

	var packageErrors []string
	for _, pkg := range loaded {
		for _, pkgErr := range pkg.Errors {
			packageErrors = append(packageErrors, pkgErr.Error())
		}
	}
	if len(packageErrors) > 0 {
		sort.Strings(packageErrors)
		return PackageSnapshot{}, fmt.Errorf("load package graph: %s", strings.Join(packageErrors, "; "))
	}

	modulePath := findModulePath(loaded)
	if modulePath == "" {
		return PackageSnapshot{}, fmt.Errorf("cannot determine module path")
	}

	// Module identities come from the same export, so the package graph and
	// the module graph always describe one Git tree.
	modules, err := buildModuleSnapshot(ctx, source.Dir)
	if err != nil {
		return PackageSnapshot{}, err
	}
	result := PackageSnapshot{
		Tree:       source.Tree,
		ModulePath: modulePath,
		Modules:    modules,
		Packages:   make(map[string]Package, len(loaded)),
		Symbols:    make(map[string]Symbol),
	}
	summaries := make([]Package, len(loaded))
	if err := parallelFor(len(loaded), func(index int) error {
		pkg, err := summarizePackage(source.Dir, modulePath, loaded[index])
		if err != nil {
			return err
		}
		summaries[index] = pkg
		return nil
	}); err != nil {
		return PackageSnapshot{}, err
	}
	// A package and its test variants are reported as one package whose
	// hash covers every variant and whose name comes from the plain package.
	variantHashes := make(map[string][]string)
	ranks := make(map[string]int)
	for index, pkg := range summaries {
		variantHashes[pkg.Path] = append(variantHashes[pkg.Path], pkg.Hash)
		rank := variantRank(loaded[index])
		if best, ok := ranks[pkg.Path]; !ok || rank < best {
			ranks[pkg.Path] = rank
			result.Packages[pkg.Path] = pkg
		}
	}
	for path, hashes := range variantHashes {
		pkg := result.Packages[path]
		slices.Sort(hashes)
		pkg.Hash = stableMarkerHash(strings.Join(hashes, "\x00"))
		result.Packages[path] = pkg
	}
	result.Symbols, err = summarizeSymbols(source.Dir, loaded, result.Packages)
	if err != nil {
		return PackageSnapshot{}, err
	}
	return result, nil
}

func parseAnalysisFile(
	fset *token.FileSet,
	filename string,
	src []byte,
) (*ast.File, error) {
	return parser.ParseFile(
		fset,
		filename,
		src,
		parser.ParseComments|parser.AllErrors|parser.SkipObjectResolution,
	)
}

// runPrepare runs the preparation command in the exported module directory,
// for example to generate code that the repository does not commit.
func runPrepare(ctx context.Context, dir, command string) error {
	if command == "" {
		return nil
	}
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("prepare %q: %w: %s", command, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// isTestMain reports the generated main package of a test binary.
func isTestMain(pkg *gopackages.Package) bool {
	return pkg.Name == "main" && pkg.ForTest == "" && strings.HasSuffix(pkg.ID, ".test")
}

// reportPath returns the package a loaded package is reported as: in-package
// test variants and external test packages belong to the package under test,
// while dependencies recompiled for a test keep their own path.
func reportPath(pkg *gopackages.Package) string {
	if pkg.ForTest != "" && strings.TrimSuffix(pkg.PkgPath, "_test") == pkg.ForTest {
		return pkg.ForTest
	}
	return pkg.PkgPath
}

// variantRank orders the variants of one reported package so its name comes
// from the plain package, then the in-package test variant.
func variantRank(pkg *gopackages.Package) int {
	switch {
	case pkg.ForTest == "":
		return 0
	case pkg.PkgPath == pkg.ForTest:
		return 1
	default:
		return 2
	}
}

func summarizePackage(root, modulePath string, pkg *gopackages.Package) (Package, error) {
	if len(pkg.Syntax) != len(pkg.CompiledGoFiles) {
		return Package{}, fmt.Errorf(
			"hash package %s: got %d syntax trees for %d compiled Go files",
			pkg.PkgPath,
			len(pkg.Syntax),
			len(pkg.CompiledGoFiles),
		)
	}

	fileHashes := make([]string, 0, len(pkg.CompiledGoFiles)+len(pkg.EmbedFiles)+len(pkg.OtherFiles))
	for index := range pkg.CompiledGoFiles {
		hash, err := astFileHash(pkg.Syntax[index], pkg.Fset)
		if err != nil {
			return Package{}, fmt.Errorf("hash package %s: %w", pkg.PkgPath, err)
		}
		fileHashes = append(fileHashes, "go:"+hash)
	}
	for _, filename := range append(append([]string{}, pkg.EmbedFiles...), pkg.OtherFiles...) {
		hash, err := contentHash(filename)
		if err != nil {
			return Package{}, fmt.Errorf("hash package input %s: %w", pkg.PkgPath, err)
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			rel = filename
		}
		fileHashes = append(fileHashes, "input:"+filepath.ToSlash(rel)+":"+hash)
	}
	sort.Strings(fileHashes)

	var imports []string
	for _, imported := range pkg.Imports {
		if imported != nil && imported.PkgPath != "" {
			imports = append(imports, imported.PkgPath)
		}
	}
	sort.Strings(imports)

	hash := sha256.New()
	for _, fileHash := range fileHashes {
		_, _ = io.WriteString(hash, fileHash)
		_, _ = hash.Write([]byte{0})
	}
	for _, imported := range imports {
		_, _ = io.WriteString(hash, imported)
		_, _ = hash.Write([]byte{0})
	}

	path := reportPath(pkg)
	return Package{
		Path:         path,
		Name:         strings.TrimSuffix(pkg.Name, "_test"),
		RelativePath: relativePackagePath(modulePath, path),
		Hash:         hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func astFileHash(file *ast.File, fset *token.FileSet) (string, error) {
	// packages.Load parses without SkipObjectResolution. These deprecated
	// parser-only links are not used by go/types, but the legacy hash included
	// their nil fields after reparsing with SkipObjectResolution. Exclude those
	// fields while printing so concurrent declaration hashing stays read-only.
	hash := sha256.New()
	if err := ast.Fprint(hash, fset, file, astFieldFilter); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func astFieldFilter(name string, value reflect.Value) bool {
	if name == "Doc" || name == "Comment" || name == "Comments" ||
		name == "Obj" || name == "Scope" || name == "Unresolved" {
		return false
	}
	return value.Type() != reflect.TypeFor[token.Pos]()
}

func astHash(node ast.Node, fset *token.FileSet) (string, error) {
	hash := sha256.New()
	if err := ast.Fprint(hash, fset, node, astFieldFilter); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func objectKind(object types.Object) string {
	switch object.(type) {
	case *types.Const:
		return "const"
	case *types.Func:
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Var:
		return "var"
	default:
		return "object"
	}
}

func contentHash(filename string) (_ string, returnErr error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func findModulePath(packages []*gopackages.Package) string {
	for _, pkg := range packages {
		if pkg.Module != nil && pkg.Module.Main {
			return pkg.Module.Path
		}
	}
	for _, pkg := range packages {
		if pkg.Module != nil {
			return pkg.Module.Path
		}
	}
	return ""
}

func relativePackagePath(modulePath, packagePath string) string {
	if packagePath == modulePath {
		return "."
	}
	if relative, ok := strings.CutPrefix(packagePath, modulePath+"/"); ok {
		return relative
	}
	return packagePath
}
