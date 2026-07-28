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
	fieldUses := interfaceFieldUses(declarations)
	resolver := valueFlowResolver{
		functions:      functions,
		functionValues: functionValueDeclarations(declarations),
	}
	globalBindings := packageInterfaceBindings(declarations, &resolver)
	resolver.fieldBindings = packageFieldBindings(declarations, globalBindings, &resolver)

	type declarationSummary struct {
		symbol       Symbol
		dependencies map[string]struct{}
	}
	summaries := make([]declarationSummary, len(declarations))
	if err := parallelFor(len(declarations), func(index int) error {
		declaration := declarations[index]
		hash := declaration.hash
		if hash == "" {
			var err error
			hash, err = astHash(declaration.hashNode, declaration.pkg.Fset)
			if err != nil {
				return fmt.Errorf("hash declaration %s: %w", declaration.id, err)
			}
		}
		if declaration.buildMetadataHash != "" {
			hash = stableMarkerHash(hash + "\x00" + declaration.buildMetadataHash)
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

		summaries[index] = declarationSummary{
			symbol: Symbol{
				ID:          declaration.id,
				PackagePath: declaration.pkg.PkgPath,
				Hash:        hash,
			},
			dependencies: dependencies,
		}
		return nil
	}); err != nil {
		return nil, err
	}

	symbols := make(map[string]Symbol, len(declarations))
	for index, declaration := range declarations {
		summary := summaries[index]
		dispatchSymbols := interfaceDependencies(
			declaration,
			packages,
			objectIDs,
			functions,
			fieldUses,
			globalBindings,
			&resolver,
		)
		for _, dispatch := range dispatchSymbols {
			summary.dependencies[dispatch.ID] = struct{}{}
			symbols[dispatch.ID] = dispatch
		}
		summary.symbol.Dependencies = sortedSet(summary.dependencies)
		symbols[summary.symbol.ID] = summary.symbol
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

type functionValueDeclaration struct {
	pkg        *gopackages.Package
	expression ast.Expr
}

func functionValueDeclarations(declarations []symbolDeclaration) map[types.Object][]functionValueDeclaration {
	result := make(map[types.Object][]functionValueDeclaration)
	for _, declaration := range declarations {
		if declaration.node == nil {
			continue
		}
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			switch typedNode := node.(type) {
			case *ast.ValueSpec:
				if len(typedNode.Names) != len(typedNode.Values) {
					return true
				}
				for index, value := range typedNode.Values {
					if isFunctionValue(declaration.pkg, value) {
						object := declaration.pkg.TypesInfo.Defs[typedNode.Names[index]]
						result[object] = append(result[object], functionValueDeclaration{
							pkg:        declaration.pkg,
							expression: value,
						})
					}
				}
			case *ast.AssignStmt:
				if len(typedNode.Lhs) != len(typedNode.Rhs) {
					return true
				}
				for index, value := range typedNode.Rhs {
					identifier, identifierOK := typedNode.Lhs[index].(*ast.Ident)
					if !identifierOK {
						continue
					}
					if !isFunctionValue(declaration.pkg, value) {
						continue
					}
					object := declaration.pkg.TypesInfo.Defs[identifier]
					if object == nil {
						object = declaration.pkg.TypesInfo.Uses[identifier]
					}
					result[object] = append(result[object], functionValueDeclaration{
						pkg:        declaration.pkg,
						expression: value,
					})
				}
			}
			return true
		})
	}
	return result
}

func isFunctionValue(pkg *gopackages.Package, expression ast.Expr) bool {
	typ := pkg.TypesInfo.TypeOf(expression)
	if typ == nil {
		return false
	}
	_, ok := typ.Underlying().(*types.Signature)
	return ok
}

