package ovr

import "errors"

// ErrRunNotImplemented means the requested runtime transport is not implemented yet.
var ErrRunNotImplemented = errors.New("run runtime not implemented")

// Run validates a pipeline declaration before starting the default runtime.
func Run(addr string, nodes ...Node) error {
	return NewRunner().Run(addr, nodes...)
}
