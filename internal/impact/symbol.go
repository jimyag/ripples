package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
	functions := functionDeclarations(declarations)

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
		dispatchSymbols := interfaceArgumentDependencies(declaration, packages, objectIDs, functions)
		for _, dispatch := range dispatchSymbols {
			dependencies[dispatch.ID] = struct{}{}
			symbols[dispatch.ID] = dispatch
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
	if err := addEmbedDependencies(root, loaded, packages, objectIDs, symbols); err != nil {
		return nil, err
	}
	addInitializationDependencies(loaded, packages, objectIDs, symbols)
	return symbols, nil
}

func functionDeclarations(declarations []symbolDeclaration) map[*types.Func]symbolDeclaration {
	result := make(map[*types.Func]symbolDeclaration)
	for _, declaration := range declarations {
		function, ok := declaration.node.(*ast.FuncDecl)
		if !ok {
			continue
		}
		object, _ := declaration.pkg.TypesInfo.Defs[function.Name].(*types.Func)
		if object != nil {
			result[object] = declaration
		}
	}
	return result
}

func interfaceArgumentDependencies(
	declaration symbolDeclaration,
	packages map[string]Package,
	objectIDs map[types.Object]string,
	functions map[*types.Func]symbolDeclaration,
) []Symbol {
	var result []Symbol
	if declaration.node == nil {
		return result
	}
	index := 0
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		switch typedNode := node.(type) {
		case *ast.CallExpr:
			functionType := declaration.pkg.TypesInfo.TypeOf(typedNode.Fun)
			if functionType == nil {
				return true
			}
			signature, _ := functionType.Underlying().(*types.Signature)
			callee := calledObject(declaration.pkg.TypesInfo, typedNode.Fun)
			if signature == nil || callee == nil || callee.Pkg() == nil {
				return true
			}
			contextPackage := callee.Pkg().Path()
			if _, local := packages[contextPackage]; !local {
				return true
			}
			for argumentIndex, argument := range typedNode.Args {
				parameterType := callParameterType(signature, argumentIndex)
				dispatch := interfaceBindingSymbol(
					declaration,
					contextPackage,
					index,
					parameterType,
					declaration.pkg.TypesInfo.TypeOf(argument),
					objectIDs,
				)
				if dispatch == nil {
					continue
				}
				index++
				result = append(result, *dispatch)
			}
			traced := traceInterfaceCall(declaration, typedNode, functions, objectIDs)
			tracedPackages := make([]string, 0, len(traced))
			for packagePath := range traced {
				tracedPackages = append(tracedPackages, packagePath)
			}
			sort.Strings(tracedPackages)
			for _, packagePath := range tracedPackages {
				id := declaration.id + "::interface-trace::" + strconv.Itoa(index)
				index++
				result = append(result, Symbol{
					ID:           id,
					PackagePath:  packagePath,
					Hash:         stableMarkerHash("interface-trace"),
					Dependencies: sortedSet(traced[packagePath]),
				})
			}
		case *ast.CompositeLit:
			structType, contextPackage := namedStruct(declaration.pkg.TypesInfo.TypeOf(typedNode))
			if structType == nil {
				return true
			}
			if _, local := packages[contextPackage]; !local {
				return true
			}
			for elementIndex, element := range typedNode.Elts {
				fieldIndex := elementIndex
				value := ast.Expr(element)
				if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
					value = keyValue.Value
					key, _ := keyValue.Key.(*ast.Ident)
					field, _ := declaration.pkg.TypesInfo.Uses[key].(*types.Var)
					fieldIndex = structFieldIndex(structType, field)
				}
				if fieldIndex < 0 || fieldIndex >= structType.NumFields() {
					continue
				}
				dispatch := interfaceBindingSymbol(
					declaration,
					contextPackage,
					index,
					structType.Field(fieldIndex).Type(),
					declaration.pkg.TypesInfo.TypeOf(value),
					objectIDs,
				)
				if dispatch == nil {
					continue
				}
				index++
				result = append(result, *dispatch)
			}
		}
		return true
	})
	return result
}

func interfaceBindingSymbol(
	declaration symbolDeclaration,
	contextPackage string,
	index int,
	interfaceType types.Type,
	actualType types.Type,
	objectIDs map[types.Object]string,
) *Symbol {
	if interfaceType == nil || actualType == nil {
		return nil
	}
	iface, ok := interfaceType.Underlying().(*types.Interface)
	if !ok || iface.NumMethods() == 0 {
		return nil
	}
	if _, alreadyInterface := actualType.Underlying().(*types.Interface); alreadyInterface {
		return nil
	}
	if !types.AssignableTo(actualType, interfaceType) {
		return nil
	}
	dependencies := concreteInterfaceMethods(actualType, iface, objectIDs)
	if len(dependencies) == 0 {
		return nil
	}
	return &Symbol{
		ID:           declaration.id + "::interface-binding::" + strconv.Itoa(index),
		PackagePath:  contextPackage,
		Hash:         stableMarkerHash("interface-binding"),
		Dependencies: dependencies,
	}
}

