package impact

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"slices"

	gopackages "golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// A value that becomes an interface can reach methods in its method set
// through dynamic dispatch, type assertions or reflection, wherever the
// interface value flows afterwards. Instead of tracking that flow, the
// declaration that converts the value (or instantiates a generic with its
// type) depends on the type's contract. There are two contracts per type:
//
//   - contract: layout, every method and the contracts of nested named types.
//     Generic instantiations and conversions to an empty interface use it,
//     because generic code, fmt, encoding/json and templates may reach any
//     method or field. So do conversions to error: libraries inspect errors
//     through anonymous interfaces (Cause, GRPCStatus, ...) that export data
//     does not show.
//   - dispatch: layout, the methods some code can invoke dynamically and the
//     dispatch contracts of nested named types. Other conversions use it, so
//     adding or changing a method that nothing calls through an interface
//     does not reach every converter.

// stdlibDynamicChecks lists methods the standard library invokes through
// unexported or anonymous interfaces on values that need not be errors.
var stdlibDynamicChecks = map[string]bool{
	"Unwrap": true, "Is": true, "As": true, // errors, http.ResponseController
	"Timeout": true, "Temporary": true, // net
}

var errorInterface = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// conversionContract returns the kind of contract a conversion to target
// depends on.
func conversionContract(target types.Type) string {
	iface, _ := target.Underlying().(*types.Interface)
	if iface == nil || iface.Empty() || types.Implements(iface, errorInterface) {
		return "contract"
	}
	return "dispatch"
}

// dynamicCall is an interface through which code can invoke a method. A call
// through an interface only reaches types implementing it. A dynamicCall
// without an interface matches every type by name: its interface mentions
// type parameters, which only match concrete types after instantiation.
type dynamicCall struct {
	iface     *types.Interface
	signature *types.Signature
	// caller is the declaration making the call; empty when the call may
	// happen anywhere, such as inside a dependency or on a value found by a
	// type assertion.
	caller string
}

// dynamicMethods holds, by method ID, the interfaces through which code can
// invoke the method.
type dynamicMethods map[string][]dynamicCall

// collectDynamicMethods gathers the interface methods local declarations
// call or select, the methods of interfaces local code asserts to, and the
// methods of interfaces declared by dependencies, whose bodies may call them.
func collectDynamicMethods(localPackages []*gopackages.Package, declarations []symbolDeclaration) dynamicMethods {
	result := make(dynamicMethods)
	type key struct {
		iface  *types.Interface
		id     string
		caller string
	}
	seen := make(map[key]bool)
	addMethod := func(iface *types.Interface, method *types.Func, caller string) {
		if seen[key{iface, method.Id(), caller}] {
			return
		}
		seen[key{iface, method.Id(), caller}] = true
		call := dynamicCall{iface: iface, signature: method.Signature(), caller: caller}
		if mentionsTypeParam(iface) {
			call = dynamicCall{caller: caller}
		}
		result[method.Id()] = append(result[method.Id()], call)
	}
	addInterface := func(typ types.Type) {
		if typ == nil {
			return
		}
		if iface, ok := typ.Underlying().(*types.Interface); ok {
			for method := range iface.Methods() {
				addMethod(iface, method, "")
			}
		}
	}
	local := make(map[*types.Package]bool, len(localPackages))
	for _, pkg := range localPackages {
		local[pkg.Types] = true
	}
	var dependencies []*types.Package
	seenPackages := make(map[*types.Package]bool)
	var visit func([]*types.Package)
	visit = func(imports []*types.Package) {
		for _, imported := range imports {
			if !local[imported] && !seenPackages[imported] {
				seenPackages[imported] = true
				dependencies = append(dependencies, imported)
				visit(imported.Imports())
			}
		}
	}
	for _, pkg := range localPackages {
		visit(pkg.Types.Imports())
	}

	// Walking the syntax and the dependency scopes runs concurrently, each
	// worker filling its own slot; the results are merged afterwards.
	type interfaceUse struct {
		typ    types.Type
		method *types.Func // nil for every method of typ
		caller string
	}
	uses := make([][]interfaceUse, len(declarations)+len(dependencies))
	_ = parallelFor(len(uses), func(index int) error {
		if index >= len(declarations) {
			scope := dependencies[index-len(declarations)].Scope()
			for _, name := range scope.Names() {
				if typeName, ok := scope.Lookup(name).(*types.TypeName); ok {
					uses[index] = append(uses[index], interfaceUse{typ: typeName.Type()})
				}
			}
			return nil
		}
		declaration := declarations[index]
		if declaration.node == nil {
			return nil
		}
		info := declaration.pkg.TypesInfo
		ast.Inspect(declaration.node, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.Ident:
				if method, ok := info.Uses[node].(*types.Func); ok {
					if recv := method.Signature().Recv(); recv != nil {
						uses[index] = append(uses[index], interfaceUse{typ: recv.Type(), method: method, caller: declaration.id})
					}
				}
			case *ast.TypeAssertExpr:
				if node.Type != nil {
					uses[index] = append(uses[index], interfaceUse{typ: info.TypeOf(node.Type)})
				}
			case *ast.TypeSwitchStmt:
				for _, clause := range node.Body.List {
					for _, expression := range clause.(*ast.CaseClause).List {
						uses[index] = append(uses[index], interfaceUse{typ: info.TypeOf(expression)})
					}
				}
			}
			return true
		})
		return nil
	})
	for _, itemUses := range uses {
		for _, use := range itemUses {
			if use.method == nil {
				addInterface(use.typ)
			} else if iface, ok := use.typ.Underlying().(*types.Interface); ok {
				addMethod(iface, use.method, use.caller)
			}
		}
	}
	addInterface(errorInterface)
	return result
}

