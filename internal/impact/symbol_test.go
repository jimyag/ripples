package impact

import (
	"go/token"
	"go/types"
	"testing"
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
