package toolvalidate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"

	"literouter/internal/translator"
)

type Category string

const (
	MalformedJSON  Category = "malformed_json"
	NonObject      Category = "non_object"
	UndeclaredTool Category = "undeclared_tool"
	SchemaMismatch Category = "schema_mismatch"
)

type Error struct {
	Tool     string
	Category Category
	Bytes    int
}

func (e *Error) Error() string {
	return fmt.Sprintf("tool %q returned invalid arguments (%s, %d bytes)", e.Tool, e.Category, e.Bytes)
}

type Schemas map[string]*jsonschema.Schema

// Compile prepares the argument validators for one turn's tools. It never fails the
// turn.
//
// These schemas only annotate tool calls on the way back, and a call whose schema is
// missing is already handled: Validate reports UndeclaredTool and the caller forwards the
// call with a warning rather than dropping it. So skipping a schema this compiler cannot
// read costs one unvalidated call, while refusing the request took out every turn in the
// session — over a tool the model may never even call.
//
// It has to be that way round because the tool list is whatever MCP servers the client
// happens to have loaded, which is not a set this proxy gets to veto. Shapes seen to fail
// here are ordinary: a zero-argument tool that omits its schema entirely, a $ref to
// another document, and draft 2020-12 keywords this compiler does not implement.
func Compile(tools []translator.OpenAITool) Schemas {
	schemas := make(Schemas, len(tools))
	for index, tool := range tools {
		name := tool.Function.Name
		compiler := jsonschema.NewCompiler()
		resource := fmt.Sprintf("urn:literouter:tool-schema:%d", index)
		if err := compiler.AddResource(resource, bytes.NewReader(tool.Function.Parameters)); err != nil {
			slog.Debug("tool schema could not be read; its calls are forwarded unvalidated",
				"tool", name, "error", err)
			continue
		}
		schema, err := compiler.Compile(resource)
		if err != nil {
			slog.Debug("tool schema could not be compiled; its calls are forwarded unvalidated",
				"tool", name, "error", err)
			continue
		}
		schemas[name] = schema
	}
	return schemas
}

func (schemas Schemas) Validate(name, arguments string) error {
	if arguments == "" {
		arguments = "{}"
	}
	if !json.Valid([]byte(arguments)) {
		return &Error{Tool: name, Category: MalformedJSON, Bytes: len(arguments)}
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return &Error{Tool: name, Category: NonObject, Bytes: len(arguments)}
	}
	schema := schemas[name]
	if schema == nil {
		return &Error{Tool: name, Category: UndeclaredTool, Bytes: len(arguments)}
	}
	if err := schema.Validate(object); err != nil {
		return &Error{Tool: name, Category: SchemaMismatch, Bytes: len(arguments)}
	}
	return nil
}
