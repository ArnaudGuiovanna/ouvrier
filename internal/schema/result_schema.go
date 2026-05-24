package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

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
	tightenGeneratedSchema(generated, typ)
	raw, err := json.Marshal(generated)
	if err != nil {
		return nil, fmt.Errorf("marshal result schema for %s: %w", typ, err)
	}

	contract := &runtimecore.ResultSchema{
		Name:       typ.String(),
		Type:       typ,
		JSONSchema: raw,
	}
	if _, err := resolve(contract); err != nil {
		return nil, fmt.Errorf("resolve generated result schema for %s: %w", typ, err)
	}
	return contract, nil
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

func tightenGeneratedSchema(generated *jsonschema.Schema, typ reflect.Type) {
	if generated == nil || typ == nil {
		return
	}
	tightenSchemaForType(generated, typ)
}

func tightenSchemaForType(generated *jsonschema.Schema, typ reflect.Type) {
	if generated == nil || typ == nil {
		return
	}
	if typ.Kind() == reflect.Pointer {
		tightenNullableSchemaForType(generated, typ.Elem())
		return
	}
	tightenNonNullableSchemaForType(generated, typ)
}

func tightenNullableSchemaForType(generated *jsonschema.Schema, typ reflect.Type) {
	if generated == nil || typ == nil {
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		tightenStructProperties(generated, typ)
	case reflect.Slice, reflect.Array:
		tightenSchemaForType(generated.Items, typ.Elem())
	case reflect.Map:
		tightenSchemaForType(generated.AdditionalProperties, typ.Elem())
	}
}

func tightenNonNullableSchemaForType(generated *jsonschema.Schema, typ reflect.Type) {
	for typ.Kind() == reflect.Pointer {
		tightenNullableSchemaForType(generated, typ)
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		setSingleSchemaType(generated, "object")
		tightenStructProperties(generated, typ)
	case reflect.Slice, reflect.Array:
		setSingleSchemaType(generated, "array")
		tightenSchemaForType(generated.Items, typ.Elem())
	case reflect.Map:
		setSingleSchemaType(generated, "object")
		tightenSchemaForType(generated.AdditionalProperties, typ.Elem())
	}
}

func tightenStructProperties(generated *jsonschema.Schema, typ reflect.Type) {
	if generated == nil || generated.Properties == nil {
		return
	}
	for _, field := range reflect.VisibleFields(typ) {
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		name, ok := jsonFieldName(field)
		if !ok {
			continue
		}
		property, ok := generated.Properties[name]
		if !ok {
			continue
		}
		tightenSchemaForType(property, field.Type)
	}
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false
	}
	if name != "" {
		return name, true
	}
	return field.Name, true
}

func setSingleSchemaType(generated *jsonschema.Schema, typ string) {
	generated.Type = typ
	generated.Types = nil
}
