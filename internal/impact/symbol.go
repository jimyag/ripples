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
		usedObjects:    packageUsedObjects(loaded),
	}
	globalBindings := packageInterfaceBindings(declarations, &resolver)
	resolver.globalBindings = globalBindings
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
	pkg         *gopackages.Package
	expression  ast.Expr
	resultIndex int
	field       *types.Var
	index       ast.Expr
}

type functionTarget struct {
	pkg              *gopackages.Package
	literal          *ast.FuncLit
	function         *types.Func
	receiver         ast.Expr
	methodExpression bool
	captured         functionBindings
}

type functionBindings map[types.Object][]functionTarget

func functionValueDeclarations(declarations []symbolDeclaration) map[types.Object][]functionValueDeclaration {
	result := make(map[types.Object][]functionValueDeclaration)
	for _, declaration := range declarations {
		if declaration.node == nil {
			continue
		}
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			switch typedNode := node.(type) {
			case *ast.ValueSpec:
				for index, name := range typedNode.Names {
					value, resultIndex := assignedExpression(typedNode.Values, index)
					if value == nil || !containsFunctionValue(declaration.pkg.TypesInfo.TypeOf(name)) {
						continue
					}
					object := declaration.pkg.TypesInfo.Defs[name]
					result[object] = append(result[object], functionValueDeclaration{
						pkg:         declaration.pkg,
						expression:  value,
						resultIndex: resultIndex,
					})
				}
			case *ast.CompositeLit:
				structType, _ := namedStruct(declaration.pkg.TypesInfo.TypeOf(typedNode))
				if structType == nil {
					return true
				}
				for index, element := range typedNode.Elts {
					fieldIndex := index
					value := element
					if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
						value = keyValue.Value
						key, _ := keyValue.Key.(*ast.Ident)
						field, _ := declaration.pkg.TypesInfo.Uses[key].(*types.Var)
						fieldIndex = structFieldIndex(structType, field)
					}
					if fieldIndex < 0 ||
						fieldIndex >= structType.NumFields() ||
						!containsFunctionValue(structType.Field(fieldIndex).Type()) {
						continue
					}
					field := structType.Field(fieldIndex)
					result[field] = append(result[field], functionValueDeclaration{
						pkg:        declaration.pkg,
						expression: value,
					})
				}
			case *ast.AssignStmt:
				for index, left := range typedNode.Lhs {
					value, resultIndex := assignedExpression(typedNode.Rhs, index)
					if value == nil || !containsFunctionValue(declaration.pkg.TypesInfo.TypeOf(left)) {
						continue
					}
					object := assignedObject(declaration.pkg.TypesInfo, left)
					var (
						field      *types.Var
						valueIndex ast.Expr
					)
					if selector, ok := left.(*ast.SelectorExpr); ok {
						field, _ = selectionObject(declaration.pkg.TypesInfo.Selections[selector]).(*types.Var)
						if receiver := assignedObject(declaration.pkg.TypesInfo, selector.X); receiver != nil {
							object = receiver
						} else {
							object = field
						}
					}
					if indexed, ok := left.(*ast.IndexExpr); ok {
						valueIndex = indexed.Index
					}
					valueDeclaration := functionValueDeclaration{
						pkg:         declaration.pkg,
						expression:  value,
						resultIndex: resultIndex,
						field:       field,
						index:       valueIndex,
					}
					result[object] = append(result[object], valueDeclaration)
					if field != nil && object != field {
						result[field] = append(result[field], valueDeclaration)
					}
				}
			case *ast.RangeStmt:
				value, _ := typedNode.Value.(*ast.Ident)
				if value == nil || !containsFunctionValue(declaration.pkg.TypesInfo.TypeOf(value)) {
					return true
				}
				object := declaration.pkg.TypesInfo.Defs[value]
				if object == nil {
					object = declaration.pkg.TypesInfo.Uses[value]
				}
				result[object] = append(result[object], functionValueDeclaration{
					pkg:        declaration.pkg,
					expression: typedNode.X,
				})
			case *ast.SendStmt:
				if !containsFunctionValue(declaration.pkg.TypesInfo.TypeOf(typedNode.Chan)) {
					return true
				}
				object := assignedObject(declaration.pkg.TypesInfo, typedNode.Chan)
				result[object] = append(result[object], functionValueDeclaration{
					pkg:        declaration.pkg,
					expression: typedNode.Value,
				})
			}
			return true
		})
	}
	return result
}

func containsFunctionValue(typ types.Type) bool {
	return containsFunctionValueSeen(typ, true, make(map[types.Type]struct{}))
}

