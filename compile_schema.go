package ovr

import (
	"fmt"
	"reflect"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/schema"
)

type resultSchemaCarrier interface {
	resultSchemaType() reflect.Type
}

func resultSchemaFromType(typ reflect.Type) (*runtimeplan.ResultSchema, error) {
	if typ == nil {
		return nil, nil
	}
	contract, err := schema.FromType(typ)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	return contract, nil
}