func interfaceDependencies(
	declaration symbolDeclaration,
	packages map[string]Package,
	objectIDs map[types.Object]string,
	functions map[*types.Func]symbolDeclaration,
	fieldUses map[*types.Var][]interfaceMethodUse,
	globalBindings map[types.Object][]types.Type,
	resolver *valueFlowResolver,
) []Symbol {
	var result []Symbol
	if declaration.node == nil {
		return result
	}
	bindings := declarationInterfaceBindings(declaration, globalBindings, resolver)
	parents := nodeParents(declaration.node)
	index := 0
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		switch typedNode := node.(type) {
		case *ast.CallExpr:
			if selector, ok := typedNode.Fun.(*ast.SelectorExpr); ok {
				selection := declaration.pkg.TypesInfo.Selections[selector]
				if selection != nil && selection.Kind() == types.MethodExpr && len(typedNode.Args) > 0 {
					for _, dispatch := range boundInterfaceSelectionSymbols(
						declaration,
						selector,
						typedNode.Args[0],
						bindings,
						objectIDs,
						resolver,
						index,
					) {
						index++
						result = append(result, dispatch)
					}
				}
			}
			traced := traceInterfaceCall(
				declaration,
				typedNode,
				functions,
				objectIDs,
				fieldUses,
				resolver,
				bindings,
				callResultUsed(declaration, typedNode, parents),
			)
			for _, fallback := range dependencyInterfaceFallbackSymbols(
				declaration,
				typedNode,
				bindings,
				packages,
				objectIDs,
				resolver,
				traced,
				index,
			) {
				index++
				result = append(result, fallback)
			}
			tracedPackages := make([]string, 0, len(traced))
			for packagePath := range traced {
				if _, local := packages[packagePath]; !local {
					continue
				}
				tracedPackages = append(tracedPackages, packagePath)
			}
			sort.Strings(tracedPackages)
			for _, packagePath := range tracedPackages {
				dependencies := sortedSet(traced[packagePath])
				id := dispatchSymbolID(
					declaration.id,
					"interface-trace",
					index,
					packagePath,
					dependencies,
				)
				index++
				result = append(result, Symbol{
					ID:           id,
					PackagePath:  packagePath,
					Hash:         stableMarkerHash("interface-trace"),
					Dependencies: dependencies,
				})
			}
		case *ast.SelectorExpr:
			selection := declaration.pkg.TypesInfo.Selections[typedNode]
			if selection == nil || selection.Kind() == types.MethodExpr {
				return true
			}
			for _, dispatch := range boundInterfaceSelectionSymbols(
				declaration,
				typedNode,
				typedNode.X,
				bindings,
				objectIDs,
				resolver,
				index,
			) {
				index++
				result = append(result, dispatch)
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
				actualTypes := resolver.expressionTypes(declaration.pkg, value, bindings, 0)
				dispatches := interfaceFieldBindingSymbols(
					declaration,
					index,
					structType.Field(fieldIndex),
					actualTypes,
					objectIDs,
					fieldUses,
				)
				for _, dispatch := range dispatches {
					index++
					result = append(result, dispatch)
				}
			}
		case *ast.AssignStmt:
			for assignmentIndex, left := range typedNode.Lhs {
				if assignmentIndex >= len(typedNode.Rhs) {
					break
				}
				selector, ok := left.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				selection := declaration.pkg.TypesInfo.Selections[selector]
				field, _ := selectionObject(selection).(*types.Var)
				if field == nil || !field.IsField() {
					continue
				}
				actualTypes := resolver.expressionTypes(declaration.pkg, typedNode.Rhs[assignmentIndex], bindings, 0)
				dispatches := interfaceFieldBindingSymbols(
					declaration,
					index,
					field,
					actualTypes,
					objectIDs,
					fieldUses,
				)
				for _, dispatch := range dispatches {
					index++
					result = append(result, dispatch)
				}
			}
		}
		return true
	})
	return result
}

func dependencyInterfaceFallbackSymbols(
	declaration symbolDeclaration,
	call *ast.CallExpr,
	bindings map[types.Object][]types.Type,
	packages map[string]Package,
	objectIDs map[types.Object]string,
	resolver *valueFlowResolver,
	traced map[string]map[string]struct{},
	index int,
) []Symbol {
	function, _ := calledObject(declaration.pkg.TypesInfo, call.Fun).(*types.Func)
	if function == nil || function.Pkg() == nil {
		return nil
	}
	if _, local := packages[function.Pkg().Path()]; local {
		return nil
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil {
		return nil
	}
	tracedDependencies := make(map[string]struct{})
	for _, dependencies := range traced {
		for dependency := range dependencies {
			tracedDependencies[dependency] = struct{}{}
		}
	}
	fallback := make(map[string]struct{})
	for argumentIndex, argument := range call.Args {
		parameterType := callParameterType(signature, argumentIndex)
		if parameterType == nil {
			continue
		}
		iface, ok := parameterType.Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 {
			continue
		}
		for _, actualType := range resolver.expressionTypes(declaration.pkg, argument, bindings, 0) {
			methods := concreteInterfaceMethods(actualType, iface, objectIDs)
			if intersects(methods, tracedDependencies) {
				continue
			}
			for _, method := range methods {
				fallback[method] = struct{}{}
			}
		}
	}
	if len(fallback) == 0 {
		return nil
	}
	dependencies := sortedSet(fallback)
	return []Symbol{{
		ID: dispatchSymbolID(
			declaration.id,
			"dependency-interface",
			index,
			declaration.pkg.PkgPath,
			dependencies,
		),
		PackagePath:  declaration.pkg.PkgPath,
		Hash:         stableMarkerHash("dependency-interface"),
		Dependencies: dependencies,
	}}
}

func intersects(values []string, set map[string]struct{}) bool {
	for _, value := range values {
		if _, exists := set[value]; exists {
			return true
		}
	}
	return false
}

func nodeParents(root ast.Node) map[ast.Node]ast.Node {
	result := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			result[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return result
}

func callResultUsed(
	declaration symbolDeclaration,
	call *ast.CallExpr,
	parents map[ast.Node]ast.Node,
) bool {
	parent := parents[call]
	switch typedParent := parent.(type) {
	case *ast.ExprStmt:
		return false
	case *ast.AssignStmt:
		for index, right := range typedParent.Rhs {
			if right != call {
				continue
			}
			if len(typedParent.Rhs) == 1 && len(typedParent.Lhs) > 1 {
				for _, left := range typedParent.Lhs {
					if identifier, ok := left.(*ast.Ident); ok && identifier.Name != "_" {
						return objectIsUsed(declaration.pkg.TypesInfo, declaration.pkg.TypesInfo.Defs[identifier])
					}
				}
				return false
			}
			if index >= len(typedParent.Lhs) {
				return true
			}
			identifier, ok := typedParent.Lhs[index].(*ast.Ident)
			if !ok {
				return true
			}
			if identifier.Name == "_" {
				return false
			}
			object := declaration.pkg.TypesInfo.Defs[identifier]
			if object == nil {
				object = declaration.pkg.TypesInfo.Uses[identifier]
			}
			return objectIsUsed(declaration.pkg.TypesInfo, object)
		}
	}
	return true
}

func objectIsUsed(info *types.Info, object types.Object) bool {
	if object == nil {
		return false
	}
	for _, used := range info.Uses {
		if used == object {
			return true
		}
	}
	return false
}

type interfaceMethodUse struct {
	method      *types.Func
	packagePath string
}

func interfaceFieldUses(declarations []symbolDeclaration) map[*types.Var][]interfaceMethodUse {
	result := make(map[*types.Var][]interfaceMethodUse)
	for _, declaration := range declarations {
		if declaration.node == nil {
			continue
		}
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			methodSelector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			methodSelection := declaration.pkg.TypesInfo.Selections[methodSelector]
			method, _ := selectionObject(methodSelection).(*types.Func)
			if method == nil {
				return true
			}
			if _, interfaceMethod := methodSelection.Recv().Underlying().(*types.Interface); !interfaceMethod {
				return true
			}
			fieldSelector, ok := methodSelector.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			fieldSelection := declaration.pkg.TypesInfo.Selections[fieldSelector]
			field, _ := selectionObject(fieldSelection).(*types.Var)
			if field == nil || !field.IsField() {
				return true
			}
			result[field] = append(result[field], interfaceMethodUse{
				method:      method,
				packagePath: declaration.pkg.PkgPath,
			})
			return true
		})
	}
	return result
}

