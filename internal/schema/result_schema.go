package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"

	runtimecore "ouvrier/internal/runtime"
)

func FromType(typ reflect.Type) (*runtimecore.ResultSchema, error) {
	if typ == nil {
		return nil, errors.New("result schema type is required")
	}

	generated, err := jsonschema.ForType(typ, nil)
	if err != nil {
		return nil, fmt.Errorf("generate result schema for %s: %w", typ, err)
	}
	raw, err := json.Marshal(generated)
	if err != nil {
		return nil, fmt.Errorf("marshal result schema for %s: %w", typ, err)
	}

	return &runtimecore.ResultSchema{
		Name:       typ.String(),
		Type:       typ,
		JSONSchema: raw,
	}, nil
}

func ValidateJSON(contract *runtimecore.ResultSchema, data []byte) error {
	if contract == nil {
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("validate result schema %s: output is empty", contract.Name)
	}

	resolved, err := resolve(contract)
	if err != nil {
		return err
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("validate result schema %s: decode output JSON: %w", contract.Name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("validate result schema %s: output must contain a single JSON value", contract.Name)
	}

	if err := resolved.Validate(value); err != nil {
		return fmt.Errorf("validate result schema %s: %w", contract.Name, err)
	}
	return nil
}

func resolve(contract *runtimecore.ResultSchema) (*jsonschema.Resolved, error) {
	if len(contract.JSONSchema) == 0 {
		return nil, fmt.Errorf("validate result schema %s: schema JSON is empty", contract.Name)
	}

	var parsed jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &parsed); err != nil {
		return nil, fmt.Errorf("validate result schema %s: decode schema JSON: %w", contract.Name, err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("validate result schema %s: resolve schema: %w", contract.Name, err)
	}
	return resolved, nil
}
