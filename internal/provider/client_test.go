package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fordjent/fordjent/internal/config"
)

func TestChatNoToolCalls(t *testing.T) {
	// Mock OpenAI-compatible API server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected /chat/completions, got %s", r.URL.Path)
		}

		resp := openAIResponse{
			Choices: []struct {
				Message struct {
					Role      string         `json:"role"`
					Content   string         `json:"content"`
					ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Message: struct {
						Role      string         `json:"role"`
						Content   string         `json:"content"`
						ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
					}{
						Role:    "assistant",
						Content: "I've analyzed the issue. Here's what I found...",
					},
					FinishReason: "stop",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{
		Name:      "test",
		APIBase:   server.URL,
		APIKey:    "test-key",
		Model:     "test-model",
		MaxTokens: 4096,
	}

	client := NewClient(cfg)
	resp, err := client.Chat(context.Background(), "You are a helpful assistant.", []Message{
		{Role: "user", Content: "Hello"},
	}, nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "I've analyzed the issue. Here's what I found..." {
		t.Errorf("unexpected content: %s", resp.Content)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(resp.ToolCalls))
	}
}

func TestChatWithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openAIResponse{
			Choices: []struct {
				Message struct {
					Role      string         `json:"role"`
					Content   string         `json:"content"`
					ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Message: struct {
						Role      string         `json:"role"`
						Content   string         `json:"content"`
						ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
					}{
						Role:    "assistant",
						Content: "",
						ToolCalls: []toolCallJSON{
							{
								ID:   "call_123",
								Type: "function",
								Function: toolCallFunctionJSON{
									Name:      "forgejo_comment",
									Arguments: `{"repository":"org/repo","issue_number":42,"body":"Hello!"}`,
								},
							},
						},
					},
					FinishReason: "tool_calls",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{
		Name:      "test",
		APIBase:   server.URL,
		APIKey:    "test-key",
		Model:     "test-model",
		MaxTokens: 4096,
	}

	client := NewClient(cfg)
	resp, err := client.Chat(context.Background(), "system", []Message{
		{Role: "user", Content: "comment on the issue"},
	}, []ToolDef{
		{
			Type: "function",
			Function: FunctionDef{
				Name:        "forgejo_comment",
				Description: "Post a comment",
				Parameters:  map[string]interface{}{"type": "object"},
			},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "forgejo_comment" {
		t.Errorf("expected forgejo_comment, got %s", resp.ToolCalls[0].Function.Name)
	}
}

func TestChatAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`))
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{
		Name:      "test",
		APIBase:   server.URL,
		APIKey:    "test-key",
		Model:     "test-model",
		MaxTokens: 4096,
	}

	client := NewClient(cfg)
	_, err := client.Chat(context.Background(), "system", []Message{
		{Role: "user", Content: "hello"},
	}, nil)

	if err == nil {
		t.Error("expected error from API")
	}
}
