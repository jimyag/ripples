package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"path/filepath"
	"sort"
	"strconv"

	gopackages "golang.org/x/tools/go/packages"
)

func summarizeSymbols(root string, loaded []*gopackages.Package, packages map[string]Package) (map[string]Symbol, error) {
	objectIDs := make(map[types.Object]string)
	declarations := make([]symbolDeclaration, 0)

	for _, pkg := range loaded {
		if _, local := packages[pkg.PkgPath]; !local {
			continue
		}
		pkgDeclarations := packageDeclarations(root, pkg, objectIDs)
		declarations = append(declarations, pkgDeclarations...)
	}

	symbols := make(map[string]Symbol, len(declarations))
	for _, declaration := range declarations {
		hash := declaration.hash
		if hash == "" {
			var err error
			hash, err = astHash(declaration.hashNode, declaration.pkg.Fset)
			if err != nil {
				return nil, fmt.Errorf("hash declaration %s: %w", declaration.id, err)
			}
		}

		dependencies := make(map[string]struct{})
		if declaration.node != nil {
			ast.Inspect(declaration.node, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if !ok {
					return true
				}
				used := declaration.pkg.TypesInfo.Uses[identifier]
				dependency, ok := objectIDs[used]
				if ok && dependency != declaration.id {
					dependencies[dependency] = struct{}{}
				}
				return true
			})
		}

		symbols[declaration.id] = Symbol{
			ID:           declaration.id,
			PackagePath:  declaration.pkg.PkgPath,
			Hash:         hash,
			Dependencies: sortedSet(dependencies),
		}
	}
	for packagePath, pkg := range packages {
		id := packageObjectID(packagePath, "package", "$content")
		symbols[id] = Symbol{
			ID:          id,
			PackagePath: packagePath,
			Hash:        pkg.Hash,
		}
	}
	return symbols, nil
}

type symbolDeclaration struct {
	id       string
	node     ast.Node
	hashNode ast.Node
	hash     string
	pkg      *gopackages.Package
}

func packageDeclarations(root string, pkg *gopackages.Package, objectIDs map[types.Object]string) []symbolDeclaration {
	var declarations []symbolDeclaration
	initIndex := 0
	for fileIndex, file := range pkg.Syntax {
		filename := pkg.CompiledGoFiles[fileIndex]
		relativeFilename, err := filepath.Rel(root, filename)
		if err != nil {
			relativeFilename = filename
		}

		for _, node := range file.Decls {
			switch declaration := node.(type) {
			case *ast.FuncDecl:
				id := functionID(pkg.PkgPath, declaration, pkg.TypesInfo, filepath.ToSlash(relativeFilename), initIndex)
				if declaration.Name.Name == "init" {
					initIndex++
				} else if object := pkg.TypesInfo.Defs[declaration.Name]; object != nil {
					objectIDs[object] = id
				}
				declarations = append(declarations, symbolDeclaration{id: id, node: declaration, hashNode: declaration, pkg: pkg})
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch typedSpec := spec.(type) {
					case *ast.TypeSpec:
						id := packageObjectID(pkg.PkgPath, "type", typedSpec.Name.Name)
						if object := pkg.TypesInfo.Defs[typedSpec.Name]; object != nil {
							objectIDs[object] = id
						}
						if fields, kind := typeFields(typedSpec.Type); fields != nil {
							var typeParameters ast.Node
							if typedSpec.TypeParams != nil {
								typeParameters = typedSpec.TypeParams
							}
							declarations = append(declarations, symbolDeclaration{
								id:   id,
								node: typeParameters,
								hash: typeShellHash(typedSpec, kind, pkg),
								pkg:  pkg,
							})
							declarations = append(declarations, memberDeclarations(pkg, typedSpec.Name.Name, kind, fields, objectIDs)...)
						} else {
							declarations = append(declarations, symbolDeclaration{id: id, node: typedSpec, hashNode: typedSpec, pkg: pkg})
						}
					case *ast.ValueSpec:
						for _, name := range typedSpec.Names {
							object := pkg.TypesInfo.Defs[name]
							if object == nil || name.Name == "_" {
								continue
							}
							id := packageObjectID(pkg.PkgPath, objectKind(object), name.Name)
							objectIDs[object] = id
							symbol := symbolDeclaration{id: id, node: typedSpec, hashNode: typedSpec, pkg: pkg}
							if constant, ok := object.(*types.Const); ok {
								symbol.hash = constantHash(constant)
							}
							declarations = append(declarations, symbol)
						}
					}
				}
			}
		}
	}
	return declarations
}