func containsFunctionValueSeen(
	typ types.Type,
	inspectStruct bool,
	seen map[types.Type]struct{},
) bool {
	if typ == nil {
		return false
	}
	if _, ok := seen[typ]; ok {
		return false
	}
	seen[typ] = struct{}{}
	switch underlying := typ.Underlying().(type) {
	case *types.Signature:
		return true
	case *types.Slice:
		return containsFunctionValueSeen(underlying.Elem(), inspectStruct, seen)
	case *types.Array:
		return containsFunctionValueSeen(underlying.Elem(), inspectStruct, seen)
	case *types.Map:
		return containsFunctionValueSeen(underlying.Elem(), inspectStruct, seen)
	case *types.Chan:
		return containsFunctionValueSeen(underlying.Elem(), inspectStruct, seen)
	case *types.Pointer:
		return containsFunctionValueSeen(underlying.Elem(), inspectStruct, seen)
	case *types.Struct:
		if !inspectStruct {
			return false
		}
		for field := range underlying.Fields() {
			if containsFunctionValueSeen(field.Type(), false, seen) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (resolver *valueFlowResolver) functionTargets(
	pkg *gopackages.Package,
	expression ast.Expr,
	bindings functionBindings,
	resultIndex int,
) []functionTarget {
	switch typedExpression := expression.(type) {
	case *ast.FuncLit:
		return []functionTarget{{
			pkg:      pkg,
			literal:  typedExpression,
			captured: cloneFunctionBindings(bindings),
		}}
	case *ast.ParenExpr:
		return resolver.functionTargets(pkg, typedExpression.X, bindings, resultIndex)
	case *ast.UnaryExpr:
		if typedExpression.Op == token.AND {
			return resolver.functionTargets(pkg, typedExpression.X, bindings, resultIndex)
		}
		if typedExpression.Op == token.ARROW {
			return resolver.functionTargets(pkg, typedExpression.X, bindings, resultIndex)
		}
	case *ast.IndexExpr:
		if isFunctionContainer(pkg.TypesInfo.TypeOf(typedExpression.X)) {
			return resolver.functionIndexedTargets(
				pkg,
				typedExpression.X,
				typedExpression.Index,
				bindings,
			)
		}
	case *ast.SelectorExpr:
		selection := pkg.TypesInfo.Selections[typedExpression]
		field, _ := selectionObject(selection).(*types.Var)
		if field != nil && field.IsField() && containsFunctionValue(field.Type()) {
			if targets, resolved := resolver.functionFieldTargets(
				pkg,
				typedExpression.X,
				field,
				bindings,
			); resolved {
				return targets
			}
		}
		if method, ok := selectionObject(selection).(*types.Func); ok {
			switch selection.Kind() {
			case types.MethodVal:
				return []functionTarget{{
					pkg:      pkg,
					function: method,
					receiver: typedExpression.X,
					captured: cloneFunctionBindings(bindings),
				}}
			case types.MethodExpr:
				return []functionTarget{{
					pkg:              pkg,
					function:         method,
					methodExpression: true,
					captured:         cloneFunctionBindings(bindings),
				}}
			}
		}
	case *ast.CompositeLit:
		var result []functionTarget
		for _, element := range typedExpression.Elts {
			value := element
			if keyValue, ok := element.(*ast.KeyValueExpr); ok {
				value = keyValue.Value
			}
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionTargets(pkg, value, bindings, 0),
			)
		}
		return result
	case *ast.CallExpr:
		if convertedType, ok := functionConversionType(pkg, typedExpression); ok &&
			containsFunctionValue(convertedType) &&
			len(typedExpression.Args) == 1 {
			return resolver.functionTargets(pkg, typedExpression.Args[0], bindings, resultIndex)
		}
		if identifier, ok := typedExpression.Fun.(*ast.Ident); ok && identifier.Name == "append" {
			var result []functionTarget
			for _, argument := range typedExpression.Args {
				result = appendUniqueFunctionTargets(
					result,
					resolver.functionTargets(pkg, argument, bindings, 0),
				)
			}
			return result
		}
		var result []functionTarget
		for _, target := range resolver.functionTargets(pkg, typedExpression.Fun, bindings, 0) {
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionReturnTargets(pkg, typedExpression, target, bindings, resultIndex),
			)
		}
		return result
	}

	object := calledObject(pkg.TypesInfo, expression)
	if function, ok := object.(*types.Func); ok {
		return []functionTarget{{pkg: pkg, function: function}}
	}
	if object == nil {
		return nil
	}
	return resolver.functionTargetsForObject(object, bindings, resultIndex)
}

func functionConversionType(pkg *gopackages.Package, call *ast.CallExpr) (types.Type, bool) {
	if pkg == nil || call == nil {
		return nil, false
	}
	value, ok := pkg.TypesInfo.Types[call.Fun]
	return value.Type, ok && value.IsType()
}

func isFunctionContainer(typ types.Type) bool {
	if typ == nil {
		return false
	}
	switch typ.Underlying().(type) {
	case *types.Slice, *types.Array, *types.Map, *types.Chan:
		return containsFunctionValue(typ)
	default:
		return false
	}
}

func (resolver *valueFlowResolver) functionIndexedTargets(
	pkg *gopackages.Package,
	container ast.Expr,
	index ast.Expr,
	bindings functionBindings,
) []functionTarget {
	queryKey, queryKnown := expressionConstantKey(pkg, index)
	return resolver.functionIndexedTargetsKey(pkg, container, bindings, queryKey, queryKnown)
}

func (resolver *valueFlowResolver) functionIndexedTargetsKey(
	pkg *gopackages.Package,
	container ast.Expr,
	bindings functionBindings,
	queryKey string,
	queryKnown bool,
) []functionTarget {
	switch typedContainer := container.(type) {
	case *ast.ParenExpr:
		return resolver.functionIndexedTargetsKey(pkg, typedContainer.X, bindings, queryKey, queryKnown)
	case *ast.CompositeLit:
		return resolver.compositeIndexedFunctionTargets(pkg, typedContainer, bindings, queryKey, queryKnown)
	case *ast.CallExpr:
		var result []functionTarget
		for _, target := range resolver.functionTargets(pkg, typedContainer.Fun, bindings, 0) {
			signature, body, targetPackage := resolver.functionTargetBody(target)
			if signature == nil || body == nil || signature.Results().Len() == 0 {
				continue
			}
			targetBindings := mergeFunctionBindings(
				target.captured,
				resolver.callTargetFunctionBindings(pkg, typedContainer, target, signature, bindings),
			)
			ast.Inspect(body, func(node ast.Node) bool {
				if _, nested := node.(*ast.FuncLit); nested {
					return false
				}
				returnStatement, ok := node.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, expression := range returnStatement.Results {
					result = appendUniqueFunctionTargets(
						result,
						resolver.functionIndexedTargetsKey(
							targetPackage,
							expression,
							targetBindings,
							queryKey,
							queryKnown,
						),
					)
				}
				return true
			})
		}
		return result
	}

	object := assignedObject(pkg.TypesInfo, container)
	if object == nil {
		return resolver.functionTargets(pkg, container, bindings, 0)
	}
	var result []functionTarget
	result = appendUniqueFunctionTargets(result, bindings[object])
	for _, declaration := range resolver.functionValues[object] {
		if declaration.index != nil {
			storedKey, storedKnown := expressionConstantKey(declaration.pkg, declaration.index)
			if queryKnown && storedKnown && queryKey != storedKey {
				continue
			}
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionTargets(
					declaration.pkg,
					declaration.expression,
					bindings,
					declaration.resultIndex,
				),
			)
			continue
		}
		if composite, ok := declaration.expression.(*ast.CompositeLit); ok && queryKnown {
			result = appendUniqueFunctionTargets(
				result,
				resolver.compositeIndexedFunctionTargets(
					declaration.pkg,
					composite,
					bindings,
					queryKey,
					queryKnown,
				),
			)
			continue
		}
		result = appendUniqueFunctionTargets(
			result,
			resolver.functionTargets(
				declaration.pkg,
				declaration.expression,
				bindings,
				declaration.resultIndex,
			),
		)
	}
	return result
}

