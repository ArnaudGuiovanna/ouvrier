package schema_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

type resultSchemaPayload struct {
	Status string `json:"status"`
}

type resultSchemaTaggedOutput struct {
	PublicID  string                `json:"public_id"`
	Count     int                   `json:",omitempty"`
	Secret    string                `json:"-"`
	DashItems []resultSchemaPayload `json:"-,"`
}

type resultSchemaNullability struct {
	Name        string               `json:"name"`
	Count       int                  `json:"count,omitempty"`
	Active      bool                 `json:"active"`
	Tags        []string             `json:"tags"`
	Detail      resultSchemaPayload  `json:"detail"`
	MaybeName   *string              `json:"maybe_name,omitempty"`
	MaybeCount  *int                 `json:"maybe_count,omitempty"`
	MaybeTags   *[]string            `json:"maybe_tags,omitempty"`
	MaybeDetail *resultSchemaPayload `json:"maybe_detail,omitempty"`
}

type resultSchemaStampedReply struct {
	CreatedAt time.Time   `json:"created_at"`
	History   []time.Time `json:"history"`
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

func TestValidateJSONRespectsJSONTagsStrictly(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaTaggedOutput]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}
	if generated.Properties["public_id"] == nil {
		t.Fatalf("schema properties = %+v, want json tag name public_id", generated.Properties)
	}
	if generated.Properties["PublicID"] != nil || generated.Properties["Secret"] != nil {
		t.Fatalf("schema properties = %+v, want Go names and ignored fields omitted", generated.Properties)
	}
	dashItems := generated.Properties["-"]
	if dashItems == nil || dashItems.Type != "array" || slices.Contains(dashItems.Types, "null") {
		t.Fatalf("dash property schema = %+v, want non-null array for json:\"-,\" field", dashItems)
	}

	valid := []byte(`{"public_id":"pub_1","-":[{"status":"ok"}]}`)
	if err := schema.ValidateJSON(contract, valid); err != nil {
		t.Fatalf("ValidateJSON returned error for valid tagged output: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"PublicID":"pub_1","-":[{"status":"ok"}]}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for Go field name instead of json tag")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"public_id":"pub_1","Secret":"leak","-":[{"status":"ok"}]}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for ignored json field")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"public_id":"pub_1","-":null}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for null non-pointer slice with json dash tag")
	}
}

