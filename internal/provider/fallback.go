package provider

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fordjent/fordjent/internal/config"
)

type FallbackClient struct {
	primary  *Client
	fallback *Client
}

func NewFallbackClient(primary, fallback *Client) *FallbackClient {
	return &FallbackClient{primary: primary, fallback: fallback}
}

func (fc *FallbackClient) Chat(ctx context.Context, systemPrompt string, messages []Message, tools []ToolDef) (*Response, *Usage, error) {
	resp, usage, err := fc.primary.Chat(ctx, systemPrompt, messages, tools)
	if err == nil {
		return resp, usage, nil
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode >= 500 {
		slog.Warn("primary provider failed, falling back",
			"primary", fc.primary.Cfg().Name,
			"fallback", fc.fallback.Cfg().Name,
			"error", err,
		)
		return fc.fallback.Chat(ctx, systemPrompt, messages, tools)
	}

	var retryErr *RetryError
	if errors.As(err, &retryErr) {
		slog.Warn("primary provider exhausted retries, falling back",
			"primary", fc.primary.Cfg().Name,
			"fallback", fc.fallback.Cfg().Name,
			"attempts", retryErr.Attempts,
		)
		return fc.fallback.Chat(ctx, systemPrompt, messages, tools)
	}

	return nil, nil, err
}

func (fc *FallbackClient) Cfg() *config.ProviderConfig {
	return fc.primary.Cfg()
}