func functionID(packagePath string, declaration *ast.FuncDecl, info *types.Info, filename string, initIndex int) string {
	if declaration.Name.Name == "init" {
		return packagePath + "::init::" + filename + "::" + strconv.Itoa(initIndex)
	}
	if declaration.Recv == nil {
		return packageObjectID(packagePath, "func", declaration.Name.Name)
	}
	object, _ := info.Defs[declaration.Name].(*types.Func)
	if object == nil {
		return packageObjectID(packagePath, "method", declaration.Name.Name)
	}
	signature, _ := object.Type().(*types.Signature)
	receiver := receiverTypeName(signature.Recv().Type())
	return packagePath + "::method::" + receiver + "." + declaration.Name.Name
}

func receiverTypeName(receiver types.Type) string {
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	if named, ok := receiver.(*types.Named); ok && named.Obj() != nil {
		return named.Obj().Name()
	}
	return types.TypeString(receiver, func(*types.Package) string { return "" })
}

func packageObjectID(packagePath, kind, name string) string {
	return packagePath + "::" + kind + "::" + name
}

func constantHash(constant *types.Const) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, types.TypeString(constant.Type(), packageQualifier))
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, constant.Val().ExactString())
	return hex.EncodeToString(hash.Sum(nil))
}

func packageQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Path()
}

func typeFields(node ast.Expr) (*ast.FieldList, string) {
	switch typedNode := node.(type) {
	case *ast.StructType:
		return typedNode.Fields, "struct"
	case *ast.InterfaceType:
		return typedNode.Methods, "interface"
	default:
		return nil, ""
	}
}

func typeShellHash(spec *ast.TypeSpec, kind string, pkg *gopackages.Package) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, spec.Name.Name)
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, kind)
	if spec.Assign.IsValid() {
		_, _ = io.WriteString(hash, "=alias")
	}
	if spec.TypeParams != nil {
		_, _ = hash.Write([]byte{0})
		_ = ast.Fprint(hash, pkg.Fset, spec.TypeParams, astFieldFilter)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func memberDeclarations(pkg *gopackages.Package, owner, kind string, fields *ast.FieldList, objectIDs map[types.Object]string) []symbolDeclaration {
	declarations := make([]symbolDeclaration, 0, len(fields.List))
	for index, field := range fields.List {
		names := field.Names
		if len(names) == 0 {
			names = []*ast.Ident{{Name: "$embed" + strconv.Itoa(index)}}
		}
		for _, name := range names {
			memberKind := "field"
			if kind == "interface" {
				memberKind = "interface-method"
			}
			id := pkg.PkgPath + "::" + memberKind + "::" + owner + "." + name.Name
			if object := pkg.TypesInfo.Defs[name]; object != nil {
				objectIDs[object] = id
			} else if object := pkg.TypesInfo.Implicits[field]; object != nil {
				objectIDs[object] = id
			} else {
				ast.Inspect(field.Type, func(node ast.Node) bool {
					identifier, ok := node.(*ast.Ident)
					if ok {
						if object, ok := pkg.TypesInfo.Defs[identifier].(*types.Var); ok && object.IsField() {
							objectIDs[object] = id
						}
					}
					return true
				})
			}
			declarations = append(declarations, symbolDeclaration{
				id:       id,
				node:     field,
				hashNode: field,
				pkg:      pkg,
			})
		}
	}
	return declarations
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
