package ovr

import (
	"errors"
)

var (
	// ErrPipelineEmpty means no pipeline nodes were provided.
	ErrPipelineEmpty = errors.New("pipeline is empty")
	// ErrFirstNodeNotFrom means the pipeline does not start with a From node.
	ErrFirstNodeNotFrom = errors.New("first node must be From")
	// ErrTerminalMissing means the pipeline has no Reply, Push, or Sink node.
	ErrTerminalMissing = errors.New("pipeline must include Reply, Push, or Sink")
	// ErrTerminalNotLast means a pipeline has nodes after its terminal before the next From.
	ErrTerminalNotLast = errors.New("terminal node must be last in its pipeline")
	// ErrMultipleTerminals means a pipeline has more than one terminal node.
	ErrMultipleTerminals = errors.New("pipeline must include exactly one terminal")
	// ErrIncompatibleTerminal means the terminal cannot be used with the trigger kind.
	ErrIncompatibleTerminal = errors.New("terminal is incompatible with trigger")
	// ErrPipeMissingModel means a Pipe node was declared without Model.
	ErrPipeMissingModel = errors.New("pipe must include Model")
	// ErrInvalidNode means a node was nil, unsupported, or otherwise invalid.
	ErrInvalidNode = errors.New("invalid node")
)

// Validate checks a pipeline declaration without starting any runtime work.
func Validate(nodes ...Node) error {
	return validatePipeline(nodes)
}

func validatePipeline(nodes []Node) error {
	_, err := compilePlans(nodes)
	return err
}