func (resolver *valueFlowResolver) compositeIndexedFunctionTargets(
	pkg *gopackages.Package,
	composite *ast.CompositeLit,
	bindings functionBindings,
	queryKey string,
	known bool,
) []functionTarget {
	if !known {
		return resolver.functionTargets(pkg, composite, bindings, 0)
	}
	var result []functionTarget
	for elementIndex, element := range composite.Elts {
		value := element
		elementKey := strconv.Itoa(elementIndex)
		if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
			value = keyValue.Value
			var keyKnown bool
			elementKey, keyKnown = expressionConstantKey(pkg, keyValue.Key)
			if !keyKnown {
				result = appendUniqueFunctionTargets(
					result,
					resolver.functionTargets(pkg, value, bindings, 0),
				)
				continue
			}
		}
		if elementKey != queryKey {
			continue
		}
		result = appendUniqueFunctionTargets(
			result,
			resolver.functionTargets(pkg, value, bindings, 0),
		)
	}
	return result
}

func expressionConstantKey(pkg *gopackages.Package, expression ast.Expr) (string, bool) {
	if pkg == nil || expression == nil {
		return "", false
	}
	value := pkg.TypesInfo.Types[expression].Value
	if value == nil {
		return "", false
	}
	return value.ExactString(), true
}

