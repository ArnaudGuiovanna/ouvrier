package ovr

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

func toolInputSchema(tool toolSpec) json.RawMessage {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	if tool.fnType == nil || tool.fnType.NumIn() < 2 {
		return mustMarshalSchema(schema)
	}

	argType := tool.fnType.In(1)
	if argType.Kind() == reflect.Pointer {
		argType = argType.Elem()
	}
	if argType.Kind() == reflect.Struct {
		return structToolSchema(argType, tool.params)
	}
	return singleValueToolSchema(argType, tool.params)
}

func structToolSchema(typ reflect.Type, params map[string]string) json.RawMessage {
	properties := map[string]any{}
	var required []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty := jsonFieldName(field)
		if name == "-" {
			continue
		}
		fieldSchema := map[string]any{"type": jsonSchemaType(field.Type)}
		if description := params[name]; description != "" {
			fieldSchema["description"] = description
		}
		properties[name] = fieldSchema
		if !omitEmpty {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	return objectToolSchema(properties, required)
}

func singleValueToolSchema(typ reflect.Type, params map[string]string) json.RawMessage {
	name := "value"
	if len(params) == 1 {
		for param := range params {
			name = param
		}
	}
	fieldSchema := map[string]any{"type": jsonSchemaType(typ)}
	if description := params[name]; description != "" {
		fieldSchema["description"] = description
	}
	return objectToolSchema(map[string]any{name: fieldSchema}, []string{name})
}

func objectToolSchema(properties map[string]any, required []string) json.RawMessage {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return mustMarshalSchema(schema)
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	name := field.Name
	omitEmpty := false
	if tag := field.Tag.Get("json"); tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			name = parts[0]
		}
		for _, part := range parts[1:] {
			if part == "omitempty" {
				omitEmpty = true
			}
		}
	}
	return name, omitEmpty
}

func jsonSchemaType(typ reflect.Type) string {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Array, reflect.Slice:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return "string"
	}
}

func mustMarshalSchema(schema map[string]any) json.RawMessage {
	raw, err := json.Marshal(schema)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}
