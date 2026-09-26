package impact

import (
	"go/ast"
	"go/token"
	"go/types"

	gopackages "golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// A value that becomes an interface can reach any method in its method set
// through dynamic dispatch, type assertions or reflection, wherever the
// interface value flows afterwards. Instead of tracking that flow, the
// declaration that converts the value (or instantiates a generic with its
// type) depends on the type's contract: its layout, every method in its
// method set and the contracts of the named types it contains.

// addTypeContractSymbols creates a layout and a contract symbol for every
// package-level named non-interface type.
func addTypeContractSymbols(
	objectIDs map[types.Object]string,
	reportPaths map[*types.Package]string,
	symbols map[string]Symbol,
) {
	for object := range objectIDs {
		typeName, ok := object.(*types.TypeName)
		if !ok || typeName.IsAlias() {
			continue
		}
		named, ok := typeName.Type().(*types.Named)
		if !ok {
			continue
		}
		if _, isInterface := named.Underlying().(*types.Interface); isInterface {
			continue
		}
		layout, _ := typeSymbolID("layout", typeName, objectIDs)
		contract, _ := typeSymbolID("contract", typeName, objectIDs)
		packagePath := reportPaths[typeName.Pkg()]
		symbols[layout] = Symbol{
			ID:          layout,
			PackagePath: packagePath,
			Hash:        stableMarkerHash(types.TypeString(named.Underlying(), packageQualifier)),
		}

		dependencies := map[string]struct{}{layout: {}}
		for selection := range types.NewMethodSet(types.NewPointer(named)).Methods() {
			if id, ok := objectIDs[declaredObject(selection.Obj())]; ok {
				dependencies[id] = struct{}{}
			}
		}
		addContractDependencies(named.Underlying(), objectIDs, dependencies)
		delete(dependencies, contract)
		symbols[contract] = Symbol{
			ID:           contract,
			PackagePath:  packagePath,
			Hash:         stableMarkerHash("contract"),
			Dependencies: sortedSet(dependencies),
		}
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

// addContractDependencies adds the contracts of the local named types that a
// value of typ holds directly or through composite types and type arguments.
func addContractDependencies(typ types.Type, objectIDs map[types.Object]string, dependencies map[string]struct{}) {
	switch typ := types.Unalias(typ).(type) {
	case *types.Named:
		if id, ok := typeSymbolID("contract", typ.Origin().Obj(), objectIDs); ok {
			dependencies[id] = struct{}{}
		}
		for argument := range typ.TypeArgs().Types() {
			addContractDependencies(argument, objectIDs, dependencies)
		}
	case *types.Pointer:
		addContractDependencies(typ.Elem(), objectIDs, dependencies)
	case *types.Slice:
		addContractDependencies(typ.Elem(), objectIDs, dependencies)
	case *types.Array:
		addContractDependencies(typ.Elem(), objectIDs, dependencies)
	case *types.Chan:
		addContractDependencies(typ.Elem(), objectIDs, dependencies)
	case *types.Map:
		addContractDependencies(typ.Key(), objectIDs, dependencies)
		addContractDependencies(typ.Elem(), objectIDs, dependencies)
	case *types.Struct:
		for field := range typ.Fields() {
			addContractDependencies(field.Type(), objectIDs, dependencies)
		}
	}
}

// addConversionDependencies builds SSA for the local packages and makes every
// declaration that converts a concrete value to an interface depend on the
// contract of the converted type. SSA makes every implicit conversion explicit
// as a MakeInterface instruction.
func addConversionDependencies(
	localPackages []*gopackages.Package,
	declarations []symbolDeclaration,
	objectIDs map[types.Object]string,
	symbols map[string]Symbol,
) {
	if len(localPackages) == 0 {
		return
	}
	program := ssa.NewProgram(localPackages[0].Fset, 0)
	created := make(map[*types.Package]bool, len(localPackages))
	for _, pkg := range localPackages {
		created[pkg.Types] = true
	}
	// Dependencies are loaded from export data, so their SSA packages carry
	// types only; function bodies outside the module remain black boxes.
	var createImports func([]*types.Package)
	createImports = func(imports []*types.Package) {
		for _, imported := range imports {
			if !created[imported] {
				created[imported] = true
				program.CreatePackage(imported, nil, nil, true)
				createImports(imported.Imports())
			}
		}
	}
	for _, pkg := range localPackages {
		createImports(pkg.Types.Imports())
	}
	ssaPackages := make([]*ssa.Package, len(localPackages))
	for index, pkg := range localPackages {
		ssaPackages[index] = program.CreatePackage(pkg.Types, pkg.Syntax, pkg.TypesInfo, true)
	}
	program.Build()

	functionIDs := make(map[*ast.FuncDecl]string)
	initializers := make(map[*types.Package][]initializerRange)
	for _, declaration := range declarations {
		switch node := declaration.node.(type) {
		case *ast.FuncDecl:
			functionIDs[node] = declaration.id
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

	added := make(map[string]map[string]struct{})
	addConversion := func(ids []string, conversion *ssa.MakeInterface) {
		for _, id := range ids {
			if added[id] == nil {
				added[id] = make(map[string]struct{})
			}
			addContractDependencies(conversion.X.Type(), objectIDs, added[id])
		}
	}
	for index, pkg := range localPackages {
		for _, file := range pkg.Syntax {
			for _, rawDeclaration := range file.Decls {
				function, ok := rawDeclaration.(*ast.FuncDecl)
				if !ok {
					continue
				}
				object, _ := pkg.TypesInfo.Defs[function.Name].(*types.Func)
				id, known := functionIDs[function]
				if object == nil || !known {
					continue
				}
				forEachConversion(program.FuncValue(object), func(conversion *ssa.MakeInterface) {
					addConversion([]string{id}, conversion)
				})
			}
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
	}

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
