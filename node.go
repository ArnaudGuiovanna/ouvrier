package ovr

type nodeKind uint8

const (
	nodeKindFrom nodeKind = iota
	nodeKindPipe
	nodeKindParallel
	nodeKindMap
	nodeKindReply
	nodeKindPush
	nodeKindSink
)

// Node is a single step in an Ouvrier pipeline.
type Node interface {
	nodeKind() nodeKind
	validateNode() error
}
