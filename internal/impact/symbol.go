package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	gopackages "golang.org/x/tools/go/packages"
)

func summarizeSymbols(root string, loaded []*gopackages.Package, packages map[string]Package) (map[string]Symbol, error) {
	objectIDs := make(map[types.Object]string)
	reportPaths := make(map[*types.Package]string)
	var (
		declarations  []symbolDeclaration
		localPackages []*gopackages.Package
		recompiled    []*gopackages.Package
	)
	// A package's test variants repeat its declarations with the same IDs, so
	// they collapse into one symbol while still registering their objects. A
	// package recompiled for another package's test repeats the plain package
	// entirely: it only registers its objects, so test code using them
	// reaches the plain declarations, and gets its own initialization.
	for _, pkg := range loaded {
		if _, local := packages[reportPath(pkg)]; !local {
			continue
		}
		reportPaths[pkg.Types] = reportPath(pkg)
		pkgDeclarations := packageDeclarations(root, pkg, objectIDs)
		if isRecompiled(pkg) {
			recompiled = append(recompiled, pkg)
			continue
		}
		localPackages = append(localPackages, pkg)
		declarations = append(declarations, pkgDeclarations...)
	}
	nonGoInputs, err := nonGoInputSymbols(root, localPackages)
	if err != nil {
		return nil, err
	}

	// From here on objectIDs and the declarations are only read, so the SSA
	// conversions are computed while the declarations are summarized.
	var (
		conversions    []map[string]map[string]struct{}
		conversionsRun sync.WaitGroup
	)
	conversionsRun.Go(func() {
		conversions = conversionDependencies(localPackages, declarations, objectIDs)
	})
	defer conversionsRun.Wait()

	summaries := make([]Symbol, len(declarations))
	if err := parallelFor(len(declarations), func(index int) error {
		declaration := declarations[index]
		hash := declaration.hash
		if hash == "" {
			var err error
			hash, err = astHash(declaration.hashNode)
			if err != nil {
				return fmt.Errorf("hash declaration %s: %w", declaration.id, err)
			}
		}
		if declaration.buildMetadataHash != "" {
			hash = stableMarkerHash(hash + "\x00" + declaration.buildMetadataHash)
		}
		dependencies := make(map[string]struct{})
		if declaration.node != nil {
			addReferenceDependencies(declaration.pkg.TypesInfo, declaration.node, objectIDs, dependencies)
		}
		if inputs, ok := nonGoInputs[declaration.pkg]; ok && declaration.usesNonGoInputs {
			dependencies[inputs.ID] = struct{}{}
		}
		delete(dependencies, declaration.id)
		summaries[index] = Symbol{
			ID:           declaration.id,
			PackagePath:  reportPath(declaration.pkg),
			Hash:         hash,
			Dependencies: sortedSet(dependencies),
		}
		return nil
	}); err != nil {
		return nil, err
	}

	symbols := make(map[string]Symbol, len(declarations))
	for _, symbol := range summaries {
		symbols[symbol.ID] = symbol
	}
	for _, inputs := range nonGoInputs {
		symbols[inputs.ID] = inputs
	}
	for packagePath, pkg := range packages {
		id := packageObjectID(packagePath, "package", "$content")
		symbols[id] = Symbol{
			ID:          id,
			PackagePath: packagePath,
			Hash:        pkg.Hash,
		}
	}
	if err := addEmbedDependencies(root, localPackages, objectIDs, symbols); err != nil {
		return nil, err
	}
	addInitializationDependencies(localPackages, recompiled, declarations, objectIDs, symbols)
	// Interfaces of recompiled packages are local too: only calls make their
	// methods dynamically reachable.
	reachable := collectDynamicMethods(slices.Concat(localPackages, recompiled), declarations)
	addTypeContractSymbols(objectIDs, reportPaths, reachable, symbols)
	conversionsRun.Wait()
	addConversionDependencies(conversions, symbols)
	return symbols, nil
}

