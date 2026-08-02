package operate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/jsonschema-go/jsonschema"
)

// decodeModelToolArguments decodes exactly one JSON object and validates it
// against the same schema advertised to the model. Model-generated arguments
// never reach the governed executor unless this succeeds.
func decodeModelToolArguments(name string, raw json.RawMessage) (map[string]any, error) {
	schemaJSON := toolSchema(name)
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}

	var parsed jsonschema.Schema
	if err := json.Unmarshal(schemaJSON, &parsed); err != nil {
		return nil, fmt.Errorf("decode schema JSON: %w", err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve schema: %w", err)
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("arguments must contain a single JSON value")
	}
	if err := resolved.Validate(value); err != nil {
		return nil, fmt.Errorf("validate against exposed schema: %w", err)
	}

	input, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return input, nil
}