// reach reports whether an interface call anywhere could invoke method on a
// value of type *named; otherwise it returns the local interface methods
// whose calls could.
func (methods dynamicMethods) reach(named *types.Named, method *types.Func) (anywhere bool, callers []string) {
	if stdlibDynamicChecks[method.Name()] {
		return true, nil
	}
	// Implements is unspecified for uninstantiated generic types, so their
	// methods match by name.
	generic := named.TypeParams().Len() > 0
	receiver := types.NewPointer(named)
	for _, call := range methods[method.Id()] {
		if call.iface != nil && !generic &&
			(!types.Identical(call.signature, method.Signature()) || !types.Implements(receiver, call.iface)) {
			continue
		}
		if call.caller == "" {
			return true, nil
		}
		callers = append(callers, call.caller)
	}
	return false, callers
}

// mentionsTypeParam reports whether typ refers to a type parameter. A
// signature's receiver does not count.
func mentionsTypeParam(typ types.Type) bool {
	switch typ := types.Unalias(typ).(type) {
	case *types.TypeParam:
		return true
	case *types.Named:
		for argument := range typ.TypeArgs().Types() {
			if mentionsTypeParam(argument) {
				return true
			}
		}
	case *types.Pointer:
		return mentionsTypeParam(typ.Elem())
	case *types.Slice:
		return mentionsTypeParam(typ.Elem())
	case *types.Array:
		return mentionsTypeParam(typ.Elem())
	case *types.Chan:
		return mentionsTypeParam(typ.Elem())
	case *types.Map:
		return mentionsTypeParam(typ.Key()) || mentionsTypeParam(typ.Elem())
	case *types.Signature:
		for variable := range typ.Params().Variables() {
			if mentionsTypeParam(variable.Type()) {
				return true
			}
		}
		for variable := range typ.Results().Variables() {
			if mentionsTypeParam(variable.Type()) {
				return true
			}
		}
	case *types.Struct:
		for field := range typ.Fields() {
			if mentionsTypeParam(field.Type()) {
				return true
			}
		}
	case *types.Interface:
		for method := range typ.Methods() {
			if mentionsTypeParam(method.Type()) {
				return true
			}
		}
	}
	return false
}

// addTypeContractSymbols creates layout, contract and dispatch symbols for
// every package-level named non-interface type.
func addTypeContractSymbols(
	objectIDs map[types.Object]string,
	reportPaths map[*types.Package]string,
	reachable dynamicMethods,
	symbols map[string]Symbol,
) {
	var named []*types.TypeName
	for object := range objectIDs {
		typeName, ok := object.(*types.TypeName)
		if !ok || typeName.IsAlias() {
			continue
		}
		if _, ok := typeName.Type().(*types.Named); !ok || types.IsInterface(typeName.Type()) {
			continue
		}
		named = append(named, typeName)
	}
	// Types are independent: each worker builds the symbols of one type from
	// read-only inputs, and the symbols are inserted afterwards.
	built := make([][3]Symbol, len(named))
	_ = parallelFor(len(named), func(index int) error {
		built[index] = typeContractSymbols(named[index], objectIDs, reportPaths, reachable)
		return nil
	})
	// Test variants declare their own copies of a package's types, and a copy
	// implements only the interfaces declared against the same copy, so the
	// copies can receive different dynamic calls. Their symbols are merged.
	for _, typeSymbols := range built {
		for _, symbol := range typeSymbols {
			if existing, ok := symbols[symbol.ID]; ok {
				symbol = mergeContractSymbols(existing, symbol)
			}
			symbols[symbol.ID] = symbol
		}
	}
}