// addReferenceDependencies records the local declarations a node statically
// depends on: referenced objects, the embedded fields a promoted selection
// passes through, the layout behind unkeyed struct literals and the
// contracts of the type arguments of the generic code it runs. Calling a
// generic function or a method of an instantiated generic type runs generic
// code that may call methods of the type arguments; merely naming an
// instantiated type runs nothing.
func addReferenceDependencies(
	info *types.Info,
	root ast.Node,
	objectIDs map[types.Object]string,
	dependencies map[string]struct{},
) {
	ast.Inspect(root, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if id, ok := objectIDs[declaredObject(info.Uses[node])]; ok {
				dependencies[id] = struct{}{}
			}
			if instance, ok := info.Instances[node]; ok {
				if _, function := info.Uses[node].(*types.Func); function {
					for argument := range instance.TypeArgs.Types() {
						addContractDependencies(argument, "contract", objectIDs, dependencies)
					}
				}
			}
		case *ast.SelectorExpr:
			if selection := info.Selections[node]; selection != nil {
				addEmbeddedPathDependencies(selection, objectIDs, dependencies)
				if method, ok := selection.Obj().(*types.Func); ok {
					receiver := method.Signature().Recv().Type()
					if pointer, ok := receiver.(*types.Pointer); ok {
						receiver = pointer.Elem()
					}
					if named, ok := types.Unalias(receiver).(*types.Named); ok {
						for argument := range named.TypeArgs().Types() {
							addContractDependencies(argument, "contract", objectIDs, dependencies)
						}
					}
				}
			}
		case *ast.CompositeLit:
			if len(node.Elts) == 0 {
				return true
			}
			if _, keyed := node.Elts[0].(*ast.KeyValueExpr); keyed {
				return true
			}
			typ := info.TypeOf(node)
			if pointer, ok := typ.(*types.Pointer); ok {
				typ = pointer.Elem()
			}
			if named, ok := types.Unalias(typ).(*types.Named); ok {
				if _, isStruct := named.Underlying().(*types.Struct); isStruct {
					if id, ok := typeSymbolID("layout", named.Origin().Obj(), objectIDs); ok {
						dependencies[id] = struct{}{}
					}
				}
			}
		}
		return true
	})
}

// nonGoInputSymbols hashes each package's non-Go sources (assembly, C, C++,
// headers, syso). Declarations implemented by those files depend on the hash:
// functions without a Go body and declarations from cgo-processed files.
func nonGoInputSymbols(root string, localPackages []*gopackages.Package) (map[*gopackages.Package]Symbol, error) {
	result := make(map[*gopackages.Package]Symbol)
	for _, pkg := range localPackages {
		if len(pkg.OtherFiles) == 0 {
			continue
		}
		hash := sha256.New()
		for _, filename := range slices.Sorted(slices.Values(pkg.OtherFiles)) {
			fileHash, err := contentHash(filename)
			if err != nil {
				return nil, fmt.Errorf("hash package input %s: %w", pkg.PkgPath, err)
			}
			relative, err := filepath.Rel(root, filename)
			if err != nil {
				relative = filename
			}
			_, _ = io.WriteString(hash, filepath.ToSlash(relative)+"\x00"+fileHash+"\x00")
		}
		id := packageObjectID(pkg.PkgPath, "inputs", "$non-go")
		result[pkg] = Symbol{
			ID:          id,
			PackagePath: reportPath(pkg),
			Hash:        hex.EncodeToString(hash.Sum(nil)),
		}
	}
	return result, nil
}

// declaredObject maps members of instantiated generic types and functions
// back to the objects of their declarations.
func declaredObject(object types.Object) types.Object {
	switch object := object.(type) {
	case *types.Func:
		return object.Origin()
	case *types.Var:
		return object.Origin()
	}
	return object
}

// addEmbeddedPathDependencies makes a promoted field or method depend on the
// embedded fields it is reached through, so replacing an embedded type
// propagates even though the selected member itself is unchanged.
func addEmbeddedPathDependencies(
	selection *types.Selection,
	objectIDs map[types.Object]string,
	dependencies map[string]struct{},
) {
	path := selection.Index()
	typ := selection.Recv()
	for _, index := range path[:len(path)-1] {
		if pointer, ok := typ.Underlying().(*types.Pointer); ok {
			typ = pointer.Elem()
		}
		structType, ok := typ.Underlying().(*types.Struct)
		if !ok || index >= structType.NumFields() {
			return
		}
		field := structType.Field(index)
		if id, ok := objectIDs[declaredObject(field)]; ok {
			dependencies[id] = struct{}{}
		}
		typ = field.Type()
	}
}

type symbolDeclaration struct {
	id                string
	node              ast.Node
	hashNode          ast.Node
	hash              string
	buildMetadataHash string
	usesNonGoInputs   bool
	pkg               *gopackages.Package
}

