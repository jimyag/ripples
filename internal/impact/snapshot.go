package impact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	gopackages "golang.org/x/tools/go/packages"

	"github.com/jimyag/ripples/internal/snapshot"
)

// Package is the stable identity and dependency summary of a Go package.
type Package struct {
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	RelativePath string   `json:"relative_path"`
	Hash         string   `json:"hash"`
	Imports      []string `json:"imports,omitempty"`
}

// PackageSnapshot is the cached package graph for one Git tree.
type PackageSnapshot struct {
	Tree       string             `json:"tree"`
	ModulePath string             `json:"module_path"`
	ModuleHash string             `json:"module_hash"`
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

func buildPackageSnapshot(ctx context.Context, source *snapshot.Source) (PackageSnapshot, error) {
	cfg := &gopackages.Config{
		Context: ctx,
		Dir:     source.Dir,
		Mode: gopackages.NeedName |
			gopackages.NeedFiles |
			gopackages.NeedCompiledGoFiles |
			gopackages.NeedImports |
			gopackages.NeedDeps |
			gopackages.NeedModule |
			gopackages.NeedEmbedFiles |
			gopackages.NeedSyntax |
			gopackages.NeedTypes |
			gopackages.NeedTypesInfo |
			gopackages.NeedTypesSizes,
	}
	loaded, err := gopackages.Load(cfg, "./...")
	if err != nil {
		return PackageSnapshot{}, fmt.Errorf("load package graph: %w", err)
	}
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

	result := PackageSnapshot{
		Tree:       source.Tree,
		ModulePath: modulePath,
		ModuleHash: moduleFilesHash(source.Dir),
		Packages:   make(map[string]Package, len(loaded)),
		Symbols:    make(map[string]Symbol),
	}
	for _, loadedPackage := range loaded {
		pkg, err := summarizePackage(source.Dir, modulePath, loadedPackage)
		if err != nil {
			return PackageSnapshot{}, err
		}
		result.Packages[pkg.Path] = pkg
	}
	result.Symbols, err = summarizeSymbols(source.Dir, loaded, result.Packages)
	if err != nil {
		return PackageSnapshot{}, err
	}
	return result, nil
}

func summarizePackage(root, modulePath string, pkg *gopackages.Package) (Package, error) {
	fileHashes := make([]string, 0, len(pkg.CompiledGoFiles)+len(pkg.EmbedFiles)+len(pkg.OtherFiles))
	for _, filename := range pkg.CompiledGoFiles {
		hash, err := goFileHash(filename)
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

	return Package{
		Path:         pkg.PkgPath,
		Name:         pkg.Name,
		RelativePath: relativePackagePath(modulePath, pkg.PkgPath),
		Hash:         hex.EncodeToString(hash.Sum(nil)),
		Imports:      imports,
	}, nil
}

func goFileHash(filename string) (string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}

	hash := sha256.New()
	if err := ast.Fprint(hash, fset, file, astFieldFilter); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func astFieldFilter(name string, value reflect.Value) bool {
	if name == "Doc" || name == "Comment" || name == "Comments" {
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

func contentHash(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()

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

func moduleFilesHash(root string) string {
	hash := sha256.New()
	for _, name := range []string{"go.mod", "go.work"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		_, _ = io.WriteString(hash, name)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func relativePackagePath(modulePath, packagePath string) string {
	if packagePath == modulePath {
		return filepath.Base(modulePath)
	}
	if relative := strings.TrimPrefix(packagePath, modulePath+"/"); relative != packagePath {
		return relative
	}
	return packagePath
}