// mergeContractSymbols combines the symbols two copies of a type produce: the
// dependencies of either and, for each method only interface calls in local
// declarations reach, the callers of either.
func mergeContractSymbols(existing, added Symbol) Symbol {
	existing.Dependencies = mergeDependencies(existing.Dependencies, added.Dependencies)
	callers := make(map[string][]string)
	for _, dynamic := range slices.Concat(existing.Dynamic, added.Dynamic) {
		callers[dynamic.ID] = append(callers[dynamic.ID], dynamic.Callers...)
	}
	existing.Dynamic = nil
	for _, id := range slices.Sorted(maps.Keys(callers)) {
		if _, unconditional := slices.BinarySearch(existing.Dependencies, id); unconditional {
			continue
		}
		slices.Sort(callers[id])
		existing.Dynamic = append(existing.Dynamic, DynamicDependency{ID: id, Callers: slices.Compact(callers[id])})
	}
	return existing
}

// typeContractSymbols returns the layout, contract and dispatch symbols of a
// named non-interface type.
func typeContractSymbols(
	typeName *types.TypeName,
	objectIDs map[types.Object]string,
	reportPaths map[*types.Package]string,
	reachable dynamicMethods,
) [3]Symbol {
	named := typeName.Type().(*types.Named)
	layout, _ := typeSymbolID("layout", typeName, objectIDs)
	packagePath := reportPaths[typeName.Pkg()]

	full := map[string]struct{}{layout: {}}
	dispatch := map[string]struct{}{layout: {}}
	var dynamic []DynamicDependency
	for selection := range types.NewMethodSet(types.NewPointer(named)).Methods() {
		method := selection.Obj().(*types.Func)
		id, ok := objectIDs[declaredObject(method)]
		if !ok {
			continue
		}
		full[id] = struct{}{}
		anywhere, callers := reachable.reach(named, method)
		if anywhere {
			dispatch[id] = struct{}{}
		} else if len(callers) > 0 {
			slices.Sort(callers)
			dynamic = append(dynamic, DynamicDependency{ID: id, Callers: slices.Compact(callers)})
		}
	}
	addContractDependencies(named.Underlying(), "contract", objectIDs, full)
	addContractDependencies(named.Underlying(), "dispatch", objectIDs, dispatch)
	contract, _ := typeSymbolID("contract", typeName, objectIDs)
	delete(full, contract)
	dispatchID, _ := typeSymbolID("dispatch", typeName, objectIDs)
	delete(dispatch, dispatchID)
	return [3]Symbol{
		{
			ID:          layout,
			PackagePath: packagePath,
			Hash:        stableMarkerHash(types.TypeString(named.Underlying(), packageQualifier)),
		},
		{
			ID:           contract,
			PackagePath:  packagePath,
			Hash:         stableMarkerHash("contract"),
			Dependencies: sortedSet(full),
		},
		{
			ID:           dispatchID,
			PackagePath:  packagePath,
			Hash:         stableMarkerHash("dispatch"),
			Dependencies: sortedSet(dispatch),
			Dynamic:      dynamic,
		},
	}
}

// typeSymbolID returns the ID of a synthetic symbol attached to a local
// package-level type.
func typeSymbolID(kind string, typeName *types.TypeName, objectIDs map[types.Object]string) (string, bool) {
	if _, local := objectIDs[typeName]; !local {
		return "", false
	}
	return packageObjectID(typeName.Pkg().Path(), kind, typeName.Name()), true
}

