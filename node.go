package ovr

type nodeKind uint8

const (
	nodeKindInvalid nodeKind = iota
	nodeKindFrom
	nodeKindPipe
	nodeKindReply
	nodeKindPush
	nodeKindSink
)

// Node is a single step in an Ouvrier pipeline.
type Node interface {
	nodeKind() nodeKind
	validateNode() error
}

type invalidNode struct {
	err error
}

func (n invalidNode) nodeKind() nodeKind {
	return nodeKindInvalid
}

func (n invalidNode) validateNode() error {
	if n.err != nil {
		return n.err
	}
	return ErrInvalidNode
}
