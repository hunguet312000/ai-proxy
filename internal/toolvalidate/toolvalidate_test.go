package toolvalidate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"literouter/internal/translator"
)

// editTool is the shape that matters most in practice: a failed edit is what an invalid
// tool call looks like from inside Claude Code.
func editTool() []translator.OpenAITool {
	return []translator.OpenAITool{{
		Type: "function",
		Function: translator.OpenAIFunction{
			Name: "Edit",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"file_path": {"type": "string"},
					"old_string": {"type": "string"},
					"new_string": {"type": "string"},
					"replace_all": {"type": "boolean"}
				},
				"required": ["file_path", "old_string", "new_string"],
				"additionalProperties": false
			}`),
		},
	}}
}

func compile(t *testing.T, tools []translator.OpenAITool) Schemas {
	t.Helper()
	return Compile(tools)
}

func categoryOf(t *testing.T, err error) Category {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error %v is not a *toolvalidate.Error", err)
	}
	return typed.Category
}

func TestValidateAcceptsAWellFormedEdit(t *testing.T) {
	schemas := compile(t, editTool())
	arguments := `{"file_path":"/tmp/a.go","old_string":"a","new_string":"b"}`
	if err := schemas.Validate("Edit", arguments); err != nil {
		t.Fatalf("valid arguments rejected: %v", err)
	}
}

func TestValidateSeparatesTheWaysArgumentsGoWrong(t *testing.T) {
	// The category drives the retry message the model is shown, so conflating two of
	// them tells the model to fix the wrong thing.
	schemas := compile(t, editTool())
	for name, testCase := range map[string]struct {
		arguments string
		want      Category
	}{
		"truncated mid-stream":   {`{"file_path":"/tmp/a.go","old_str`, MalformedJSON},
		"a JSON array":           {`["/tmp/a.go"]`, NonObject},
		"a bare string":          {`"just text"`, NonObject},
		"missing a required key": {`{"file_path":"/tmp/a.go"}`, SchemaMismatch},
		"wrong type":             {`{"file_path":1,"old_string":"a","new_string":"b"}`, SchemaMismatch},
		"an undeclared key":      {`{"file_path":"/a","old_string":"a","new_string":"b","nope":1}`, SchemaMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			err := schemas.Validate("Edit", testCase.arguments)
			if err == nil {
				t.Fatalf("accepted %s", name)
			}
			if got := categoryOf(t, err); got != testCase.want {
				t.Errorf("category = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestValidateReportsACallToAToolNobodyDeclared(t *testing.T) {
	// A model inventing a tool is not a schema problem, and telling it to match a schema
	// it was never given would send it in circles.
	schemas := compile(t, editTool())
	err := schemas.Validate("WriteFile", `{"path":"/tmp/a"}`)
	if err == nil {
		t.Fatal("a call to an undeclared tool must not pass")
	}
	if got := categoryOf(t, err); got != UndeclaredTool {
		t.Errorf("category = %q, want %q", got, UndeclaredTool)
	}
}

func TestValidateTreatsEmptyArgumentsAsAnEmptyObject(t *testing.T) {
	// Providers stream nothing at all for a no-argument tool. Reading that as malformed
	// would fail every such call.
	schemas := compile(t, []translator.OpenAITool{{
		Type:     "function",
		Function: translator.OpenAIFunction{Name: "ListDir", Parameters: json.RawMessage(`{"type":"object"}`)},
	}})
	if err := schemas.Validate("ListDir", ""); err != nil {
		t.Fatalf("empty arguments rejected: %v", err)
	}
}

func TestValidateKeepsLargeIntegersExact(t *testing.T) {
	// Decoding through float64 silently rounds a large offset, so a tool call that was
	// valid on the wire would be forwarded with different numbers than the model sent.
	schemas := compile(t, []translator.OpenAITool{{
		Type: "function",
		Function: translator.OpenAIFunction{
			Name:       "Read",
			Parameters: json.RawMessage(`{"type":"object","properties":{"offset":{"type":"integer"}}}`),
		},
	}})
	if err := schemas.Validate("Read", `{"offset":9007199254740993}`); err != nil {
		t.Fatalf("large integer rejected: %v", err)
	}
}

func TestErrorMessageNamesToolCategoryAndSize(t *testing.T) {
	// This string is what shows up in the logs when an agent run goes wrong, and the
	// byte count is how a truncation is told apart from a model mistake.
	err := (&Error{Tool: "Edit", Category: MalformedJSON, Bytes: 4096}).Error()
	for _, want := range []string{`"Edit"`, "malformed_json", "4096"} {
		if !strings.Contains(err, want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

// This used to fail the whole request, on the reasoning that a broken declaration must
// not silently disable validation for every tool. Skipping per tool serves that reasoning
// better: the healthy tools keep their validation, and the broken one costs one
// unvalidated call instead of the entire session — because the caller forwards a call that
// fails validation rather than dropping it, refusing here bought nothing and broke turns.
func TestCompileSkipsAToolWhoseSchemaIsNotUsable(t *testing.T) {
	schemas := Compile([]translator.OpenAITool{
		{Type: "function", Function: translator.OpenAIFunction{
			Name: "Broken", Parameters: json.RawMessage(`{"type": 12}`)}},
		{Type: "function", Function: translator.OpenAIFunction{
			Name: "Healthy", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false}`)}},
	})
	if _, ok := schemas["Broken"]; ok {
		t.Fatal("an unusable schema must not be kept")
	}
	if _, ok := schemas["Healthy"]; !ok {
		t.Fatal("a sibling tool must keep its validation")
	}
	if categoryOf(t, schemas.Validate("Broken", `{"a":1}`)) != UndeclaredTool {
		t.Fatal("a skipped tool must be reported as undeclared, which the caller forwards with a warning")
	}
}

// Every one of these is a schema a real MCP server can emit, and each one used to turn
// every turn in the session into a 400.
func TestCompileToleratesSchemaShapesRealClientsSend(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema json.RawMessage
	}{
		{"omitted entirely by a zero-argument tool", nil},
		{"a $ref into another document", json.RawMessage(`{"$ref":"https://example.com/schema.json"}`)},
		{"a $ref that does not resolve", json.RawMessage(`{"$ref":"#/definitions/missing"}`)},
		{"draft 2020-12 keywords", json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","prefixItems":[]}`)},
		{"truncated JSON", json.RawMessage(`{"type":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			schemas := Compile([]translator.OpenAITool{
				{Type: "function", Function: translator.OpenAIFunction{Name: "Odd", Parameters: test.schema}},
				{Type: "function", Function: translator.OpenAIFunction{
					Name: "Read", Parameters: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)}},
			})
			// Whatever happened to the odd tool, the turn survives and the rest still works.
			if _, ok := schemas["Read"]; !ok {
				t.Fatal("a well-formed sibling lost its validation")
			}
			if err := schemas.Validate("Read", `{"path":"/a.go"}`); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
		})
	}
}

func TestCompileWithoutToolsValidatesNothingRatherThanEverything(t *testing.T) {
	// A request that declares no tools must not have upstream tool calls measured
	// against an empty schema set and then all reported as undeclared — the gateway
	// drops them instead, and that is tested there.
	schemas := compile(t, nil)
	if len(schemas) != 0 {
		t.Fatalf("schemas = %d, want none", len(schemas))
	}
}
