package ovr

import (
	"errors"
	"fmt"
)

// ErrRunNotImplemented means the requested runtime transport is not implemented yet.
var ErrRunNotImplemented = errors.New("run runtime not implemented")

// Run validates a pipeline declaration before starting the runtime.
func Run(addr string, nodes ...Node) error {
	if err := validatePipeline(nodes); err != nil {
		return err
	}

	runtime, closeRuntime, err := defaultHTTPRuntimeForRun()
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}

	handler, err := newHTTPHandlerWithRuntime(nodes, runtime)
	if err != nil {
		_ = closeRuntime()
		return err
	}
	serveErr := serveHTTP(addr, handler)
	closeErr := closeRuntime()
	return errors.Join(serveErr, closeErr)
}
