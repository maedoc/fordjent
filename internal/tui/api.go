package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/lifecycle"
)

type StatusClient struct {
	baseURL string
	client  *http.Client
}

func NewStatusClient(baseURL string) *StatusClient {
	return &StatusClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *StatusClient) FetchStatus(ctx context.Context) (*StatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/status returned %d", resp.StatusCode)
	}
	var result StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *StatusClient) FetchTokensPerMinute(ctx context.Context, hours int) ([]TokenMinute, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/tokens-per-minute?hours=%d", c.baseURL, hours), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/tokens-per-minute returned %d", resp.StatusCode)
	}
	var result []TokenMinute
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

type SSEClient struct {
	url    string
	client *http.Client
	Events chan lifecycle.SSEEvent
	done   chan struct{}
	lastID string
	mu     sync.Mutex
	body   io.ReadCloser
}

func NewSSEClient(baseURL string) *SSEClient {
	return &SSEClient{
		url:    strings.TrimRight(baseURL, "/") + "/acp/v1/stream",
		client: &http.Client{},
		Events: make(chan lifecycle.SSEEvent, 64),
		done:   make(chan struct{}),
	}
}

func (c *SSEClient) GetLastEventID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastID
}

func (c *SSEClient) setLastID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastID = id
}

func (c *SSEClient) Connect(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE connection failed: %d", resp.StatusCode)
	}

	c.body = resp.Body
	go c.readStream(resp.Body)
	return nil
}

func (c *SSEClient) Reconnect(ctx context.Context) error {
	if c.body != nil {
		c.body.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if id := c.GetLastEventID(); id != "" {
		req.Header.Set("Last-Event-ID", id)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE reconnection failed: %d", resp.StatusCode)
	}

	c.body = resp.Body
	go c.readStream(resp.Body)
	return nil
}

func (c *SSEClient) Close() {
	close(c.done)
}

func (c *SSEClient) readStream(body io.ReadCloser) {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	var eventType, data, id string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		} else if strings.HasPrefix(line, "id: ") {
			id = strings.TrimPrefix(line, "id: ")
			c.setLastID(id)
		} else if line == "" && (eventType != "" || data != "") {
			select {
			case c.Events <- lifecycle.SSEEvent{Type: eventType, Data: data, ID: id}:
			case <-c.done:
				return
			}
			eventType = ""
			data = ""
			id = ""
		}
	}
}

type StatusResponse struct {
	Now       string          `json:"now"`
	Costs     StatusCosts     `json:"costs"`
	ByModel   []ModelCostRow  `json:"by_model"`
	Lifecycle StatusLifecycle `json:"lifecycle"`
	Metrics   StatusMetrics   `json:"metrics"`
}

type StatusCosts struct {
	TotalSessions int     `json:"total_sessions"`
	TotalTokens   int64   `json:"total_tokens"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
}

type ModelCostRow struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	Calls       int64   `json:"calls"`
	InputTokens int64   `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

type StatusLifecycle struct {
	ActiveSessions int       `json:"active_sessions"`
	FailedSessions int       `json:"failed_sessions"`
	RecentTurns    []TurnRow `json:"recent_turns"`
}

type TurnRow struct {
	SessionKey string `json:"session_key"`
	Turn       int    `json:"turn"`
	ToolCalls  int    `json:"tool_calls"`
	LatencyMs  int    `json:"latency_ms"`
	TokensIn   int    `json:"tokens_in"`
	TokensOut  int    `json:"tokens_out"`
	Error      string `json:"error"`
	Timestamp  string `json:"timestamp"`
}

type StatusMetrics struct {
	EventsTotal     int64   `json:"events_total"`
	SessionsTotal   int64   `json:"sessions_total"`
	SessionsActive  int64   `json:"sessions_active"`
	LLMCallsTotal   int64   `json:"llm_calls_total"`
	LLMRetriesTotal int64   `json:"llm_retries_total"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CostUSD         float64 `json:"cost_usd"`
}

type TokenMinute struct {
	Minute       string `json:"minute"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Calls        int64  `json:"calls"`
}