func packageInterfaceBindings(
	declarations []symbolDeclaration,
	resolver *valueFlowResolver,
) map[types.Object][]types.Type {
	result := make(map[types.Object][]types.Type)
	for range 2 {
		for _, declaration := range declarations {
			spec, ok := declaration.node.(*ast.ValueSpec)
			if !ok {
				continue
			}
			addValueSpecBindings(declaration.pkg, spec, result, resolver)
		}
	}
	return result
}

func declarationInterfaceBindings(
	declaration symbolDeclaration,
	global map[types.Object][]types.Type,
	resolver *valueFlowResolver,
) map[types.Object][]types.Type {
	result := make(map[types.Object][]types.Type, len(global))
	for object, candidates := range global {
		result[object] = append([]types.Type(nil), candidates...)
	}
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		switch typedNode := node.(type) {
		case *ast.ValueSpec:
			addValueSpecBindings(declaration.pkg, typedNode, result, resolver)
		case *ast.AssignStmt:
			for index, left := range typedNode.Lhs {
				right, resultIndex := assignedExpression(typedNode.Rhs, index)
				if right == nil {
					continue
				}
				object := assignedObject(declaration.pkg.TypesInfo, left)
				addValueBinding(
					object,
					declaration.pkg.TypesInfo.TypeOf(left),
					resolver.expressionTypes(declaration.pkg, right, result, resultIndex),
					result,
				)
			}
		case *ast.RangeStmt:
			identifier, _ := typedNode.Value.(*ast.Ident)
			if identifier == nil {
				return true
			}
			object := declaration.pkg.TypesInfo.Defs[identifier]
			if object == nil {
				object = declaration.pkg.TypesInfo.Uses[identifier]
			}
			addValueBinding(
				object,
				declaration.pkg.TypesInfo.TypeOf(identifier),
				resolver.expressionTypes(declaration.pkg, typedNode.X, result, 0),
				result,
			)
		case *ast.TypeSwitchStmt:
			assertion := typeSwitchAssertion(typedNode.Assign)
			if assertion == nil {
				return true
			}
			candidates := resolver.expressionTypes(declaration.pkg, assertion.X, result, 0)
			for _, statement := range typedNode.Body.List {
				caseClause, _ := statement.(*ast.CaseClause)
				if caseClause == nil {
					continue
				}
				object, _ := declaration.pkg.TypesInfo.Implicits[caseClause].(*types.Var)
				if object == nil {
					continue
				}
				addValueBinding(object, object.Type(), candidates, result)
			}
		case *ast.SendStmt:
			object := assignedObject(declaration.pkg.TypesInfo, typedNode.Chan)
			addValueBinding(
				object,
				declaration.pkg.TypesInfo.TypeOf(typedNode.Chan),
				resolver.expressionTypes(declaration.pkg, typedNode.Value, result, 0),
				result,
			)
		}
		return true
	})
	return result
}

func typeSwitchAssertion(statement ast.Stmt) *ast.TypeAssertExpr {
	var expression ast.Expr
	switch typedStatement := statement.(type) {
	case *ast.AssignStmt:
		if len(typedStatement.Rhs) == 1 {
			expression = typedStatement.Rhs[0]
		}
	case *ast.ExprStmt:
		expression = typedStatement.X
	}
	assertion, _ := expression.(*ast.TypeAssertExpr)
	return assertion
}