// addContractDependencies adds the contracts of the given kind for the local
// named types that a value of typ holds directly or through composite types
// and type arguments.
func addContractDependencies(typ types.Type, kind string, objectIDs map[types.Object]string, dependencies map[string]struct{}) {
	switch typ := types.Unalias(typ).(type) {
	case *types.Named:
		if id, ok := typeSymbolID(kind, typ.Origin().Obj(), objectIDs); ok {
			dependencies[id] = struct{}{}
		}
		for argument := range typ.TypeArgs().Types() {
			addContractDependencies(argument, kind, objectIDs, dependencies)
		}
	case *types.Pointer:
		addContractDependencies(typ.Elem(), kind, objectIDs, dependencies)
	case *types.Slice:
		addContractDependencies(typ.Elem(), kind, objectIDs, dependencies)
	case *types.Array:
		addContractDependencies(typ.Elem(), kind, objectIDs, dependencies)
	case *types.Chan:
		addContractDependencies(typ.Elem(), kind, objectIDs, dependencies)
	case *types.Map:
		addContractDependencies(typ.Key(), kind, objectIDs, dependencies)
		addContractDependencies(typ.Elem(), kind, objectIDs, dependencies)
	case *types.Struct:
		for field := range typ.Fields() {
			addContractDependencies(field.Type(), kind, objectIDs, dependencies)
		}
	}
}

// conversionDependencies builds SSA for the local packages and returns, per
// package, the contracts or dispatch contracts of the types each declaration
// converts to an interface. SSA makes every implicit conversion explicit as a
// MakeInterface instruction. It only reads its inputs, so it runs while the
// declarations are hashed.
func conversionDependencies(
	localPackages []*gopackages.Package,
	declarations []symbolDeclaration,
	objectIDs map[types.Object]string,
) []map[string]map[string]struct{} {
	if len(localPackages) == 0 {
		return nil
	}
	type declaredFunction struct {
		object *types.Func
		id     string
	}
	functions := make(map[*types.Package][]declaredFunction)
	initializers := make(map[*types.Package][]initializerRange)
	for _, declaration := range declarations {
		switch node := declaration.node.(type) {
		case *ast.FuncDecl:
			if object, ok := declaration.pkg.TypesInfo.Defs[node.Name].(*types.Func); ok {
				functions[declaration.pkg.Types] = append(functions[declaration.pkg.Types], declaredFunction{object, declaration.id})
			}
		case *ast.ValueSpec:
			if len(node.Values) > 0 {
				initializers[declaration.pkg.Types] = append(initializers[declaration.pkg.Types], initializerRange{
					start: node.Values[0].Pos(),
					end:   node.Values[len(node.Values)-1].End(),
					id:    declaration.id,
				})
			}
		}
	}

	program := ssa.NewProgram(localPackages[0].Fset, 0)
	created := make(map[*types.Package]bool, len(localPackages))
	for _, pkg := range localPackages {
		created[pkg.Types] = true
	}
	// Dependencies are loaded from export data, so their SSA packages carry
	// types only; function bodies outside the module remain black boxes. SSA
	// needs them for direct imports only and creates the methods of indirect
	// dependencies on demand.
	for _, pkg := range localPackages {
		for _, imported := range pkg.Types.Imports() {
			if !created[imported] {
				created[imported] = true
				program.CreatePackage(imported, nil, nil, true)
			}
		}
	}
	ssaPackages := make([]*ssa.Package, len(localPackages))
	for index, pkg := range localPackages {
		ssaPackages[index] = program.CreatePackage(pkg.Types, pkg.Syntax, pkg.TypesInfo, true)
	}

	// Packages are built, scanned and released one at a time per worker, so
	// only the SSA bodies of the packages in flight are in memory together.
	results := make([]map[string]map[string]struct{}, len(localPackages))
	_ = parallelFor(len(localPackages), func(index int) error {
		pkg := localPackages[index]
		ssaPackages[index].Build()
		added := make(map[string]map[string]struct{})
		addConversion := func(ids []string, conversion *ssa.MakeInterface) {
			kind := conversionContract(conversion.Type())
			for _, id := range ids {
				if added[id] == nil {
					added[id] = make(map[string]struct{})
				}
				addContractDependencies(conversion.X.Type(), kind, objectIDs, added[id])
			}
		}
		for _, function := range functions[pkg.Types] {
			ssaFunction := program.FuncValue(function.object)
			forEachConversion(ssaFunction, func(conversion *ssa.MakeInterface) {
				addConversion([]string{function.id}, conversion)
			})
			releaseBody(ssaFunction)
		}

		// Package-level variable initializers are compiled into the synthetic
		// package initializer; attribute each conversion to its variable and
		// fall back to package initialization when no variable matches.
		ranges := initializers[pkg.Types]
		fallback := []string{packageInitID(pkg.ID)}
		initializer := ssaPackages[index].Func("init")
		forEachInstruction(initializer, func(instruction ssa.Instruction) {
			conversion, ok := instruction.(*ssa.MakeInterface)
			if !ok {
				return
			}
			ids := initializerTargets(conversion, objectIDs, ranges)
			if len(ids) == 0 {
				ids = fallback
			}
			addConversion(ids, conversion)
		})
		for _, anonymous := range initializer.AnonFuncs {
			ids := lookupInitializers(ranges, anonymous.Pos())
			if len(ids) == 0 {
				ids = fallback
			}
			forEachConversion(anonymous, func(conversion *ssa.MakeInterface) {
				addConversion(ids, conversion)
			})
		}
		releaseBody(initializer)
		results[index] = added
		return nil
	})
	return results
}

