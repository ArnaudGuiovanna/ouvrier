package ovr

import (
	"fmt"
	"reflect"
)

// ReplyFormat configures how Reply serializes the final outcome.
type ReplyFormat interface {
	validateReplyFormat() error
}

// JSONReply is a typed JSON reply format.
type JSONReply[T any] struct {
	schema reflect.Type
}

// JSON declares a typed JSON reply format.
func JSON[T any]() JSONReply[T] {
	return JSONReply[T]{schema: reflect.TypeFor[T]()}
}

func (f JSONReply[T]) validateReplyFormat() error {
	if f.schema == nil {
		return fmt.Errorf("%w: JSON reply schema is required", ErrInvalidNode)
	}
	return nil
}

func (f JSONReply[T]) resultSchemaType() reflect.Type {
	return f.schema
}

type replyNode struct {
	format ReplyFormat
}

// Reply terminates a synchronous pipeline by answering the caller.
func Reply(format ReplyFormat) Node {
	return replyNode{format: format}
}

func (n replyNode) nodeKind() nodeKind {
	return nodeKindReply
}

func (n replyNode) validateNode() error {
	if n.format == nil {
		return fmt.Errorf("%w: Reply format is required", ErrInvalidNode)
	}
	return n.format.validateReplyFormat()
}

// PushTarget configures where Push sends the final outcome.
type PushTarget interface {
	validatePushTarget() error
}

type pushNode struct {
	target PushTarget
}

// Push terminates an asynchronous pipeline by sending the outcome elsewhere.
func Push(target PushTarget) Node {
	return pushNode{target: target}
}

func (n pushNode) nodeKind() nodeKind {
	return nodeKindPush
}

func (n pushNode) validateNode() error {
	if n.target == nil {
		return fmt.Errorf("%w: Push target is required", ErrInvalidNode)
	}
	return n.target.validatePushTarget()
}

// SinkTarget configures where Sink records or discards the final outcome.
type SinkTarget interface {
	validateSinkTarget() error
}

// LogSink is a Sink target that writes the outcome to logs and metrics.
type LogSink struct{}

// Log declares a log sink target.
func Log() LogSink {
	return LogSink{}
}

func (LogSink) validateSinkTarget() error {
	return nil
}

type sinkNode struct {
	target SinkTarget
}

// Sink terminates a pipeline without replying to the trigger source.
func Sink(target SinkTarget) Node {
	return sinkNode{target: target}
}

func (n sinkNode) nodeKind() nodeKind {
	return nodeKindSink
}

func (n sinkNode) validateNode() error {
	if n.target == nil {
		return fmt.Errorf("%w: Sink target is required", ErrInvalidNode)
	}
	return n.target.validateSinkTarget()
}
