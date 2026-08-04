package toolvalidate

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func Compile(tools []translator.OpenAITool) (Schemas, error) {
	schemas := make(Schemas, len(tools))
	for index, tool := range tools {
		name := tool.Function.Name
		compiler := jsonschema.NewCompiler()
		resource := fmt.Sprintf("urn:literouter:tool-schema:%d", index)
		if err := compiler.AddResource(resource, bytes.NewReader(tool.Function.Parameters)); err != nil {
			return nil, fmt.Errorf("tool %q has invalid input schema", name)
		}
		schema, err := compiler.Compile(resource)
		if err != nil {
			return nil, fmt.Errorf("tool %q has invalid input schema", name)
		}
		schemas[name] = schema
	}
	return schemas, nil
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
