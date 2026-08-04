package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"literouter/internal/cache"
	"literouter/internal/translator"
)

func BenchmarkGatewayNonStream(b *testing.B) {
	service := New(Options{OpenAI: &fakeClient{}, ResponseCache: cache.NewResponseCache(100, 0)})
	request := translator.OpenAIRequest{
		Model: "gpt-4.1", MaxTokens: 128,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hello"}},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := service.Chat(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGatewayWarmResponseCache(b *testing.B) {
	service := New(Options{OpenAI: &fakeClient{}, ResponseCache: cache.NewResponseCache(100, 0)})
	request := translator.OpenAIRequest{
		Model: "gpt-4.1", MaxTokens: 128,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: strings.Repeat("prompt ", 1000)}},
	}
	if _, err := service.Chat(context.Background(), request); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := service.Chat(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadOpenAIStream(b *testing.B) {
	var stream strings.Builder
	for index := range 1000 {
		fmt.Fprintf(&stream, "data: {\"id\":\"r\",\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", index)
	}
	stream.WriteString("data: [DONE]\n\n")
	payload := stream.String()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if err := readOpenAIStream(strings.NewReader(payload), func(OpenAIStreamChunk) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamOpen(b *testing.B) {
	client := &benchmarkStreamClient{payload: "data: [DONE]\n\n"}
	request := translator.OpenAIRequest{Model: "gpt-4.1", Stream: true}
	b.ReportAllocs()
	for b.Loop() {
		body, err := client.DoStream(context.Background(), "/chat/completions", request)
		if err != nil {
			b.Fatal(err)
		}
		_ = body.Close()
	}
}

type benchmarkStreamClient struct{ payload string }

func (client *benchmarkStreamClient) DoStream(context.Context, string, any) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(client.payload)), nil
}