func packageFieldBindings(
	declarations []symbolDeclaration,
	global map[types.Object][]types.Type,
	resolver *valueFlowResolver,
) map[types.Object][]types.Type {
	result := make(map[types.Object][]types.Type)
	resolver.fieldBindings = result
	for range 2 {
		for _, declaration := range declarations {
			if declaration.node == nil {
				continue
			}
			bindings := declarationInterfaceBindings(declaration, global, resolver)
			ast.Inspect(declaration.node, func(node ast.Node) bool {
				switch typedNode := node.(type) {
				case *ast.CompositeLit:
					structType, _ := namedStruct(declaration.pkg.TypesInfo.TypeOf(typedNode))
					if structType == nil {
						return true
					}
					for index, element := range typedNode.Elts {
						fieldIndex := index
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
						field := structType.Field(fieldIndex)
						addValueBinding(
							field,
							field.Type(),
							resolver.expressionTypes(declaration.pkg, value, bindings, 0),
							result,
						)
					}
				case *ast.AssignStmt:
					for index, left := range typedNode.Lhs {
						selector, ok := left.(*ast.SelectorExpr)
						if !ok {
							continue
						}
						field, _ := selectionObject(declaration.pkg.TypesInfo.Selections[selector]).(*types.Var)
						right, resultIndex := assignedExpression(typedNode.Rhs, index)
						if field == nil || right == nil {
							continue
						}
						addValueBinding(
							field,
							field.Type(),
							resolver.expressionTypes(declaration.pkg, right, bindings, resultIndex),
							result,
						)
					}
				}
				return true
			})
		}
	}
	return result
}

func addValueSpecBindings(
	pkg *gopackages.Package,
	spec *ast.ValueSpec,
	bindings map[types.Object][]types.Type,
	resolver *valueFlowResolver,
) {
	for index, name := range spec.Names {
		value, resultIndex := assignedExpression(spec.Values, index)
		if value == nil {
			continue
		}
		object := pkg.TypesInfo.Defs[name]
		addValueBinding(
			object,
			pkg.TypesInfo.TypeOf(name),
			resolver.expressionTypes(pkg, value, bindings, resultIndex),
			bindings,
		)
	}
}

func assignedExpression(expressions []ast.Expr, index int) (ast.Expr, int) {
	if index < len(expressions) {
		return expressions[index], 0
	}
	if len(expressions) == 1 {
		return expressions[0], index
	}
	return nil, 0
}

func assignedObject(info *types.Info, expression ast.Expr) types.Object {
	switch typedExpression := expression.(type) {
	case *ast.Ident:
		object := info.Uses[typedExpression]
		if object == nil {
			object = info.Defs[typedExpression]
		}
		return object
	case *ast.IndexExpr:
		return assignedObject(info, typedExpression.X)
	case *ast.ParenExpr:
		return assignedObject(info, typedExpression.X)
	default:
		return nil
	}
}

func addValueBinding(
	object types.Object,
	targetType types.Type,
	candidates []types.Type,
	bindings map[types.Object][]types.Type,
) {
	if object == nil || targetType == nil {
		return
	}
	acceptedType := targetType
	switch underlying := targetType.Underlying().(type) {
	case *types.Slice:
		acceptedType = underlying.Elem()
	case *types.Array:
		acceptedType = underlying.Elem()
	case *types.Map:
		acceptedType = underlying.Elem()
	case *types.Chan:
		acceptedType = underlying.Elem()
	}
	for _, candidate := range candidates {
		if types.AssignableTo(candidate, acceptedType) {
			bindings[object] = appendUniqueType(bindings[object], candidate)
		}
	}
}

func appendUniqueType(existing []types.Type, candidate types.Type) []types.Type {
	key := types.TypeString(candidate, packageQualifier)
	for _, current := range existing {
		if types.TypeString(current, packageQualifier) == key {
			return existing
		}
	}
	return append(existing, candidate)
}

type valueFlowResolver struct {
	functions      map[*types.Func]symbolDeclaration
	functionValues map[types.Object][]functionValueDeclaration
	fieldBindings  map[types.Object][]types.Type
	resolving      map[string]struct{}
}

