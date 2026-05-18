package tools

import (
	"context"

	"ouvrier/internal/provider"
)

type Handler interface {
	Execute(context.Context, provider.ToolCall) (provider.ToolResult, error)
}