func (resolver *valueFlowResolver) functionFieldTargets(
	pkg *gopackages.Package,
	receiver ast.Expr,
	field *types.Var,
	bindings functionBindings,
) ([]functionTarget, bool) {
	key := fmt.Sprintf("function-field:%p:%p", receiver, field)
	if object := assignedObject(pkg.TypesInfo, receiver); object != nil {
		key = fmt.Sprintf("function-field-object:%p:%p", object, field)
	}
	if !resolver.startResolving(key) {
		return nil, true
	}
	defer resolver.stopResolving(key)

	switch typedReceiver := receiver.(type) {
	case *ast.ParenExpr:
		return resolver.functionFieldTargets(pkg, typedReceiver.X, field, bindings)
	case *ast.StarExpr:
		return resolver.functionFieldTargets(pkg, typedReceiver.X, field, bindings)
	case *ast.CompositeLit:
		structType, _ := namedStruct(pkg.TypesInfo.TypeOf(typedReceiver))
		if structType == nil {
			return nil, false
		}
		for index, element := range typedReceiver.Elts {
			fieldIndex := index
			value := element
			if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
				value = keyValue.Value
				key, _ := keyValue.Key.(*ast.Ident)
				keyField, _ := pkg.TypesInfo.Uses[key].(*types.Var)
				fieldIndex = structFieldIndex(structType, keyField)
			}
			if fieldIndex >= 0 && fieldIndex < structType.NumFields() && structType.Field(fieldIndex) == field {
				return resolver.functionTargets(pkg, value, bindings, 0), true
			}
		}
		return nil, true
	case *ast.CallExpr:
		var result []functionTarget
		resolved := false
		for _, target := range resolver.functionTargets(pkg, typedReceiver.Fun, bindings, 0) {
			targets, targetResolved := resolver.functionReturnFieldTargets(
				pkg,
				typedReceiver,
				target,
				bindings,
				field,
			)
			resolved = resolved || targetResolved
			result = appendUniqueFunctionTargets(result, targets)
		}
		return result, resolved
	}

	object := assignedObject(pkg.TypesInfo, receiver)
	if object == nil {
		return nil, false
	}
	declarations := resolver.functionValues[object]
	if len(declarations) == 0 {
		return nil, false
	}
	var result []functionTarget
	resolved := false
	for _, declaration := range declarations {
		if declaration.field != nil {
			if declaration.field != field {
				continue
			}
			resolved = true
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionTargets(
					declaration.pkg,
					declaration.expression,
					bindings,
					declaration.resultIndex,
				),
			)
			continue
		}
		targets, declarationResolved := resolver.functionFieldTargets(
			declaration.pkg,
			declaration.expression,
			field,
			bindings,
		)
		resolved = resolved || declarationResolved
		result = appendUniqueFunctionTargets(result, targets)
	}
	return result, resolved
}

func (resolver *valueFlowResolver) functionReturnFieldTargets(
	caller *gopackages.Package,
	call *ast.CallExpr,
	target functionTarget,
	current functionBindings,
	field *types.Var,
) ([]functionTarget, bool) {
	signature, body, pkg := resolver.functionTargetBody(target)
	if signature == nil || body == nil || signature.Results().Len() == 0 {
		return nil, false
	}
	bindings := mergeFunctionBindings(
		target.captured,
		resolver.callTargetFunctionBindings(caller, call, target, signature, current),
	)
	var result []functionTarget
	resolved := false
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		returnStatement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, expression := range returnStatement.Results {
			targets, expressionResolved := resolver.functionFieldTargets(
				pkg,
				expression,
				field,
				bindings,
			)
			resolved = resolved || expressionResolved
			result = appendUniqueFunctionTargets(result, targets)
		}
		return true
	})
	return result, resolved
}

func (resolver *valueFlowResolver) functionTargetsForObject(
	object types.Object,
	bindings functionBindings,
	resultIndex int,
) []functionTarget {
	key := fmt.Sprintf("function-target:%p::%d", object, resultIndex)
	if !resolver.startResolving(key) {
		return nil
	}
	defer resolver.stopResolving(key)

	result := append([]functionTarget(nil), bindings[object]...)
	for _, declaration := range resolver.functionValues[object] {
		result = appendUniqueFunctionTargets(
			result,
			resolver.functionTargets(
				declaration.pkg,
				declaration.expression,
				bindings,
				declaration.resultIndex,
			),
		)
	}
	return result
}

func (resolver *valueFlowResolver) functionReturnTargets(
	caller *gopackages.Package,
	call *ast.CallExpr,
	target functionTarget,
	current functionBindings,
	resultIndex int,
) []functionTarget {
	signature, body, pkg := resolver.functionTargetBody(target)
	if signature == nil || body == nil || resultIndex < 0 || resultIndex >= signature.Results().Len() {
		return nil
	}
	// A recursive call may reach the same function with progressively richer
	// bindings. Those bindings affect precision, but they must not make the
	// recursion guard itself unbounded.
	key := "function-return-target:" + functionTargetIdentity(target) + "::" + strconv.Itoa(resultIndex)
	if !resolver.startResolving(key) {
		return nil
	}
	defer resolver.stopResolving(key)

	bindings := mergeFunctionBindings(
		target.captured,
		resolver.callFunctionBindings(caller, call, signature, current),
	)
	var result []functionTarget
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		returnStatement, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(returnStatement.Results) == 0 {
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionTargetsForObject(signature.Results().At(resultIndex), bindings, 0),
			)
			return true
		}
		expression, nestedResult := assignedExpression(returnStatement.Results, resultIndex)
		if expression != nil {
			result = appendUniqueFunctionTargets(
				result,
				resolver.functionTargets(pkg, expression, bindings, nestedResult),
			)
		}
		return true
	})
	return result
}