// addConversionDependencies adds the dependencies from conversionDependencies
// to the converting declarations.
func addConversionDependencies(results []map[string]map[string]struct{}, symbols map[string]Symbol) {
	for _, added := range results {
		for id, dependencies := range added {
			symbol, ok := symbols[id]
			if !ok {
				continue
			}
			delete(dependencies, id)
			symbol.Dependencies = mergeDependencies(symbol.Dependencies, sortedSet(dependencies))
			symbols[id] = symbol
		}
	}
}

// releaseBody drops the SSA body of a scanned function and its closures.
// Building other packages never reads it: calls, wrappers and thunks refer to
// the function value, not its instructions.
func releaseBody(function *ssa.Function) {
	if function == nil {
		return
	}
	for _, anonymous := range function.AnonFuncs {
		releaseBody(anonymous)
	}
	function.Blocks = nil
}

type initializerRange struct {
	start, end token.Pos
	id         string
}

func lookupInitializers(ranges []initializerRange, pos token.Pos) []string {
	var ids []string
	for _, current := range ranges {
		if pos.IsValid() && current.start <= pos && pos < current.end {
			ids = append(ids, current.id)
		}
	}
	return ids
}

// initializerTargets returns the package variables whose initializer performs
// a conversion. SSA gives implicit conversions no position, so they are
// located through the instruction that consumes the converted value.
func initializerTargets(
	conversion *ssa.MakeInterface,
	objectIDs map[types.Object]string,
	ranges []initializerRange,
) []string {
	for _, referrer := range *conversion.Referrers() {
		if store, ok := referrer.(*ssa.Store); ok {
			if global, ok := store.Addr.(*ssa.Global); ok {
				if id, ok := objectIDs[global.Object()]; ok {
					return []string{id}
				}
			}
		}
		if ids := lookupInitializers(ranges, instructionPos(referrer)); len(ids) > 0 {
			return ids
		}
	}
	if instruction, ok := conversion.X.(ssa.Instruction); ok {
		return lookupInitializers(ranges, instructionPos(instruction))
	}
	return nil
}

// instructionPos returns the position of an instruction or, for stores into
// variadic argument slots that SSA leaves unpositioned, of the slot's array.
func instructionPos(instruction ssa.Instruction) token.Pos {
	if pos := instruction.Pos(); pos.IsValid() {
		return pos
	}
	switch instruction := instruction.(type) {
	case *ssa.Store:
		if address, ok := instruction.Addr.(ssa.Instruction); ok {
			return instructionPos(address)
		}
	case *ssa.IndexAddr:
		if array, ok := instruction.X.(ssa.Instruction); ok {
			return array.Pos()
		}
	}
	return token.NoPos
}

func forEachConversion(function *ssa.Function, visit func(*ssa.MakeInterface)) {
	forEachInstruction(function, func(instruction ssa.Instruction) {
		if conversion, ok := instruction.(*ssa.MakeInterface); ok {
			visit(conversion)
		}
	})
	if function == nil {
		return
	}
	for _, anonymous := range function.AnonFuncs {
		forEachConversion(anonymous, visit)
	}
}

func forEachInstruction(function *ssa.Function, visit func(ssa.Instruction)) {
	if function == nil {
		return
	}
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			visit(instruction)
		}
	}
}
