package impact

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
)

func TestContainsFunctionValueLimitsStructTraversal(t *testing.T) {
	signature := types.NewSignatureType(
		nil,
		nil,
		nil,
		types.NewTuple(),
		types.NewTuple(),
		false,
	)
	direct := types.NewStruct(
		[]*types.Var{types.NewField(token.NoPos, nil, "Factory", signature, false)},
		nil,
	)
	nested := types.NewStruct(
		[]*types.Var{types.NewField(token.NoPos, nil, "Direct", direct, false)},
		nil,
	)

	if !containsFunctionValue(direct) {
		t.Fatal("direct function field was not detected")
	}
	if !containsFunctionValue(types.NewSlice(direct)) {
		t.Fatal("container of structs with a direct function field was not detected")
	}
	if containsFunctionValue(nested) {
		t.Fatal("nested struct function field should be resolved through its own storage location")
	}
}

func TestPackageUsedObjectsIndexesEachObjectOnce(t *testing.T) {
	first := types.NewVar(token.NoPos, nil, "first", types.Typ[types.Int])
	second := types.NewVar(token.NoPos, nil, "second", types.Typ[types.Int])
	pkg := &gopackages.Package{
		TypesInfo: &types.Info{
			Uses: map[*ast.Ident]types.Object{
				{Name: "first"}:      first,
				{Name: "firstAgain"}: first,
				{Name: "second"}:     second,
			},
		},
	}

	used := packageUsedObjects([]*gopackages.Package{pkg})[pkg]
	if len(used) != 2 {
		t.Fatalf("packageUsedObjects() size = %d, want 2", len(used))
	}
	if !objectIsUsed(used, first) || !objectIsUsed(used, second) {
		t.Fatal("packageUsedObjects() omitted a used object")
	}
	if objectIsUsed(used, types.NewVar(token.NoPos, nil, "unused", types.Typ[types.Int])) {
		t.Fatal("packageUsedObjects() included an unused object")
	}
}
