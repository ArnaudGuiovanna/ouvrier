package ovr

import "errors"

// ErrRunNotImplemented means the runtime server loop is not implemented yet.
var ErrRunNotImplemented = errors.New("run runtime not implemented")

// Run validates a pipeline declaration before starting the runtime.
func Run(addr string, nodes ...Node) error {
	if err := validatePipeline(nodes); err != nil {
		return err
	}
	return ErrRunNotImplemented
}
