package provider

import "testing"

func TestRequestValidate(t *testing.T) {
	valid := Request{Model: "model", Messages: []Message{{Role: "user", Content: []Content{{Type: "text", Text: "hello"}}}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	tests := []Request{
		{},
		{Model: "model"},
		{Model: "model", Messages: []Message{{Role: "system", Content: []Content{{Type: "text"}}}}},
		{Model: "model", Messages: []Message{{Role: "user"}}},
		{Model: "model", Messages: valid.Messages, Tools: []Tool{{Name: "tool", InputSchema: []byte(`{`)}}},
		{Model: "model", Messages: valid.Messages, Effort: "extreme"},
	}
	for _, request := range tests {
		if err := request.Validate(); err == nil {
			t.Fatalf("Validate(%#v) error = nil", request)
		}
	}
}