func TestValidateJSONAllowsNullOnlyForPointerFields(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaNullability]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	valid := []byte(`{
		"name":"ok",
		"active":true,
		"tags":["a"],
		"detail":{"status":"ok"},
		"maybe_name":null,
		"maybe_count":null,
		"maybe_tags":null,
		"maybe_detail":null
	}`)
	if err := schema.ValidateJSON(contract, valid); err != nil {
		t.Fatalf("ValidateJSON returned error for null pointer fields: %v", err)
	}

	for _, tc := range []struct {
		name string
		data string
	}{
		{
			name: "string",
			data: `{"name":null,"active":true,"tags":[],"detail":{"status":"ok"}}`,
		},
		{
			name: "omitempty integer",
			data: `{"name":"ok","count":null,"active":true,"tags":[],"detail":{"status":"ok"}}`,
		},
		{
			name: "boolean",
			data: `{"name":"ok","active":null,"tags":[],"detail":{"status":"ok"}}`,
		},
		{
			name: "slice",
			data: `{"name":"ok","active":true,"tags":null,"detail":{"status":"ok"}}`,
		},
		{
			name: "struct",
			data: `{"name":"ok","active":true,"tags":[],"detail":null}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := schema.ValidateJSON(contract, []byte(tc.data)); err == nil {
				t.Fatal("ValidateJSON returned nil for null non-pointer field")
			}
		})
	}
}

func TestFromTypePreservesTimeSchemasAsJSONStrings(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[resultSchemaStampedReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}

	createdAt := generated.Properties["created_at"]
	if createdAt == nil || createdAt.Type != "string" {
		t.Fatalf("created_at schema = %+v, want JSON string schema for time.Time", createdAt)
	}
	history := generated.Properties["history"]
	if history == nil || history.Type != "array" || history.Items == nil || history.Items.Type != "string" {
		t.Fatalf("history schema = %+v, want array of JSON string schemas for []time.Time", history)
	}

	valid := []byte(`{"created_at":"2026-05-24T12:00:00Z","history":["2026-05-24T12:00:00Z"]}`)
	if err := schema.ValidateJSON(contract, valid); err != nil {
		t.Fatalf("ValidateJSON returned error for valid time output: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"created_at":{},"history":[]}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for object in time.Time field")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"created_at":"2026-05-24T12:00:00Z","history":[{}]}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for object in []time.Time item")
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

func TestFromTypeGeneratesStrictTopLevelSliceSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[[]resultSchemaPayload]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}
	if generated.Type != "array" || slices.Contains(generated.Types, "null") {
		t.Fatalf("schema = %+v, want non-null top-level array", generated)
	}

	if err := schema.ValidateJSON(contract, []byte(`[{"status":"ok"}]`)); err != nil {
		t.Fatalf("ValidateJSON returned error for valid slice: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`null`)); err == nil {
		t.Fatal("ValidateJSON returned nil for null top-level slice")
	}
	if err := schema.ValidateJSON(contract, []byte(`[{"status":"ok","extra":true}]`)); err == nil {
		t.Fatal("ValidateJSON returned nil for extra nested slice field")
	}
}

func TestFromTypeGeneratesStrictTopLevelMapSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[map[string]resultSchemaPayload]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}
	if generated.Type != "object" || slices.Contains(generated.Types, "null") {
		t.Fatalf("schema = %+v, want non-null top-level object map", generated)
	}

	if err := schema.ValidateJSON(contract, []byte(`{"ticket":{"status":"ok"}}`)); err != nil {
		t.Fatalf("ValidateJSON returned error for valid map: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`null`)); err == nil {
		t.Fatal("ValidateJSON returned nil for null top-level map")
	}
	if err := schema.ValidateJSON(contract, []byte(`{"ticket":{"status":"ok","extra":true}}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for extra nested map value field")
	}
}

func TestFromTypeAllowsNullableTopLevelPointerSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[*resultSchemaPayload]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	var generated jsonschema.Schema
	if err := json.Unmarshal(contract.JSONSchema, &generated); err != nil {
		t.Fatalf("JSONSchema is not JSON Schema: %v", err)
	}
	if !slices.Contains(generated.Types, "null") || !slices.Contains(generated.Types, "object") {
		t.Fatalf("schema = %+v, want nullable top-level object pointer", generated)
	}

	if err := schema.ValidateJSON(contract, []byte(`null`)); err != nil {
		t.Fatalf("ValidateJSON returned error for null pointer output: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"status":"ok"}`)); err != nil {
		t.Fatalf("ValidateJSON returned error for valid pointer output: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"status":"ok","extra":true}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for extra pointer field")
	}
}

func TestFromTypeGeneratesScalarSchema(t *testing.T) {
	contract, err := schema.FromType(reflect.TypeFor[string]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}

	if err := schema.ValidateJSON(contract, []byte(`"ok"`)); err != nil {
		t.Fatalf("ValidateJSON returned error for valid scalar: %v", err)
	}
	if err := schema.ValidateJSON(contract, []byte(`{"status":"ok"}`)); err == nil {
		t.Fatal("ValidateJSON returned nil for object against scalar schema")
	}
}

func TestFromTypeRejectsUnsupportedTypes(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[func()](),
		reflect.TypeFor[chan string](),
		reflect.TypeFor[map[int]string](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			if _, err := schema.FromType(typ); err == nil {
				t.Fatal("FromType returned nil error for unsupported type")
			}
		})
	}
}
