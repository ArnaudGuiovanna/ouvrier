package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func FromType(typ reflect.Type) (*runtimecore.ResultSchema, error) {
	if typ == nil {
		return nil, errors.New("result schema type is required")
	}

	generated, err := jsonschema.ForType(typ, resultSchemaForOptions())
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

func resultSchemaForOptions() *jsonschema.ForOptions {
	return &jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[json.Number]():     {Type: "number"},
			reflect.TypeFor[json.RawMessage](): {Types: []string{"null", "boolean", "number", "string", "array", "object"}},
			reflect.TypeFor[net.IP]():          {Type: "string"},
			reflect.TypeFor[netip.Addr]():      {Type: "string"},
		},
	}
}

func ValidateJSON(contract *runtimecore.ResultSchema, data []byte) error {
	_, err := NormalizeJSON(contract, data)
	return err
}

func NormalizeJSON(contract *runtimecore.ResultSchema, data []byte) ([]byte, error) {
	if contract == nil {
		return bytes.TrimSpace(data), nil
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("validate result schema %s: output is empty", contract.Name)
	}

	resolved, err := resolve(contract)
	if err != nil {
		return nil, err
	}

	data = resultJSONCandidate(data)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("validate result schema %s: decode output JSON: %w", contract.Name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("validate result schema %s: output must contain a single JSON value", contract.Name)
	}

	if err := resolved.Validate(value); err != nil {
		return nil, fmt.Errorf("validate result schema %s: %w", contract.Name, err)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("validate result schema %s: normalize output JSON: %w", contract.Name, err)
	}
	return normalized, nil
}

func resultJSONCandidate(data []byte) []byte {
	if isSingleJSONValue(data) {
		return data
	}
	if fenced, ok := markdownFenceBody(data); ok && isSingleJSONValue(fenced) {
		return fenced
	}
	if extracted, ok := firstBalancedJSONValue(data); ok {
		return extracted
	}
	return data
}

func isSingleJSONValue(data []byte) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func markdownFenceBody(data []byte) ([]byte, bool) {
	if !bytes.HasPrefix(data, []byte("```")) {
		return nil, false
	}
	firstNewline := bytes.IndexByte(data, '\n')
	if firstNewline < 0 {
		return nil, false
	}
	body := bytes.TrimSpace(data[firstNewline+1:])
	if !bytes.HasSuffix(body, []byte("```")) {
		return nil, false
	}
	return bytes.TrimSpace(body[:len(body)-3]), true
}

func firstBalancedJSONValue(data []byte) ([]byte, bool) {
	for start, b := range data {
		if b != '{' && b != '[' {
			continue
		}
		end, ok := balancedJSONEnd(data, start)
		if !ok {
			continue
		}
		candidate := bytes.TrimSpace(data[start:end])
		if isSingleJSONValue(candidate) {
			return candidate, true
		}
	}
	return nil, false
}

func balancedJSONEnd(data []byte, start int) (int, bool) {
	stack := []byte{jsonCloser(data[start])}
	inString := false
	escaped := false
	for i := start + 1; i < len(data); i++ {
		b := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, jsonCloser(b))
		case '}', ']':
			last := len(stack) - 1
			if last < 0 || stack[last] != b {
				return 0, false
			}
			stack = stack[:last]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func jsonCloser(open byte) byte {
	if open == '{' {
		return '}'
	}
	return ']'
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
	if typ == reflect.TypeFor[json.RawMessage]() || typ == reflect.TypeFor[net.IP]() {
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
		if schemaHasConcreteNonObjectType(generated) {
			return
		}
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
	if !field.IsExported() {
		return "", false
	}
	name := field.Name
	if tag, ok := field.Tag.Lookup("json"); ok {
		tagName, _, hasOptions := strings.Cut(tag, ",")
		if tagName == "-" && !hasOptions {
			return "", false
		}
		if tagName != "" {
			name = tagName
		}
	}
	return name, true
}

func setSingleSchemaType(generated *jsonschema.Schema, typ string) {
	generated.Type = typ
	generated.Types = nil
}

func schemaHasConcreteNonObjectType(generated *jsonschema.Schema) bool {
	if generated == nil {
		return false
	}
	if generated.Type != "" {
		return generated.Type != "object"
	}
	if len(generated.Types) == 0 {
		return false
	}
	for _, typ := range generated.Types {
		if typ == "object" {
			return false
		}
	}
	return true
}
