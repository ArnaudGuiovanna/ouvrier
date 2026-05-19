package schema_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ouvrier/internal/schema"
)

type resultSchemaReply struct {
	Status string `json:"status"`
	Count  int    `json:"count,omitempty"`
}

func TestFromTypeGeneratesStrictJSONSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	if contract.Name != "schema_test.resultSchemaReply" {
		t.Fatalf("Name = %q, want schema_test.resultSchemaReply", contract.Name)
	}
	if contract.Type != reflect.TypeFor[resultSchemaReply]() {
		t.Fatalf("Type = %v, want resultSchemaReply", contract.Type)
	}
	if len(contract.JSONSchema) == 0 {
		t.Fatal("JSONSchema is empty")
	}

	var raw map[string]any
	if err := json.Unmarshal(contract.JSONSchema, &raw); err != nil {
		t.Fatalf("JSONSchema is not JSON: %v", err)
	}
	if raw["type"] != "object" {
		t.Fatalf("schema type = %v, want object", raw["type"])
	}
	if _, ok := raw["additionalProperties"]; !ok {
		t.Fatalf("schema does not declare strict additionalProperties: %s", contract.JSONSchema)
	}
}

func TestValidateJSONEnforcesGeneratedSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	if err := schema.ValidateJSON(contract, []byte(`{"status":"ok","count":2}`)); err != nil {
		t.Fatalf("ValidateJSON returned error for valid JSON: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"status":1}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for wrong field type")
	}
	err = schema.ValidateJSON(contract, []byte(`{"status":"ok","extra":true}`))
	if err == nil {
		t.Fatal("ValidateJSON returned nil for extra field")
	}
	if !strings.Contains(err.Error(), "additional properties") {
		t.Fatalf("ValidateJSON error = %v, want additional properties context", err)
	}
}

func TestValidateJSONRejectsMalformedJSON(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	if err := schema.ValidateJSON(contract, []byte(`{"status":"ok"`)); err == nil {
		t.Fatal("ValidateJSON returned nil for malformed JSON")
	}
}
