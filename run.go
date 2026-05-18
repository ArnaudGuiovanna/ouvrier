package ovr

import "errors"

// ErrRunNotImplemented means the requested runtime transport is not implemented yet.
var ErrRunNotImplemented = errors.New("run runtime not implemented")

// Run validates a pipeline declaration before starting the runtime.
func Run(addr string, nodes ...Node) error {
	if err := validatePipeline(nodes); err != nil {
		return err
	}

	handler, err := newHTTPHandler(nodes)
	if err != nil {
		return err
	}
	return serveHTTP(addr, handler)
}
