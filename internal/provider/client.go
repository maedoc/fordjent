package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fordjent/fordjent/internal/config"
)

// Message represents a chat message in the LLM conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a tool call from the LLM.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents the function name and arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef defines a tool for the LLM.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef defines a function tool.
type FunctionDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// Response represents the LLM response.
type Response struct {
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	StopReason string     `json:"stop_reason"`
}

// Usage tracks token consumption from the LLM API.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Cost returns the estimated cost in USD for this usage.
func (u *Usage) Cost(cfg *config.ProviderConfig) float64 {
	inputCost := float64(u.PromptTokens) / 1_000_000 * cfg.CostPer1MInputTokens
	outputCost := float64(u.CompletionTokens) / 1_000_000 * cfg.CostPer1MOutputTokens
	return inputCost + outputCost
}

// openAIRequest is the OpenAI-compatible chat completion request.
type openAIRequest struct {
	Model       string        `json:"model"`
	Messages    []messageJSON `json:"messages"`
	Tools       []toolJSON    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

type messageJSON struct {
	Role       string         `json:"role"`
	Content    interface{}    `json:"content"`
	ToolCalls  []toolCallJSON `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type toolJSON struct {
	Type     string       `json:"type"`
	Function functionJSON `json:"function"`
}

type functionJSON struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type toolCallJSON struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function toolCallFunctionJSON `json:"function"`
}

type toolCallFunctionJSON struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openAIResponse is the OpenAI-compatible chat completion response.
type openAIResponse struct {
	Choices []struct {
		Message struct {
			Role      string         `json:"role"`
			Content   string         `json:"content"`
			ToolCalls []toolCallJSON `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Client is an LLM provider client using OpenAI-compatible API.
type Client struct {
	cfg    *config.ProviderConfig
	client *http.Client
	retry  RetryPolicy
}

// NewClient creates a new provider client with retry support.
func NewClient(cfg *config.ProviderConfig) *Client {
	retry := DefaultRetryPolicy()
	if cfg.MaxRetries > 0 {
		retry.MaxRetries = cfg.MaxRetries
	}
	if cfg.RetryBaseDelay > 0 {
		retry.BaseDelay = cfg.RetryBaseDelay
	}
	if cfg.RetryMaxDelay > 0 {
		retry.MaxDelay = cfg.RetryMaxDelay
	}

	timeout := 120 * time.Second
	if cfg.RequestTimeout > 0 {
		timeout = cfg.RequestTimeout
	}

	return &Client{
		cfg: cfg,
		client: &http.Client{
			Timeout: timeout,
		},
		retry: retry,
	}
}

// Cfg returns the provider configuration.
func (c *Client) Cfg() *config.ProviderConfig { return c.cfg }

// setRequestTimeout creates a child context with the configured timeout.
func (c *Client) setRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := c.client.Timeout
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// Chat sends a chat completion request to the LLM. It retries on transient errors.
func (c *Client) Chat(ctx context.Context, systemPrompt string, messages []Message, tools []ToolDef) (*Response, *Usage, error) {
	var resp *Response
	var usage *Usage

	err := c.retry.Retry(ctx, func() error {
		reqCtx, cancel := c.setRequestTimeout(ctx)
		defer cancel()

		r, u, err := c.chatOnce(reqCtx, systemPrompt, messages, tools)
		resp = r
		usage = u
		return err
	})

	return resp, usage, err
}

// chatOnce makes a single API call (no retry).
func (c *Client) chatOnce(ctx context.Context, systemPrompt string, messages []Message, tools []ToolDef) (*Response, *Usage, error) {
	// Build request messages
	var reqMessages []messageJSON
	reqMessages = append(reqMessages, messageJSON{
		Role:    "system",
		Content: systemPrompt,
	})

	for _, msg := range messages {
		mj := messageJSON{
			Role:       msg.Role,
			Content:    msg.Content,
			ToolCallID: msg.ToolCallID,
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				mj.ToolCalls = append(mj.ToolCalls, toolCallJSON{
					ID:   tc.ID,
					Type: tc.Type,
					Function: toolCallFunctionJSON{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		reqMessages = append(reqMessages, mj)
	}

	// Build tools
	var reqTools []toolJSON
	for _, t := range tools {
		reqTools = append(reqTools, toolJSON{
			Type: t.Type,
			Function: functionJSON{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}

	reqBody := openAIRequest{
		Model:       c.cfg.Model,
		Messages:    reqMessages,
		MaxTokens:   c.cfg.MaxTokens,
		Temperature: 0.3,
	}
	if len(reqTools) > 0 {
		reqBody.Tools = reqTools
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.APIBase, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	respHTTP, err := c.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}
	defer respHTTP.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(respHTTP.Body, 10<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}

	if respHTTP.StatusCode != http.StatusOK {
		return nil, nil, &HTTPError{
			StatusCode: respHTTP.StatusCode,
			Body:       string(respBody),
			Err:        fmt.Errorf("LLM API error (%d): %s", respHTTP.StatusCode, string(respBody)),
		}
	}

	var openaiResp openAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(openaiResp.Choices) == 0 {
		return nil, nil, fmt.Errorf("no choices in response")
	}

	choice := openaiResp.Choices[0]
	result := &Response{
		Content:    choice.Message.Content,
		StopReason: choice.FinishReason,
	}

	for _, tc := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	usage := &Usage{
		PromptTokens:     openaiResp.Usage.PromptTokens,
		CompletionTokens: openaiResp.Usage.CompletionTokens,
		TotalTokens:      openaiResp.Usage.TotalTokens,
	}

	return result, usage, nil
}