func (resolver *valueFlowResolver) functionTargetBody(
	target functionTarget,
) (*types.Signature, *ast.BlockStmt, *gopackages.Package) {
	if target.literal != nil {
		signature, _ := target.pkg.TypesInfo.TypeOf(target.literal).(*types.Signature)
		return signature, target.literal.Body, target.pkg
	}
	declaration, ok := resolver.functions[target.function]
	if !ok {
		return nil, nil, nil
	}
	function, _ := declaration.node.(*ast.FuncDecl)
	signature, _ := target.function.Type().(*types.Signature)
	if function == nil {
		return nil, nil, nil
	}
	return signature, function.Body, declaration.pkg
}

func appendUniqueFunctionTargets(existing, candidates []functionTarget) []functionTarget {
	seen := make(map[string]struct{}, len(existing))
	for _, target := range existing {
		seen[functionTargetKey(target)] = struct{}{}
	}
	for _, target := range candidates {
		key := functionTargetKey(target)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		existing = append(existing, target)
	}
	return existing
}

func functionTargetKey(target functionTarget) string {
	identity := functionTargetIdentity(target)
	if len(target.captured) > 0 {
		identity += functionBindingCandidatesKey(target.captured)
	}
	return identity
}

func functionTargetIdentity(target functionTarget) string {
	if target.literal != nil {
		return fmt.Sprintf("literal:%p", target.literal)
	}
	if target.function != nil {
		identity := "function:" + target.function.FullName()
		if target.receiver != nil {
			identity += fmt.Sprintf(":receiver:%p", target.receiver)
		}
		if target.methodExpression {
			identity += ":method-expression"
		}
		return identity
	}
	return ""
}

func functionBindingCandidatesKey(bindings functionBindings) string {
	parts := make([]string, 0, len(bindings))
	for object, targets := range bindings {
		targetKeys := make([]string, 0, len(targets))
		for _, target := range targets {
			targetKeys = append(targetKeys, functionTargetIdentity(target))
		}
		sort.Strings(targetKeys)
		parts = append(parts, object.Name()+"="+strings.Join(targetKeys, "|"))
	}
	sort.Strings(parts)
	return "::functions:" + strings.Join(parts, ",")
}

func cloneFunctionBindings(bindings functionBindings) functionBindings {
	return mergeFunctionBindings(nil, bindings)
}

func mergeFunctionBindings(bindings ...functionBindings) functionBindings {
	var result functionBindings
	for _, current := range bindings {
		for object, targets := range current {
			if result == nil {
				result = make(functionBindings)
			}
			result[object] = appendUniqueFunctionTargets(result[object], targets)
		}
	}
	return result
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
				callResultUsed(declaration, typedNode, parents, resolver.usedObjects[declaration.pkg]),
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
				value := element
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
	usedObjects map[types.Object]struct{},
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
						return objectIsUsed(usedObjects, declaration.pkg.TypesInfo.Defs[identifier])
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
			return objectIsUsed(usedObjects, object)
		}
	}
	return true
}

func packageUsedObjects(loaded []*gopackages.Package) map[*gopackages.Package]map[types.Object]struct{} {
	result := make(map[*gopackages.Package]map[types.Object]struct{}, len(loaded))
	for _, pkg := range loaded {
		used := make(map[types.Object]struct{})
		for _, object := range pkg.TypesInfo.Uses {
			if object != nil {
				used[object] = struct{}{}
			}
		}
		result[pkg] = used
	}
	return result
}