func (resolver *valueFlowResolver) expressionTypes(
	pkg *gopackages.Package,
	expression ast.Expr,
	bindings map[types.Object][]types.Type,
	resultIndex int,
) []types.Type {
	switch typedExpression := expression.(type) {
	case *ast.Ident:
		object := pkg.TypesInfo.Uses[typedExpression]
		if object == nil {
			object = pkg.TypesInfo.Defs[typedExpression]
		}
		if candidates := bindings[object]; len(candidates) > 0 {
			return candidates
		}
	case *ast.ParenExpr:
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, resultIndex)
	case *ast.TypeAssertExpr:
		asserted := pkg.TypesInfo.TypeOf(typedExpression)
		if asserted == nil {
			return nil
		}
		if _, isInterface := asserted.Underlying().(*types.Interface); !isInterface {
			return []types.Type{asserted}
		}
		var result []types.Type
		for _, candidate := range resolver.expressionTypes(pkg, typedExpression.X, bindings, 0) {
			if types.AssignableTo(candidate, asserted) {
				result = appendUniqueType(result, candidate)
			}
		}
		return result
	case *ast.SelectorExpr:
		field, _ := selectionObject(pkg.TypesInfo.Selections[typedExpression]).(*types.Var)
		if call, ok := typedExpression.X.(*ast.CallExpr); ok {
			if candidates := resolver.callFieldTypes(pkg, call, field, bindings); len(candidates) > 0 {
				return candidates
			}
		}
		if candidates := resolver.fieldBindings[field]; len(candidates) > 0 {
			return candidates
		}
	case *ast.IndexExpr:
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0)
	case *ast.IndexListExpr:
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0)
	case *ast.UnaryExpr:
		if typedExpression.Op == token.ARROW {
			return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0)
		}
	case *ast.CompositeLit:
		typ := pkg.TypesInfo.TypeOf(typedExpression)
		if typ == nil {
			return nil
		}
		switch typ.Underlying().(type) {
		case *types.Slice, *types.Array, *types.Map:
			var result []types.Type
			for _, element := range typedExpression.Elts {
				value := ast.Expr(element)
				if keyValue, ok := element.(*ast.KeyValueExpr); ok {
					value = keyValue.Value
				}
				for _, candidate := range resolver.expressionTypes(pkg, value, bindings, 0) {
					result = appendUniqueType(result, candidate)
				}
			}
			return result
		}
	case *ast.CallExpr:
		if identifier, ok := typedExpression.Fun.(*ast.Ident); ok && identifier.Name == "append" && len(typedExpression.Args) > 0 {
			var result []types.Type
			for _, argument := range typedExpression.Args {
				for _, candidate := range resolver.expressionTypes(pkg, argument, bindings, 0) {
					result = appendUniqueType(result, candidate)
				}
			}
			return result
		}
		if result := resolver.callResultTypes(pkg, typedExpression, bindings, resultIndex); len(result) > 0 {
			return result
		}
	}
	typ := pkg.TypesInfo.TypeOf(expression)
	if tuple, ok := typ.(*types.Tuple); ok {
		if resultIndex >= 0 && resultIndex < tuple.Len() {
			typ = tuple.At(resultIndex).Type()
		} else {
			return nil
		}
	}
	if typ == nil {
		return nil
	}
	if _, isInterface := typ.Underlying().(*types.Interface); isInterface {
		return nil
	}
	return []types.Type{typ}
}

func (resolver *valueFlowResolver) callFieldTypes(
	caller *gopackages.Package,
	call *ast.CallExpr,
	field *types.Var,
	current map[types.Object][]types.Type,
) []types.Type {
	if field == nil {
		return nil
	}
	function, _ := calledObject(caller.TypesInfo, call.Fun).(*types.Func)
	declaration, ok := resolver.functions[function]
	if !ok {
		return nil
	}
	signature, _ := function.Type().(*types.Signature)
	functionNode, _ := declaration.node.(*ast.FuncDecl)
	if signature == nil || functionNode == nil {
		return nil
	}
	bindings := resolver.callBindings(caller, call, signature, current)
	bindings = declarationInterfaceBindings(declaration, bindings, resolver)
	var result []types.Type
	ast.Inspect(functionNode.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		returnStatement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, expression := range returnStatement.Results {
			composite, _ := expression.(*ast.CompositeLit)
			if composite == nil {
				continue
			}
			structType, _ := namedStruct(declaration.pkg.TypesInfo.TypeOf(composite))
			if structType == nil {
				continue
			}
			for index, element := range composite.Elts {
				fieldIndex := index
				value := ast.Expr(element)
				if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
					value = keyValue.Value
					key, _ := keyValue.Key.(*ast.Ident)
					keyField, _ := declaration.pkg.TypesInfo.Uses[key].(*types.Var)
					fieldIndex = structFieldIndex(structType, keyField)
				}
				if fieldIndex < 0 || fieldIndex >= structType.NumFields() || structType.Field(fieldIndex) != field {
					continue
				}
				for _, candidate := range resolver.expressionTypes(declaration.pkg, value, bindings, 0) {
					result = appendUniqueType(result, candidate)
				}
			}
		}
		return true
	})
	return result
}

func (resolver *valueFlowResolver) callResultTypes(
	caller *gopackages.Package,
	call *ast.CallExpr,
	current map[types.Object][]types.Type,
	resultIndex int,
) []types.Type {
	if literal, ok := call.Fun.(*ast.FuncLit); ok {
		signature, _ := caller.TypesInfo.TypeOf(literal).(*types.Signature)
		if signature == nil {
			return nil
		}
		bindings := resolver.callBindings(caller, call, signature, current)
		return resolver.returnTypes(caller, literal.Body, signature, bindings, resultIndex)
	}
	if object := calledObject(caller.TypesInfo, call.Fun); object != nil {
		var result []types.Type
		for _, value := range resolver.functionValues[object] {
			result = appendUniqueTypes(
				result,
				resolver.functionValueResultTypes(caller, call, value, current, resultIndex),
			)
		}
		if len(result) > 0 {
			return result
		}
	}

	function, _ := calledObject(caller.TypesInfo, call.Fun).(*types.Func)
	return resolver.namedFunctionResultTypes(caller, call, function, current, resultIndex)
}

