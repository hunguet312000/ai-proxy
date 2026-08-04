package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleClientDoJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer key" || request.URL.Path != "/v1/chat/completions" {
			t.Errorf("request = %#v", request)
		}
		_, _ = w.Write([]byte(`{"id":"response"}`))
	}))
	defer server.Close()
	client, err := NewOpenAICompatibleClient("test", server.URL+"/v1", "key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := client.DoJSON(context.Background(), "/chat/completions", map[string]string{"model": "model"}, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != "response" {
		t.Fatalf("response = %#v", response)
	}
}

func TestOpenAICompatibleClientProviderError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()
	client, _ := NewOpenAICompatibleClient("test", server.URL, "key", server.Client())
	var response any
	err := client.DoJSON(context.Background(), "/chat", map[string]string{}, &response)
	var providerError *ProviderError
	if !errors.As(err, &providerError) || providerError.StatusCode != 429 || providerError.RetryAfter != 30*time.Second {
		t.Fatalf("error = %#v", err)
	}
}

func TestDecodeProviderErrorSupportsCompatibleShapes(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		message string
		code    string
	}{
		{name: "nested", body: `{"error":{"code":"bad_content","message":"unsupported content type"}}`, message: "unsupported content type", code: "bad_content"},
		{name: "top level", body: `{"code":400,"detail":"invalid messages payload"}`, message: "invalid messages payload", code: "400"},
		{name: "error string", body: `{"error":"model rejected request"}`, message: "model rejected request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: make(http.Header)}
			err := DecodeProviderError("xai", response, strings.NewReader(test.body))
			var providerError *ProviderError
			if !errors.As(err, &providerError) || providerError.Message != test.message || providerError.Code != test.code {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestDecodeProviderErrorLimitsUnknownBody(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Header: make(http.Header)}
	err := DecodeProviderError("xai", response, strings.NewReader(strings.Repeat("x", 2000)))
	var providerError *ProviderError
	if !errors.As(err, &providerError) || len(providerError.Message) != 1027 || !strings.HasSuffix(providerError.Message, "...") {
		t.Fatalf("error = %#v", err)
	}
}

func TestOpenAICompatibleClientRejectsInsecureRemoteURL(t *testing.T) {
	if _, err := NewOpenAICompatibleClient("test", "http://example.com/v1", "key", nil); err == nil {
		t.Fatal("NewOpenAICompatibleClient() error = nil")
	}
}
