package impact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
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
	// Dynamic lists the methods of a dispatch contract that only interface
	// calls in local declarations invoke.
	Dynamic []DynamicDependency `json:"dynamic,omitempty"`
}

// DynamicDependency is a method that only the Callers declarations invoke,
// through interfaces. Converting the method's type affects a package only if
// the package also reaches one of those declarations.
type DynamicDependency struct {
	ID      string   `json:"id"`
	Callers []string `json:"callers"`
}

// Snapshots are cached as a manifest per tree listing content-addressed
// chunks: one per package with its symbols and module dependencies, and one
// with the module checksums. Trees share the chunks of unchanged packages,
// so caching another commit only adds the chunks of the packages it changed.
const (
	manifestNamespace = "snapshots"
	chunkNamespace    = "snapshot-chunks"
)

type snapshotManifest struct {
	Tree       string   `json:"tree"`
	ModulePath string   `json:"module_path"`
	Chunks     []string `json:"chunks"`
}

// snapshotChunk holds one package, or the module checksums when Path is empty.
type snapshotChunk struct {
	Path       string            `json:"path,omitempty"`
	Package    *Package          `json:"package,omitempty"`
	Modules    *packageModules   `json:"modules,omitempty"`
	Symbols    []Symbol          `json:"symbols,omitempty"`
	GlobalHash string            `json:"global_hash,omitempty"`
	Sums       map[string]string `json:"sums,omitempty"`
}

func storeSnapshot(cache *snapshot.Cache, key string, result *PackageSnapshot) error {
	byPath := make(map[string]*snapshotChunk)
	chunkFor := func(path string) *snapshotChunk {
		if byPath[path] == nil {
			byPath[path] = &snapshotChunk{Path: path}
		}
		return byPath[path]
	}
	for path, pkg := range result.Packages {
		chunkFor(path).Package = &pkg
	}
	for path, modules := range result.Modules.Packages {
		chunkFor(path).Modules = &modules
	}
	for _, symbol := range result.Symbols {
		chunk := chunkFor(symbol.PackagePath)
		chunk.Symbols = append(chunk.Symbols, symbol)
	}
	chunks := []*snapshotChunk{{GlobalHash: result.Modules.GlobalHash, Sums: result.Modules.Sums}}
	for _, path := range slices.Sorted(maps.Keys(byPath)) {
		chunk := byPath[path]
		slices.SortFunc(chunk.Symbols, func(a, b Symbol) int { return strings.Compare(a.ID, b.ID) })
		chunks = append(chunks, chunk)
	}

	manifest := snapshotManifest{Tree: result.Tree, ModulePath: result.ModulePath}
	for _, chunk := range chunks {
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("encode snapshot chunk: %w", err)
		}
		chunkKey := snapshot.Key(string(encoded))
		manifest.Chunks = append(manifest.Chunks, chunkKey)
		if !cache.Touch(chunkNamespace, chunkKey) {
			if err := cache.Store(chunkNamespace, chunkKey, json.RawMessage(encoded)); err != nil {
				return err
			}
		}
	}
	return cache.Store(manifestNamespace, key, manifest)
}

// loadCachedSnapshot reports a miss when the manifest or any of its chunks is
// missing or unreadable, so a partially pruned entry is rebuilt.
func loadCachedSnapshot(cache *snapshot.Cache, key string) (*PackageSnapshot, bool) {
	var manifest snapshotManifest
	if hit, err := cache.Load(manifestNamespace, key, &manifest); err != nil || !hit || len(manifest.Chunks) == 0 {
		return nil, false
	}
	result := &PackageSnapshot{
		Tree:       manifest.Tree,
		ModulePath: manifest.ModulePath,
		Modules:    moduleSnapshot{Packages: make(map[string]packageModules)},
		Packages:   make(map[string]Package),
		Symbols:    make(map[string]Symbol),
		Cached:     true,
	}
	chunks := make([]snapshotChunk, len(manifest.Chunks))
	if err := parallelFor(len(chunks), func(index int) error {
		hit, err := cache.Load(chunkNamespace, manifest.Chunks[index], &chunks[index])
		if err == nil && !hit {
			err = os.ErrNotExist
		}
		return err
	}); err != nil {
		return nil, false
	}
	for _, chunk := range chunks {
		if chunk.Path == "" {
			result.Modules.GlobalHash, result.Modules.Sums = chunk.GlobalHash, chunk.Sums
			continue
		}
		if chunk.Package != nil {
			result.Packages[chunk.Path] = *chunk.Package
		}
		if chunk.Modules != nil {
			result.Modules.Packages[chunk.Path] = *chunk.Modules
		}
		for _, symbol := range chunk.Symbols {
			result.Symbols[symbol.ID] = symbol
		}
	}
	return result, true
}

func buildPackageSnapshot(ctx context.Context, source *snapshot.Source, prepare string, tests bool) (PackageSnapshot, error) {
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
		// With tests, test variants and external test packages are analyzed so
		// test-only changes and declarations used by tests reach their packages.
		Tests: tests,
		// go list compiles export data for every listed package. Without
		// -trimpath the build cache keys that output by the export directory,
		// so every run recompiled the whole module into new cache entries;
		// with it, unchanged packages hit the cache across runs and trees.
		BuildFlags: []string{"-trimpath"},
		ParseFile:  parseAnalysisFile,
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