func (resolver *valueFlowResolver) functionValueResultTypes(
	caller *gopackages.Package,
	call *ast.CallExpr,
	value functionValueDeclaration,
	current map[types.Object][]types.Type,
	resultIndex int,
) []types.Type {
	if literal, ok := value.expression.(*ast.FuncLit); ok {
		signature, _ := value.pkg.TypesInfo.TypeOf(literal).(*types.Signature)
		if signature == nil {
			return nil
		}
		key := fmt.Sprintf("function-value:%p::%d", literal, resultIndex)
		if !resolver.startResolving(key) {
			return nil
		}
		defer resolver.stopResolving(key)

		bindings := make(map[types.Object][]types.Type, len(current))
		for currentObject, candidates := range current {
			bindings[currentObject] = append([]types.Type(nil), candidates...)
		}
		for parameter, candidates := range resolver.callBindings(caller, call, signature, current) {
			bindings[parameter] = appendUniqueTypes(bindings[parameter], candidates)
		}
		return resolver.returnTypes(value.pkg, literal.Body, signature, bindings, resultIndex)
	}

	object := calledObject(value.pkg.TypesInfo, value.expression)
	if function, ok := object.(*types.Func); ok {
		return resolver.namedFunctionResultTypes(caller, call, function, current, resultIndex)
	}
	if object == nil {
		return nil
	}
	key := fmt.Sprintf("function-value-object:%p::%d", object, resultIndex)
	if !resolver.startResolving(key) {
		return nil
	}
	defer resolver.stopResolving(key)

	var result []types.Type
	for _, candidate := range resolver.functionValues[object] {
		result = appendUniqueTypes(
			result,
			resolver.functionValueResultTypes(caller, call, candidate, current, resultIndex),
		)
	}
	return result
}

func (resolver *valueFlowResolver) namedFunctionResultTypes(
	caller *gopackages.Package,
	call *ast.CallExpr,
	function *types.Func,
	current map[types.Object][]types.Type,
	resultIndex int,
) []types.Type {
	declaration, ok := resolver.functions[function]
	if !ok {
		return nil
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil || resultIndex < 0 || resultIndex >= signature.Results().Len() {
		return nil
	}
	bindings := resolver.callBindings(caller, call, signature, current)
	key := function.FullName() + bindingCandidatesKey(bindings) + "::" + strconv.Itoa(resultIndex)
	if !resolver.startResolving(key) {
		return nil
	}
	defer resolver.stopResolving(key)

	bindings = declarationInterfaceBindings(declaration, bindings, resolver)
	functionNode, _ := declaration.node.(*ast.FuncDecl)
	if functionNode == nil {
		return nil
	}
	return resolver.returnTypes(declaration.pkg, functionNode.Body, signature, bindings, resultIndex)
}

func (resolver *valueFlowResolver) startResolving(key string) bool {
	if resolver.resolving == nil {
		resolver.resolving = make(map[string]struct{})
	}
	if _, active := resolver.resolving[key]; active {
		return false
	}
	resolver.resolving[key] = struct{}{}
	return true
}

func (resolver *valueFlowResolver) stopResolving(key string) {
	delete(resolver.resolving, key)
}

func appendUniqueTypes(existing, candidates []types.Type) []types.Type {
	for _, candidate := range candidates {
		existing = appendUniqueType(existing, candidate)
	}
	return existing
}

func (resolver *valueFlowResolver) callBindings(
	caller *gopackages.Package,
	call *ast.CallExpr,
	signature *types.Signature,
	current map[types.Object][]types.Type,
) map[types.Object][]types.Type {
	result := make(map[types.Object][]types.Type)
	for index, argument := range call.Args {
		parameterIndex := index
		if signature.Variadic() && parameterIndex >= signature.Params().Len() {
			parameterIndex = signature.Params().Len() - 1
		}
		if parameterIndex < 0 || parameterIndex >= signature.Params().Len() {
			continue
		}
		parameter := signature.Params().At(parameterIndex)
		for _, candidate := range resolver.expressionTypes(caller, argument, current, 0) {
			result[parameter] = appendUniqueType(result[parameter], candidate)
		}
	}
	if signature.Recv() != nil {
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			for _, candidate := range resolver.expressionTypes(caller, selector.X, current, 0) {
				result[signature.Recv()] = appendUniqueType(result[signature.Recv()], candidate)
			}
		}
	}
	return result
}

func (resolver *valueFlowResolver) returnTypes(
	pkg *gopackages.Package,
	body *ast.BlockStmt,
	signature *types.Signature,
	bindings map[types.Object][]types.Type,
	resultIndex int,
) []types.Type {
	if body == nil || resultIndex < 0 || resultIndex >= signature.Results().Len() {
		return nil
	}
	var result []types.Type
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		returnStatement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(returnStatement.Results) == 0 {
			for _, candidate := range bindings[signature.Results().At(resultIndex)] {
				result = appendUniqueType(result, candidate)
			}
			return true
		}
		expression, nestedResult := assignedExpression(returnStatement.Results, resultIndex)
		if expression == nil {
			return true
		}
		for _, candidate := range resolver.expressionTypes(pkg, expression, bindings, nestedResult) {
			result = appendUniqueType(result, candidate)
		}
		return true
	})
	return result
}