func namedStruct(typ types.Type) (*types.Struct, string) {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, ok := typ.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return nil, ""
	}
	structType, _ := named.Underlying().(*types.Struct)
	return structType, named.Obj().Pkg().Path()
}

func structFieldIndex(structType *types.Struct, field *types.Var) int {
	if field == nil {
		return -1
	}
	for index := range structType.NumFields() {
		if structType.Field(index) == field {
			return index
		}
	}
	return -1
}

type interfaceCallTracer struct {
	functions map[*types.Func]symbolDeclaration
	objectIDs map[types.Object]string
	visited   map[string]struct{}
	byPackage map[string]map[string]struct{}
	active    []string
}

func traceInterfaceCall(
	caller symbolDeclaration,
	call *ast.CallExpr,
	functions map[*types.Func]symbolDeclaration,
	objectIDs map[types.Object]string,
) map[string]map[string]struct{} {
	function, _ := calledObject(caller.pkg.TypesInfo, call.Fun).(*types.Func)
	declaration, ok := functions[function]
	if !ok {
		return nil
	}
	bindings := callInterfaceBindings(caller.pkg, call, function, nil)
	if len(bindings) == 0 {
		return nil
	}
	tracer := interfaceCallTracer{
		functions: functions,
		objectIDs: objectIDs,
		visited:   make(map[string]struct{}),
		byPackage: make(map[string]map[string]struct{}),
	}
	tracer.traceFunction(declaration, function, bindings)
	return tracer.byPackage
}

func (tracer *interfaceCallTracer) traceFunction(
	declaration symbolDeclaration,
	function *types.Func,
	bindings map[types.Object]types.Type,
) {
	key := function.FullName() + bindingKey(bindings)
	if _, seen := tracer.visited[key]; seen {
		return
	}
	tracer.visited[key] = struct{}{}

	ast.Inspect(declaration.node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selection, ok := call.Fun.(*ast.SelectorExpr); ok {
			staticSelection := declaration.pkg.TypesInfo.Selections[selection]
			if staticSelection != nil {
				receiverType := resolvedExpressionType(declaration.pkg, selection.X, bindings)
				if receiverType != nil {
					if _, concrete := receiverType.Underlying().(*types.Interface); !concrete {
						method, _, _ := types.LookupFieldOrMethod(
							receiverType,
							true,
							staticSelection.Obj().Pkg(),
							staticSelection.Obj().Name(),
						)
						concreteMethod, _ := method.(*types.Func)
						if concreteMethod != nil && concreteMethod != staticSelection.Obj() {
							tracer.addMethod(concreteMethod)
							if target, exists := tracer.functions[concreteMethod]; exists {
								next := callInterfaceBindings(declaration.pkg, call, concreteMethod, bindings)
								tracer.active = append(tracer.active, concreteMethod.Pkg().Path())
								tracer.traceFunction(target, concreteMethod, next)
								tracer.active = tracer.active[:len(tracer.active)-1]
							}
							return true
						}
					}
				}
			}
		}

		callee, _ := calledObject(declaration.pkg.TypesInfo, call.Fun).(*types.Func)
		target, exists := tracer.functions[callee]
		if !exists {
			return true
		}
		next := callInterfaceBindings(declaration.pkg, call, callee, bindings)
		if len(next) > 0 {
			tracer.traceFunction(target, callee, next)
		}
		return true
	})
}

func (tracer *interfaceCallTracer) addMethod(method *types.Func) {
	id, ok := tracer.objectIDs[method]
	if !ok || method.Pkg() == nil {
		return
	}
	packagePaths := append([]string{method.Pkg().Path()}, tracer.active...)
	for _, packagePath := range packagePaths {
		if tracer.byPackage[packagePath] == nil {
			tracer.byPackage[packagePath] = make(map[string]struct{})
		}
		tracer.byPackage[packagePath][id] = struct{}{}
	}
}

