package ovr

import (
	"fmt"
	"reflect"
)

type outputSpec struct {
	typ reflect.Type
}

type outputOption struct {
	spec outputSpec
}

// Output declares the typed result schema expected from a Pipe.
func Output[T any]() PipeOption {
	return outputOption{spec: outputSpec{typ: reflect.TypeFor[T]()}}
}

func (o outputOption) applyPipe(config *pipeConfig) {
	if o.spec.typ == nil {
		config.setErr(fmt.Errorf("%w: Pipe output schema is required", ErrInvalidNode))
		return
	}
	if config.output != nil {
		config.setErr(fmt.Errorf("%w: Pipe output declared more than once", ErrInvalidNode))
		return
	}
	config.output = &o.spec
}