func bindingCandidatesKey(bindings map[types.Object][]types.Type) string {
	parts := make([]string, 0, len(bindings))
	for object, candidates := range bindings {
		typesForObject := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			typesForObject = append(typesForObject, types.TypeString(candidate, packageQualifier))
		}
		sort.Strings(typesForObject)
		parts = append(parts, object.Name()+"="+strings.Join(typesForObject, "|"))
	}
	sort.Strings(parts)
	return "::" + strings.Join(parts, ",")
}

func boundInterfaceSelectionSymbols(
	declaration symbolDeclaration,
	selector *ast.SelectorExpr,
	receiver ast.Expr,
	bindings map[types.Object][]types.Type,
	objectIDs map[types.Object]string,
	resolver *valueFlowResolver,
	index int,
) []Symbol {
	selection := declaration.pkg.TypesInfo.Selections[selector]
	required, _ := selectionObject(selection).(*types.Func)
	if required == nil {
		return nil
	}
	if _, interfaceCall := selection.Recv().Underlying().(*types.Interface); !interfaceCall {
		return nil
	}
	dependencies := make(map[string]struct{})
	for _, actualType := range resolver.expressionTypes(declaration.pkg, receiver, bindings, 0) {
		method, _, _ := types.LookupFieldOrMethod(actualType, true, required.Pkg(), required.Name())
		if id, exists := objectIDs[method]; exists {
			dependencies[id] = struct{}{}
		}
	}
	if len(dependencies) == 0 {
		return nil
	}
	sortedDependencies := sortedSet(dependencies)
	return []Symbol{{
		ID: dispatchSymbolID(
			declaration.id,
			"interface-selection",
			index,
			declaration.pkg.PkgPath,
			sortedDependencies,
		),
		PackagePath:  declaration.pkg.PkgPath,
		Hash:         stableMarkerHash("interface-selection"),
		Dependencies: sortedDependencies,
	}}
}

func interfaceFieldBindingSymbols(
	declaration symbolDeclaration,
	index int,
	field *types.Var,
	actualTypes []types.Type,
	objectIDs map[types.Object]string,
	fieldUses map[*types.Var][]interfaceMethodUse,
) []Symbol {
	if field == nil {
		return nil
	}
	iface, ok := field.Type().Underlying().(*types.Interface)
	if !ok || iface.NumMethods() == 0 {
		return nil
	}
	byPackage := make(map[string]map[string]struct{})
	for _, actualType := range actualTypes {
		if !types.AssignableTo(actualType, field.Type()) {
			continue
		}
		for _, use := range fieldUses[field] {
			method, _, _ := types.LookupFieldOrMethod(actualType, true, use.method.Pkg(), use.method.Name())
			id, exists := objectIDs[method]
			if !exists {
				continue
			}
			if byPackage[use.packagePath] == nil {
				byPackage[use.packagePath] = make(map[string]struct{})
			}
			byPackage[use.packagePath][id] = struct{}{}
		}
	}
	packagePaths := make([]string, 0, len(byPackage))
	for packagePath := range byPackage {
		packagePaths = append(packagePaths, packagePath)
	}
	sort.Strings(packagePaths)
	result := make([]Symbol, 0, len(packagePaths))
	for offset, packagePath := range packagePaths {
		dependencies := sortedSet(byPackage[packagePath])
		result = append(result, Symbol{
			ID: dispatchSymbolID(
				declaration.id,
				"interface-field",
				index+offset,
				packagePath,
				dependencies,
			),
			PackagePath:  packagePath,
			Hash:         stableMarkerHash("interface-field"),
			Dependencies: dependencies,
		})
	}
	return result
}

func selectionObject(selection *types.Selection) types.Object {
	if selection == nil {
		return nil
	}
	return selection.Obj()
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
	functions   map[*types.Func]symbolDeclaration
	objectIDs   map[types.Object]string
	fieldUses   map[*types.Var][]interfaceMethodUse
	resolver    *valueFlowResolver
	visited     map[string]struct{}
	byPackage   map[string]map[string]struct{}
	active      []string
	traceFields bool
}

func traceInterfaceCall(
	caller symbolDeclaration,
	call *ast.CallExpr,
	functions map[*types.Func]symbolDeclaration,
	objectIDs map[types.Object]string,
	fieldUses map[*types.Var][]interfaceMethodUse,
	resolver *valueFlowResolver,
	callerBindings map[types.Object][]types.Type,
	traceFields bool,
) map[string]map[string]struct{} {
	function, _ := calledObject(caller.pkg.TypesInfo, call.Fun).(*types.Func)
	declaration, ok := functions[function]
	if !ok {
		return nil
	}
	signature, _ := function.Type().(*types.Signature)
	if signature == nil {
		return nil
	}
	bindings := resolver.callBindings(caller.pkg, call, signature, callerBindings)
	if len(bindings) == 0 {
		return nil
	}
	tracer := interfaceCallTracer{
		functions:   functions,
		objectIDs:   objectIDs,
		fieldUses:   fieldUses,
		resolver:    resolver,
		visited:     make(map[string]struct{}),
		byPackage:   make(map[string]map[string]struct{}),
		active:      []string{declaration.pkg.PkgPath},
		traceFields: traceFields,
	}
	tracer.traceFunction(declaration, function, bindings)
	return tracer.byPackage
}

