package tools

import (
	"context"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type Handler interface {
	Execute(context.Context, provider.ToolCall) (provider.ToolResult, error)
}