func callInterfaceBindings(
	caller *gopackages.Package,
	call *ast.CallExpr,
	callee *types.Func,
	current map[types.Object]types.Type,
) map[types.Object]types.Type {
	signature, _ := callee.Type().(*types.Signature)
	if signature == nil {
		return nil
	}
	result := make(map[types.Object]types.Type)
	for index, argument := range call.Args {
		parameterType := callParameterType(signature, index)
		if parameterType == nil {
			continue
		}
		if _, ok := parameterType.Underlying().(*types.Interface); !ok {
			continue
		}
		actualType := resolvedExpressionType(caller, argument, current)
		if actualType == nil {
			continue
		}
		if _, stillInterface := actualType.Underlying().(*types.Interface); stillInterface {
			continue
		}
		parameterIndex := index
		if signature.Variadic() && parameterIndex >= signature.Params().Len() {
			parameterIndex = signature.Params().Len() - 1
		}
		if parameterIndex >= 0 && parameterIndex < signature.Params().Len() {
			result[signature.Params().At(parameterIndex)] = actualType
		}
	}
	return result
}

func resolvedExpressionType(
	pkg *gopackages.Package,
	expression ast.Expr,
	bindings map[types.Object]types.Type,
) types.Type {
	if identifier, ok := expression.(*ast.Ident); ok {
		if resolved := bindings[pkg.TypesInfo.Uses[identifier]]; resolved != nil {
			return resolved
		}
	}
	return pkg.TypesInfo.TypeOf(expression)
}

func bindingKey(bindings map[types.Object]types.Type) string {
	parts := make([]string, 0, len(bindings))
	for object, typ := range bindings {
		parts = append(parts, object.Name()+"="+types.TypeString(typ, packageQualifier))
	}
	sort.Strings(parts)
	return "::" + strings.Join(parts, ",")
}

func callParameterType(signature *types.Signature, argumentIndex int) types.Type {
	count := signature.Params().Len()
	if argumentIndex < count {
		if !signature.Variadic() || argumentIndex < count-1 {
			return signature.Params().At(argumentIndex).Type()
		}
	}
	if !signature.Variadic() || count == 0 {
		return nil
	}
	slice, _ := signature.Params().At(count - 1).Type().(*types.Slice)
	if slice == nil {
		return nil
	}
	return slice.Elem()
}

func calledObject(info *types.Info, expression ast.Expr) types.Object {
	switch typedExpression := expression.(type) {
	case *ast.Ident:
		return info.Uses[typedExpression]
	case *ast.SelectorExpr:
		if selection := info.Selections[typedExpression]; selection != nil {
			return selection.Obj()
		}
		return info.Uses[typedExpression.Sel]
	case *ast.IndexExpr:
		return calledObject(info, typedExpression.X)
	case *ast.IndexListExpr:
		return calledObject(info, typedExpression.X)
	case *ast.ParenExpr:
		return calledObject(info, typedExpression.X)
	default:
		return nil
	}
}

func concreteInterfaceMethods(actualType types.Type, iface *types.Interface, objectIDs map[types.Object]string) []string {
	dependencies := make(map[string]struct{})
	for methodIndex := range iface.NumMethods() {
		required := iface.Method(methodIndex)
		object, _, _ := types.LookupFieldOrMethod(actualType, true, required.Pkg(), required.Name())
		if id, ok := objectIDs[object]; ok {
			dependencies[id] = struct{}{}
		}
	}
	return sortedSet(dependencies)
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
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func addInitializationDependencies(loaded []*gopackages.Package, packages map[string]Package, objectIDs map[types.Object]string, symbols map[string]Symbol) {
	for _, pkg := range loaded {
		if _, local := packages[pkg.PkgPath]; !local {
			continue
		}
		dependencies := make(map[string]struct{})
		initPrefix := pkg.PkgPath + "::init::"
		for id := range symbols {
			if strings.HasPrefix(id, initPrefix) {
				dependencies[id] = struct{}{}
			}
		}
		for importedPath := range pkg.Imports {
			if _, local := packages[importedPath]; local {
				dependencies[packageInitID(importedPath)] = struct{}{}
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
					if !hasInitializationEffect(spec.Values) {
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
		id := packageInitID(pkg.PkgPath)
		symbols[id] = Symbol{
			ID:           id,
			PackagePath:  pkg.PkgPath,
			Hash:         stableMarkerHash("package-init"),
			Dependencies: sortedSet(dependencies),
		}
	}
}

func hasInitializationEffect(expressions []ast.Expr) bool {
	hasEffect := false
	for _, expression := range expressions {
		ast.Inspect(expression, func(node ast.Node) bool {
			switch typedNode := node.(type) {
			case *ast.CallExpr:
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

func packageInitID(packagePath string) string {
	return packagePath + "::package-init::$init"
}

func stableMarkerHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