func (tracer *interfaceCallTracer) traceFunction(
	declaration symbolDeclaration,
	function *types.Func,
	bindings map[types.Object][]types.Type,
) {
	key := function.FullName() + bindingCandidatesKey(bindings)
	if _, seen := tracer.visited[key]; seen {
		return
	}
	tracer.visited[key] = struct{}{}

	ast.Inspect(declaration.node, func(node ast.Node) bool {
		if composite, ok := node.(*ast.CompositeLit); ok {
			tracer.traceCompositeLiteral(declaration, composite, bindings)
			return true
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selection, ok := call.Fun.(*ast.SelectorExpr); ok {
			staticSelection := declaration.pkg.TypesInfo.Selections[selection]
			if staticSelection != nil {
				for _, receiverType := range tracer.resolver.expressionTypes(
					declaration.pkg,
					selection.X,
					bindings,
					0,
				) {
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
								signature, _ := concreteMethod.Type().(*types.Signature)
								next := tracer.resolver.callBindings(declaration.pkg, call, signature, bindings)
								tracer.active = append(tracer.active, concreteMethod.Pkg().Path())
								tracer.traceFunction(target, concreteMethod, next)
								tracer.active = tracer.active[:len(tracer.active)-1]
							}
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
		signature, _ := callee.Type().(*types.Signature)
		next := tracer.resolver.callBindings(declaration.pkg, call, signature, bindings)
		if len(next) > 0 {
			tracer.traceFunction(target, callee, next)
		}
		return true
	})
}

func (tracer *interfaceCallTracer) traceCompositeLiteral(
	declaration symbolDeclaration,
	composite *ast.CompositeLit,
	bindings map[types.Object][]types.Type,
) {
	structType, _ := namedStruct(declaration.pkg.TypesInfo.TypeOf(composite))
	if structType == nil {
		return
	}
	for elementIndex, element := range composite.Elts {
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
		if !tracer.traceFields {
			continue
		}
		for _, actualType := range tracer.resolver.expressionTypes(declaration.pkg, value, bindings, 0) {
			tracer.addFieldBinding(structType.Field(fieldIndex), actualType)
		}
	}
}

func (tracer *interfaceCallTracer) addFieldBinding(field *types.Var, actualType types.Type) {
	if field == nil || actualType == nil || !types.AssignableTo(actualType, field.Type()) {
		return
	}
	for _, use := range tracer.fieldUses[field] {
		method, _, _ := types.LookupFieldOrMethod(actualType, true, use.method.Pkg(), use.method.Name())
		id, exists := tracer.objectIDs[method]
		if !exists {
			continue
		}
		if tracer.byPackage[use.packagePath] == nil {
			tracer.byPackage[use.packagePath] = make(map[string]struct{})
		}
		tracer.byPackage[use.packagePath][id] = struct{}{}
	}
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

func concreteInterfaceMethods(
	actualType types.Type,
	iface *types.Interface,
	objectIDs map[types.Object]string,
) []string {
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
	id                string
	node              ast.Node
	hashNode          ast.Node
	hash              string
	buildMetadataHash string
	pkg               *gopackages.Package
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
				declarations = append(declarations, symbolDeclaration{
					id:                id,
					node:              declaration,
					hashNode:          declaration,
					buildMetadataHash: declarationBuildMetadataHash(declaration.Doc),
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
								hash:              typeShellHash(typedSpec, kind, pkg),
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
						for _, name := range typedSpec.Names {
							object := pkg.TypesInfo.Defs[name]
							if object == nil || name.Name == "_" {
								continue
							}
							id := packageObjectID(pkg.PkgPath, objectKind(object), name.Name)
							objectIDs[object] = id
							symbol := symbolDeclaration{
								id:                id,
								node:              typedSpec,
								hashNode:          typedSpec,
								buildMetadataHash: buildMetadataHash,
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
		hashValue := "package-init"
		if buildMetadataHash := packageBuildMetadataHash(pkg); buildMetadataHash != "" {
			hashValue += "\x00" + buildMetadataHash
		}
		symbols[id] = Symbol{
			ID:           id,
			PackagePath:  pkg.PkgPath,
			Hash:         stableMarkerHash(hashValue),
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

func dispatchSymbolID(
	declarationID, kind string,
	index int,
	packagePath string,
	dependencies []string,
) string {
	identity := packagePath + "\x00" + strings.Join(dependencies, "\x00")
	return declarationID +
		"::" + kind +
		"::" + strconv.Itoa(index) +
		"::" + stableMarkerHash(identity)
}
