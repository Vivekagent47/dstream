package outbound

import (
	"strings"
	"testing"
)

func TestValidatePayloadAgainstSchema(t *testing.T) {
	schema := []byte(`{"type":"object","required":["amount"],"properties":{"amount":{"type":"integer"}}}`)
	if err := validatePayloadAgainstSchema(nil, []byte(`{"anything":true}`)); err != nil {
		t.Errorf("nil schema must skip: %v", err)
	}
	if err := validatePayloadAgainstSchema(schema, []byte(`{"amount":5}`)); err != nil {
		t.Errorf("valid payload: %v", err)
	}
	if err := validatePayloadAgainstSchema(schema, []byte(`{"amount":"nope"}`)); err == nil {
		t.Error("wrong type: want error")
	}
	if err := validatePayloadAgainstSchema(schema, []byte(`{}`)); err == nil {
		t.Error("missing required: want error")
	}
	if err := compileSchema([]byte(`{"type":"object"}`)); err != nil {
		t.Errorf("valid schema: %v", err)
	}
	if err := compileSchema([]byte(`{"type":123}`)); err == nil {
		t.Error("invalid schema: want error")
	}
}

// TestSchemaRejectsExternalRef proves the compiler refuses external $ref (LFI/DoS
// guard): a schema pointing $ref at a file:// url must fail to compile, and never
// os.Open that path.
func TestSchemaRejectsExternalRef(t *testing.T) {
	ext := []byte(`{"$ref":"file:///etc/hosts"}`)
	// Assert the REFUSAL fired — distinguishes "loader blocked the read" from the
	// default FileLoader having os.Open'd the file and failed to parse it.
	err := compileSchema(ext)
	if err == nil || !strings.Contains(err.Error(), "external schema references are not allowed") {
		t.Fatalf("external file:// $ref must be refused, got: %v", err)
	}
	if err := validatePayloadAgainstSchema(ext, []byte(`{}`)); err == nil {
		t.Fatal("external file:// $ref must be rejected on validate")
	}
	// self-contained internal $ref still compiles (loader not consulted).
	ok := []byte(`{"$defs":{"n":{"type":"integer"}},"properties":{"x":{"$ref":"#/$defs/n"}}}`)
	if err := compileSchema(ok); err != nil {
		t.Errorf("self-contained $ref must compile: %v", err)
	}
}
