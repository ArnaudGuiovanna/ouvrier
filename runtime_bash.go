package ovr

import (
	"encoding/json"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	internalsandbox "github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

func registerRuntimeBash(executor *tools.Executor, bashes []runtimeplan.BashTool) ([]provider.ToolSpec, error) {
	specs := make([]provider.ToolSpec, 0, len(bashes))
	for _, bash := range bashes {
		name := strings.TrimSpace(bash.Name)
		if name == "" {
			name = defaultBashToolName
		}
		sandbox, err := internalsandbox.New(bash.SandboxRoot,
			internalsandbox.WithEnvironment(currentEnvironment()),
			internalsandbox.WithAllowedEnv(bash.AllowedEnv...),
		)
		if err != nil {
			return nil, err
		}
		handler, err := tools.NewBashHandler(sandbox, tools.BashHandlerConfig{
			MaxOutputBytes:     bash.MaxOutputBytes,
			AllowHostExecution: bash.UnsafeHostExecution,
		})
		if err != nil {
			return nil, err
		}
		inputSchema := bashInputSchema()
		if err := executor.RegisterHandler(name, handler, tools.WithMetadata(tools.Metadata{
			Kind:         tools.ToolKindBash,
			Target:       sandbox.Root(),
			Effect:       policy.EffectSideEffecting,
			SideEffects:  []string{"process", "filesystem"},
			InputSchema:  inputSchema,
			ArgumentName: "",
			Timeout:      bash.Timeout,
		})); err != nil {
			return nil, err
		}
		specs = append(specs, provider.ToolSpec{
			Name:        name,
			Description: "Run a bash command inside the configured sandbox workspace.",
			InputSchema: inputSchema,
		})
	}
	return specs, nil
}

func bashInputSchema() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Bash command to run in the sandbox workspace.",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Optional working directory relative to the sandbox workspace.",
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	})
}