func packageDeclarations(root string, pkg *gopackages.Package, objectIDs map[types.Object]string) []symbolDeclaration {
	var declarations []symbolDeclaration
	for fileIndex, file := range pkg.Syntax {
		// Indexes are per file so a package and its test variant, which adds
		// files, give the same declaration the same ID.
		initIndex := 0
		blankInitializerIndex := 0
		// cgo rewrites files that import "C" into generated files whose line
		// directives still name the original file.
		cgoProcessed := !slices.Contains(pkg.GoFiles, pkg.CompiledGoFiles[fileIndex])
		filename := pkg.Fset.Position(file.Package).Filename
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
				declarations = append(declarations, symbolDeclaration{
					id:                id,
					node:              declaration,
					hashNode:          declaration,
					buildMetadataHash: declarationBuildMetadataHash(declaration.Doc),
					usesNonGoInputs:   cgoProcessed || declaration.Body == nil,
					pkg:               pkg,
				})
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch typedSpec := spec.(type) {
					case *ast.TypeSpec:
						id := packageObjectID(pkg.PkgPath, "type", typedSpec.Name.Name)
						buildMetadataHash := declarationBuildMetadataHash(declaration.Doc, typedSpec.Doc, typedSpec.Comment)
						if object := pkg.TypesInfo.Defs[typedSpec.Name]; object != nil {
							objectIDs[object] = id
						}
						if fields, kind := typeFields(typedSpec.Type); fields != nil {
							var typeParameters ast.Node
							if typedSpec.TypeParams != nil {
								typeParameters = typedSpec.TypeParams
							}
							declarations = append(declarations, symbolDeclaration{
								id:                id,
								node:              typeParameters,
								hash:              typeShellHash(typedSpec, kind),
								buildMetadataHash: buildMetadataHash,
								pkg:               pkg,
							})
							declarations = append(declarations, memberDeclarations(pkg, typedSpec.Name.Name, kind, fields, objectIDs)...)
						} else {
							declarations = append(declarations, symbolDeclaration{
								id:                id,
								node:              typedSpec,
								hashNode:          typedSpec,
								buildMetadataHash: buildMetadataHash,
								pkg:               pkg,
							})
						}
					case *ast.ValueSpec:
						buildMetadataHash := declarationBuildMetadataHash(declaration.Doc, typedSpec.Doc, typedSpec.Comment)
						for index, name := range typedSpec.Names {
							valueNode := individualValueSpec(typedSpec, index)
							if name.Name == "_" {
								initializerIndex := blankInitializerIndex
								blankInitializerIndex++
								if declaration.Tok != token.VAR || !hasInitializationEffect(pkg.TypesInfo, valueNode.Values) {
									continue
								}
								declarations = append(declarations, symbolDeclaration{
									id: blankInitializerID(
										pkg.PkgPath,
										filepath.ToSlash(relativeFilename),
										initializerIndex,
									),
									node:              valueNode,
									hashNode:          valueNode,
									buildMetadataHash: buildMetadataHash,
									usesNonGoInputs:   cgoProcessed,
									pkg:               pkg,
								})
								continue
							}
							object := pkg.TypesInfo.Defs[name]
							if object == nil {
								continue
							}
							id := packageObjectID(pkg.PkgPath, objectKind(object), name.Name)
							objectIDs[object] = id
							symbol := symbolDeclaration{
								id:                id,
								node:              valueNode,
								hashNode:          valueNode,
								buildMetadataHash: buildMetadataHash,
								usesNonGoInputs:   cgoProcessed,
								pkg:               pkg,
							}
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

func individualValueSpec(spec *ast.ValueSpec, index int) *ast.ValueSpec {
	if spec == nil || index < 0 || index >= len(spec.Names) {
		return spec
	}
	if len(spec.Values) == 1 && len(spec.Names) > 1 {
		return spec
	}
	var values []ast.Expr
	if index < len(spec.Values) {
		values = []ast.Expr{spec.Values[index]}
	}
	return &ast.ValueSpec{
		Doc:     spec.Doc,
		Names:   []*ast.Ident{spec.Names[index]},
		Type:    spec.Type,
		Values:  values,
		Comment: spec.Comment,
	}
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

func blankInitializerID(packagePath, filename string, index int) string {
	return blankInitializerPrefix(packagePath) + filename + "::" + strconv.Itoa(index)
}

func blankInitializerPrefix(packagePath string) string {
	return packagePath + "::blank-initializer::"
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

func typeShellHash(spec *ast.TypeSpec, kind string) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, spec.Name.Name)
	_, _ = hash.Write([]byte{0})
	_, _ = io.WriteString(hash, kind)
	if spec.Assign.IsValid() {
		_, _ = io.WriteString(hash, "=alias")
	}
	if spec.TypeParams != nil {
		_, _ = hash.Write([]byte{0})
		typeParams, _ := astHash(spec.TypeParams)
		_, _ = io.WriteString(hash, typeParams)
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
	if len(values) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(values))
}

// addInitializationDependencies creates one package-init symbol per loaded
// package variant. It depends on the init functions and effectful variable
// initializers in that variant's own files and on the package-init of its
// local imports, so test-only initialization never reaches the importers of
// the plain package. A package recompiled for a test runs the plain
// package's initialization inside that test binary, so its package-init
// depends on the plain one and belongs to the package under test.
func addInitializationDependencies(
	localPackages, recompiled []*gopackages.Package,
	declarations []symbolDeclaration,
	objectIDs map[types.Object]string,
	symbols map[string]Symbol,
) {
	localIDs := make(map[string]bool, len(localPackages)+len(recompiled))
	for _, pkg := range slices.Concat(localPackages, recompiled) {
		localIDs[pkg.ID] = true
	}
	for _, pkg := range recompiled {
		dependencies := map[string]struct{}{packageInitID(pkg.PkgPath): {}}
		for _, imported := range pkg.Imports {
			if localIDs[imported.ID] {
				dependencies[packageInitID(imported.ID)] = struct{}{}
			}
		}
		id := packageInitID(pkg.ID)
		symbols[id] = Symbol{
			ID:           id,
			PackagePath:  pkg.ForTest,
			Hash:         stableMarkerHash("package-init"),
			Dependencies: sortedSet(dependencies),
		}
	}
	initializers := make(map[*gopackages.Package][]string)
	for _, declaration := range declarations {
		path := declaration.pkg.PkgPath
		if strings.HasPrefix(declaration.id, path+"::init::") ||
			strings.HasPrefix(declaration.id, blankInitializerPrefix(path)) {
			initializers[declaration.pkg] = append(initializers[declaration.pkg], declaration.id)
		}
	}
	for _, pkg := range localPackages {
		dependencies := make(map[string]struct{})
		for _, id := range initializers[pkg] {
			dependencies[id] = struct{}{}
		}
		for _, imported := range pkg.Imports {
			if localIDs[imported.ID] {
				dependencies[packageInitID(imported.ID)] = struct{}{}
			}
		}
		for _, file := range pkg.Syntax {
			for _, declaration := range file.Decls {
				gen, ok := declaration.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, rawSpec := range gen.Specs {
					spec := rawSpec.(*ast.ValueSpec)
					if !hasInitializationEffect(pkg.TypesInfo, spec.Values) {
						continue
					}
					for _, name := range spec.Names {
						if id, ok := objectIDs[pkg.TypesInfo.Defs[name]]; ok {
							dependencies[id] = struct{}{}
						}
					}
				}
			}
		}
		id := packageInitID(pkg.ID)
		hashValue := "package-init"
		if buildMetadataHash := packageBuildMetadataHash(pkg); buildMetadataHash != "" {
			hashValue += "\x00" + buildMetadataHash
		}
		symbols[id] = Symbol{
			ID:           id,
			PackagePath:  reportPath(pkg),
			Hash:         stableMarkerHash(hashValue),
			Dependencies: sortedSet(dependencies),
		}
	}
}

func hasInitializationEffect(info *types.Info, expressions []ast.Expr) bool {
	hasEffect := false
	for _, expression := range expressions {
		ast.Inspect(expression, func(node ast.Node) bool {
			switch typedNode := node.(type) {
			case *ast.FuncLit:
				// Creating a function value does not execute its body. An
				// immediately invoked function literal is still effectful
				// because its enclosing CallExpr is visited first.
				return false
			case *ast.CallExpr:
				if info != nil {
					if typeAndValue, ok := info.Types[typedNode.Fun]; ok && typeAndValue.IsType() {
						if conversionMayPanic(info, typedNode) {
							hasEffect = true
							return false
						}
						return true
					}
				}
				hasEffect = true
				return false
			case *ast.UnaryExpr:
				if typedNode.Op == token.ARROW {
					hasEffect = true
					return false
				}
			}
			return !hasEffect
		})
	}
	return hasEffect
}

func conversionMayPanic(info *types.Info, call *ast.CallExpr) bool {
	if info == nil || call == nil || len(call.Args) != 1 {
		return false
	}

	source := types.Unalias(info.TypeOf(call.Args[0]))
	if source == nil {
		return false
	}
	if _, ok := source.Underlying().(*types.Slice); !ok {
		return false
	}

	target := types.Unalias(info.TypeOf(call))
	if target == nil {
		return false
	}
	target = target.Underlying()
	if pointer, ok := target.(*types.Pointer); ok {
		target = types.Unalias(pointer.Elem())
		if target == nil {
			return false
		}
		target = target.Underlying()
	}
	array, ok := target.(*types.Array)
	return ok && array.Len() > 0
}

// packageInitID identifies package initialization by package ID; a plain
// package's ID is its import path.
func packageInitID(packageID string) string {
	return packageID + "::package-init::$init"
}

func stableMarkerHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