func objectIsUsed(usedObjects map[types.Object]struct{}, object types.Object) bool {
	if object == nil {
		return false
	}
	_, used := usedObjects[object]
	return used
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
	initial map[types.Object][]types.Type,
	resolver *valueFlowResolver,
) map[types.Object][]types.Type {
	referenced := make(map[types.Object]struct{})
	ast.Inspect(declaration.node, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		object := declaration.pkg.TypesInfo.Uses[identifier]
		if object == nil {
			object = declaration.pkg.TypesInfo.Defs[identifier]
		}
		if object != nil {
			referenced[object] = struct{}{}
		}
		return true
	})

	result := make(map[types.Object][]types.Type, len(referenced))
	for object := range referenced {
		result[object] = appendUniqueTypes(result[object], resolver.globalBindings[object])
		result[object] = appendUniqueTypes(result[object], initial[object])
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
				targetType := declaration.pkg.TypesInfo.TypeOf(left)
				if !acceptsInterfaceBinding(targetType) {
					continue
				}
				addValueBinding(
					object,
					targetType,
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
			targetType := declaration.pkg.TypesInfo.TypeOf(identifier)
			if !acceptsInterfaceBinding(targetType) {
				return true
			}
			addValueBinding(
				object,
				targetType,
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
			targetType := declaration.pkg.TypesInfo.TypeOf(typedNode.Chan)
			if !acceptsInterfaceBinding(targetType) {
				return true
			}
			addValueBinding(
				object,
				targetType,
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
						value := element
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
						if !acceptsInterfaceBinding(field.Type()) {
							continue
						}
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
						if !acceptsInterfaceBinding(field.Type()) {
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
		targetType := pkg.TypesInfo.TypeOf(name)
		if !acceptsInterfaceBinding(targetType) {
			continue
		}
		addValueBinding(
			object,
			targetType,
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
	case *ast.StarExpr:
		return assignedObject(info, typedExpression.X)
	case *ast.UnaryExpr:
		if typedExpression.Op == token.MUL || typedExpression.Op == token.ARROW {
			return assignedObject(info, typedExpression.X)
		}
		return nil
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
	if object == nil || !acceptsInterfaceBinding(targetType) {
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

func acceptsInterfaceBinding(targetType types.Type) bool {
	if targetType == nil {
		return false
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
	_, ok := acceptedType.Underlying().(*types.Interface)
	return ok
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
	usedObjects    map[*gopackages.Package]map[types.Object]struct{}
	globalBindings map[types.Object][]types.Type
	fieldBindings  map[types.Object][]types.Type
	resolving      map[string]struct{}
}

func (resolver *valueFlowResolver) expressionTypes(
	pkg *gopackages.Package,
	expression ast.Expr,
	bindings map[types.Object][]types.Type,
	resultIndex int,
	functionContext ...functionBindings,
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
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, resultIndex, functionContext...)
	case *ast.TypeAssertExpr:
		asserted := pkg.TypesInfo.TypeOf(typedExpression)
		if asserted == nil {
			return nil
		}
		if _, isInterface := asserted.Underlying().(*types.Interface); !isInterface {
			return []types.Type{asserted}
		}
		var result []types.Type
		for _, candidate := range resolver.expressionTypes(pkg, typedExpression.X, bindings, 0, functionContext...) {
			if types.AssignableTo(candidate, asserted) {
				result = appendUniqueType(result, candidate)
			}
		}
		return result
	case *ast.SelectorExpr:
		field, _ := selectionObject(pkg.TypesInfo.Selections[typedExpression]).(*types.Var)
		if call, ok := typedExpression.X.(*ast.CallExpr); ok {
			if candidates := resolver.callFieldTypes(pkg, call, field, bindings, functionContext...); len(candidates) > 0 {
				return candidates
			}
		}
		if candidates := resolver.fieldBindings[field]; len(candidates) > 0 {
			return candidates
		}
	case *ast.IndexExpr:
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0, functionContext...)
	case *ast.IndexListExpr:
		return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0, functionContext...)
	case *ast.UnaryExpr:
		if typedExpression.Op == token.ARROW {
			return resolver.expressionTypes(pkg, typedExpression.X, bindings, 0, functionContext...)
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
				value := element
				if keyValue, ok := element.(*ast.KeyValueExpr); ok {
					value = keyValue.Value
				}
				for _, candidate := range resolver.expressionTypes(pkg, value, bindings, 0, functionContext...) {
					result = appendUniqueType(result, candidate)
				}
			}
			return result
		}
	case *ast.CallExpr:
		if identifier, ok := typedExpression.Fun.(*ast.Ident); ok && identifier.Name == "append" && len(typedExpression.Args) > 0 {
			var result []types.Type
			for _, argument := range typedExpression.Args {
				for _, candidate := range resolver.expressionTypes(pkg, argument, bindings, 0, functionContext...) {
					result = appendUniqueType(result, candidate)
				}
			}
			return result
		}
		if result := resolver.callResultTypes(pkg, typedExpression, bindings, resultIndex, functionContext...); len(result) > 0 {
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
	functionContext ...functionBindings,
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
	bindings := resolver.callBindings(caller, call, signature, current, functionContext...)
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
				value := element
				if keyValue, keyed := element.(*ast.KeyValueExpr); keyed {
					value = keyValue.Value
					key, _ := keyValue.Key.(*ast.Ident)
					keyField, _ := declaration.pkg.TypesInfo.Uses[key].(*types.Var)
					fieldIndex = structFieldIndex(structType, keyField)
				}
				if fieldIndex < 0 || fieldIndex >= structType.NumFields() || structType.Field(fieldIndex) != field {
					continue
				}
				for _, candidate := range resolver.expressionTypes(declaration.pkg, value, bindings, 0, functionContext...) {
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
	functionContext ...functionBindings,
) []types.Type {
	var result []types.Type
	bindings := firstFunctionBindings(functionContext)
	for _, target := range resolver.functionTargets(caller, call.Fun, bindings, 0) {
		result = appendUniqueTypes(
			result,
			resolver.functionTargetResultTypes(caller, call, target, current, bindings, resultIndex),
		)
	}
	return result
}

func (resolver *valueFlowResolver) functionTargetResultTypes(
	caller *gopackages.Package,
	call *ast.CallExpr,
	target functionTarget,
	current map[types.Object][]types.Type,
	currentFunctions functionBindings,
	resultIndex int,
) []types.Type {
	signature, body, pkg := resolver.functionTargetBody(target)
	if signature == nil || resultIndex < 0 || resultIndex >= signature.Results().Len() {
		return nil
	}
	bindings := resolver.callTargetBindings(
		caller,
		call,
		target,
		signature,
		current,
		currentFunctions,
	)
	functionBindings := mergeFunctionBindings(
		target.captured,
		resolver.callTargetFunctionBindings(caller, call, target, signature, currentFunctions),
	)
	// Inspect every return in the active function once. Recursive calls can
	// carry different bindings, but treating each binding set as a new stack
	// frame creates unbounded analysis contexts in large repositories.
	key := "function-result-types:" + functionTargetIdentity(target) + "::" + strconv.Itoa(resultIndex)
	if !resolver.startResolving(key) {
		return nil
	}
	defer resolver.stopResolving(key)

	if target.function != nil {
		declaration := resolver.functions[target.function]
		bindings = declarationInterfaceBindings(declaration, bindings, resolver)
	}
	return resolver.returnTypes(pkg, body, signature, bindings, resultIndex, functionBindings)
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
	functionContext ...functionBindings,
) map[types.Object][]types.Type {
	return resolver.callBindingsOffset(caller, call, signature, current, 0, functionContext...)
}

func (resolver *valueFlowResolver) callTargetBindings(
	caller *gopackages.Package,
	call *ast.CallExpr,
	target functionTarget,
	signature *types.Signature,
	current map[types.Object][]types.Type,
	functionContext ...functionBindings,
) map[types.Object][]types.Type {
	offset := 0
	if target.methodExpression {
		offset = 1
	}
	result := resolver.callBindingsOffset(
		caller,
		call,
		signature,
		current,
		offset,
		functionContext...,
	)
	if signature.Recv() == nil {
		return result
	}
	receiver := target.receiver
	receiverPackage := target.pkg
	if target.methodExpression && len(call.Args) > 0 {
		receiver = call.Args[0]
		receiverPackage = caller
	}
	if receiver == nil {
		return result
	}
	for _, candidate := range resolver.expressionTypes(
		receiverPackage,
		receiver,
		current,
		0,
		functionContext...,
	) {
		result[signature.Recv()] = appendUniqueType(result[signature.Recv()], candidate)
	}
	return result
}

func (resolver *valueFlowResolver) callBindingsOffset(
	caller *gopackages.Package,
	call *ast.CallExpr,
	signature *types.Signature,
	current map[types.Object][]types.Type,
	offset int,
	functionContext ...functionBindings,
) map[types.Object][]types.Type {
	result := make(map[types.Object][]types.Type)
	for index, argument := range call.Args[offset:] {
		parameterIndex := index
		if signature.Variadic() && parameterIndex >= signature.Params().Len() {
			parameterIndex = signature.Params().Len() - 1
		}
		if parameterIndex < 0 || parameterIndex >= signature.Params().Len() {
			continue
		}
		parameter := signature.Params().At(parameterIndex)
		for _, candidate := range resolver.expressionTypes(caller, argument, current, 0, functionContext...) {
			result[parameter] = appendUniqueType(result[parameter], candidate)
		}
	}
	if signature.Recv() != nil {
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
			for _, candidate := range resolver.expressionTypes(caller, selector.X, current, 0, functionContext...) {
				result[signature.Recv()] = appendUniqueType(result[signature.Recv()], candidate)
			}
		}
	}
	return result
}

func (resolver *valueFlowResolver) callFunctionBindings(
	caller *gopackages.Package,
	call *ast.CallExpr,
	signature *types.Signature,
	current functionBindings,
) functionBindings {
	return resolver.callFunctionBindingsOffset(caller, call, signature, current, 0)
}

func (resolver *valueFlowResolver) callTargetFunctionBindings(
	caller *gopackages.Package,
	call *ast.CallExpr,
	target functionTarget,
	signature *types.Signature,
	current functionBindings,
) functionBindings {
	offset := 0
	if target.methodExpression {
		offset = 1
	}
	return resolver.callFunctionBindingsOffset(caller, call, signature, current, offset)
}

func (resolver *valueFlowResolver) callFunctionBindingsOffset(
	caller *gopackages.Package,
	call *ast.CallExpr,
	signature *types.Signature,
	current functionBindings,
	offset int,
) functionBindings {
	result := make(functionBindings)
	for index, argument := range call.Args[offset:] {
		parameterIndex := index
		if signature.Variadic() && parameterIndex >= signature.Params().Len() {
			parameterIndex = signature.Params().Len() - 1
		}
		if parameterIndex < 0 || parameterIndex >= signature.Params().Len() {
			continue
		}
		parameter := signature.Params().At(parameterIndex)
		result[parameter] = appendUniqueFunctionTargets(
			result[parameter],
			resolver.functionTargets(caller, argument, current, 0),
		)
	}
	return result
}

func (resolver *valueFlowResolver) returnTypes(
	pkg *gopackages.Package,
	body *ast.BlockStmt,
	signature *types.Signature,
	bindings map[types.Object][]types.Type,
	resultIndex int,
	functionContext ...functionBindings,
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
		for _, candidate := range resolver.expressionTypes(
			pkg,
			expression,
			bindings,
			nestedResult,
			functionContext...,
		) {
			result = appendUniqueType(result, candidate)
		}
		return true
	})
	return result
}

func firstFunctionBindings(context []functionBindings) functionBindings {
	if len(context) == 0 {
		return nil
	}
	return context[0]
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
	functionBindings := resolver.callFunctionBindings(caller.pkg, call, signature, nil)
	if len(bindings) == 0 && len(functionBindings) == 0 {
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
	tracer.traceFunction(declaration, function, bindings, functionBindings)
	return tracer.byPackage
}

func (tracer *interfaceCallTracer) traceFunction(
	declaration symbolDeclaration,
	function *types.Func,
	bindings map[types.Object][]types.Type,
	functionBindings functionBindings,
) {
	key := function.FullName() +
		bindingCandidatesKey(bindings) +
		functionBindingCandidatesKey(functionBindings)
	if _, seen := tracer.visited[key]; seen {
		return
	}
	tracer.visited[key] = struct{}{}

	ast.Inspect(declaration.node, func(node ast.Node) bool {
		if composite, ok := node.(*ast.CompositeLit); ok {
			tracer.traceCompositeLiteral(declaration, composite, bindings, functionBindings)
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
					functionBindings,
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
								next := tracer.resolver.callBindings(
									declaration.pkg,
									call,
									signature,
									bindings,
									functionBindings,
								)
								nextFunctions := tracer.resolver.callFunctionBindings(
									declaration.pkg,
									call,
									signature,
									functionBindings,
								)
								tracer.active = append(tracer.active, concreteMethod.Pkg().Path())
								tracer.traceFunction(target, concreteMethod, next, nextFunctions)
								tracer.active = tracer.active[:len(tracer.active)-1]
							}
						}
					}
				}
			}
		}

		for _, functionTarget := range tracer.resolver.functionTargets(
			declaration.pkg,
			call.Fun,
			functionBindings,
			0,
		) {
			callee := functionTarget.function
			target, exists := tracer.functions[callee]
			if !exists {
				continue
			}
			signature, _ := callee.Type().(*types.Signature)
			next := tracer.resolver.callBindings(
				declaration.pkg,
				call,
				signature,
				bindings,
				functionBindings,
			)
			nextFunctions := tracer.resolver.callFunctionBindings(
				declaration.pkg,
				call,
				signature,
				functionBindings,
			)
			if len(next) > 0 || len(nextFunctions) > 0 {
				tracer.traceFunction(target, callee, next, nextFunctions)
			}
		}
		return true
	})
}

