package ovr

import (
	"reflect"

	runtimeplan "ouvrier/internal/runtime"
)

type resultSchemaCarrier interface {
	resultSchemaType() reflect.Type
}

func resultSchemaFromType(typ reflect.Type) *runtimeplan.ResultSchema {
	if typ == nil {
		return nil
	}
	return &runtimeplan.ResultSchema{
		Name: typ.String(),
		Type: typ,
	}
}
