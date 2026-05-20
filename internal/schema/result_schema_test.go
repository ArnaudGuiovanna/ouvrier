package schema_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"ouvrier/internal/schema"
)

type resultSchemaReply struct {
	Status string `json:"status"`
	Count  int    `json:"count,omitempty"`
}

type resultSchemaOrder struct {
	ID       string                         `json:"id"`
	Items    []resultSchemaOrderItem        `json:"items"`
	Labels   map[string]string              `json:"labels"`
	Metadata *map[string]resultSchemaStatus `json:"metadata,omitempty"`
}

type resultSchemaOrderItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type resultSchemaStatus struct {
	State string `json:"state"`
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

func TestFromTypeGeneratesStrictRepresentativeJSONSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaOrder]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}
	if generated.Type != "object" {
		t.Fatalf("schema type = %q, want object", generated.Type)
	}
	items := generated.Properties["items"]
	if items == nil || items.Type != "array" || slices.Contains(items.Types, "null") {
		t.Fatalf("items schema = %+v, want non-null array", items)
	}
	labels := generated.Properties["labels"]
	if labels == nil || labels.Type != "object" || slices.Contains(labels.Types, "null") {
		t.Fatalf("labels schema = %+v, want non-null object", labels)
	}
	metadata := generated.Properties["metadata"]
	if metadata == nil || !slices.Contains(metadata.Types, "null") || !slices.Contains(metadata.Types, "object") {
		t.Fatalf("metadata schema = %+v, want nullable object", metadata)
	}

	valid := []byte(`{"id":"ord_1","items":[{"sku":"sku_1","quantity":2}],"labels":{"tier":"gold"},"metadata":null}`)
	if err := schema.ValidateJSON(contract, valid); err != nil {
		t.Fatalf("ValidateJSON returned error for valid representative output: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"id":"ord_1","items":null,"labels":{}}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for null required slice")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"id":"ord_1","items":[],"labels":null}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for null required map")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"id":"ord_1","items":[{"sku":"sku_1","quantity":2,"extra":true}],"labels":{}}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for extra nested field")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"items":[],"labels":{}}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for missing required field")
	}
}