func (tracer *interfaceCallTracer) traceCompositeLiteral(
	declaration symbolDeclaration,
	composite *ast.CompositeLit,
	bindings map[types.Object][]types.Type,
	functionBindings functionBindings,
) {
	structType, _ := namedStruct(declaration.pkg.TypesInfo.TypeOf(composite))
	if structType == nil {
		return
	}
	for elementIndex, element := range composite.Elts {
		fieldIndex := elementIndex
		value := element
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
		for _, actualType := range tracer.resolver.expressionTypes(
			declaration.pkg,
			value,
			bindings,
			0,
			functionBindings,
		) {
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
	case *ast.StarExpr:
		return calledObject(info, typedExpression.X)
	case *ast.UnaryExpr:
		if typedExpression.Op == token.MUL || typedExpression.Op == token.ARROW {
			return calledObject(info, typedExpression.X)
		}
		return nil
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
	for required := range iface.Methods() {
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
		blankInitializerIndex := 0
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
						for index, name := range typedSpec.Names {
							valueNode := individualValueSpec(typedSpec, index)
							if name.Name == "_" {
								initializerIndex := blankInitializerIndex
								blankInitializerIndex++
								if declaration.Tok != token.VAR || !hasInitializationEffect(valueNode.Values) {
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
		blankInitPrefix := blankInitializerPrefix(pkg.PkgPath)
		for id := range symbols {
			if strings.HasPrefix(id, initPrefix) || strings.HasPrefix(id, blankInitPrefix) {
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
			case *ast.FuncLit:
				// Creating a function value does not execute its body. An
				// immediately invoked function literal is still effectful
				// because its enclosing CallExpr is visited first.
				return false
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
