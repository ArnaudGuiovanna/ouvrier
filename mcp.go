package ovr

import (
	"fmt"
	"strings"
)

type mcpSpec struct {
	name string
}

type mcpPipeOption struct {
	spec mcpSpec
}

// MCP connects an external Model Context Protocol server to a Pipe.
func MCP(name string) PipeOption {
	return mcpPipeOption{
		spec: mcpSpec{name: strings.TrimSpace(name)},
	}
}

func (o mcpPipeOption) applyPipe(config *pipeConfig) {
	config.mcpServers = append(config.mcpServers, o.spec)
}

func (s mcpSpec) validateMCP() error {
	switch {
	case s.name == "":
		return fmt.Errorf("%w: MCP server name is required", ErrInvalidNode)
	case s.name == "." || s.name == "..":
		return fmt.Errorf("%w: MCP server name must not be a relative path", ErrInvalidNode)
	case strings.ContainsAny(s.name, `/\`):
		return fmt.Errorf("%w: MCP server name must not contain path separators", ErrInvalidNode)
	default:
		return nil
	}
}
