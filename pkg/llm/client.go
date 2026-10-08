package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Bounds for LLM request gate configuration.
const (
	DefaultLLMConcurrency      = 2
	MinLLMConcurrency          = 1
	MaxLLMConcurrency          = 16
	DefaultLLMMinInterval      = 1 * time.Second
	MinLLMMinInterval          = 1 * time.Millisecond
	MaxLLMMinInterval          = 1 * time.Minute
	DefaultLLMResponseMaxBytes = 1048576 // 1 MiB
	MinLLMResponseMaxBytes     = 65536   // 64 KiB
	MaxLLMResponseMaxBytes     = 16777216 // 16 MiB
)

// Clock defines the time interface used for spacing request starts.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RequestGate limits concurrent LLM requests and spaces request starts.
type RequestGate interface {
	Acquire(ctx context.Context) (func(), error)
	MaxResponseBytes() int64
}

// DefaultRequestGate implements RequestGate using buffered permits and a start spacer token.
type DefaultRequestGate struct {
	concurrency      int
	minInterval      time.Duration
	maxResponseBytes int64
	clock            Clock
	permits          chan struct{}
	startTokens      chan struct{}
	nextStart        time.Time
}

// NewRequestGate creates a new DefaultRequestGate validating bounds.
func NewRequestGate(concurrency int, minInterval time.Duration, maxResponseBytes int64, clock Clock) (*DefaultRequestGate, error) {
	if concurrency < MinLLMConcurrency || concurrency > MaxLLMConcurrency {
		return nil, fmt.Errorf("invalid LLM concurrency %d: must be between %d and %d", concurrency, MinLLMConcurrency, MaxLLMConcurrency)
	}
	if minInterval < MinLLMMinInterval || minInterval > MaxLLMMinInterval {
		return nil, fmt.Errorf("invalid LLM min interval %v: must be between %v and %v", minInterval, MinLLMMinInterval, MaxLLMMinInterval)
	}
	if maxResponseBytes < MinLLMResponseMaxBytes || maxResponseBytes > MaxLLMResponseMaxBytes {
		return nil, fmt.Errorf("invalid LLM response max bytes %d: must be between %d and %d", maxResponseBytes, MinLLMResponseMaxBytes, MaxLLMResponseMaxBytes)
	}
	if clock == nil {
		clock = realClock{}
	}

	permits := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		permits <- struct{}{}
	}

	startTokens := make(chan struct{}, 1)
	startTokens <- struct{}{}

	startTime := clock.Now()
	nextStart := startTime.Add(minInterval)

	return &DefaultRequestGate{
		concurrency:      concurrency,
		minInterval:      minInterval,
		maxResponseBytes: maxResponseBytes,
		clock:            clock,
		permits:          permits,
		startTokens:      startTokens,
		nextStart:        nextStart,
	}, nil
}

func (g *DefaultRequestGate) MaxResponseBytes() int64 {
	return g.maxResponseBytes
}

func (g *DefaultRequestGate) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. Acquire concurrency permit
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.permits:
	}

	permitReleased := false
	releasePermit := func() {
		if !permitReleased {
			permitReleased = true
			g.permits <- struct{}{}
		}
	}

	// 2. Wait for start token (cancellable)
	select {
	case <-ctx.Done():
		releasePermit()
		return nil, ctx.Err()
	case <-g.startTokens:
	}

	// 3. Space start rate
	now := g.clock.Now()
	if now.Before(g.nextStart) {
		wait := g.nextStart.Sub(now)
		select {
		case <-ctx.Done():
			g.startTokens <- struct{}{}
			releasePermit()
			return nil, ctx.Err()
		case <-g.clock.After(wait):
		}
	}

	actualNow := g.clock.Now()
	g.nextStart = actualNow.Add(g.minInterval)
	g.startTokens <- struct{}{}

	var once sync.Once
	release := func() {
		once.Do(func() {
			releasePermit()
		})
	}
	return release, nil
}

type contextKey string

const admissionContextKey contextKey = "llm.request_gate"

// WithAdmission injects a RequestGate into the context.
func WithAdmission(ctx context.Context, gate RequestGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, admissionContextKey, gate)
}

// GateFromContext retrieves a RequestGate from context if present.
func GateFromContext(ctx context.Context) RequestGate {
	if ctx == nil {
		return nil
	}
	if g, ok := ctx.Value(admissionContextKey).(RequestGate); ok {
		return g
	}
	return nil
}

type Client struct {
	baseURL          string
	apiKey           string
	model            string
	httpClient       *http.Client
	maxResponseBytes int64
}

func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		maxResponseBytes: DefaultLLMResponseMaxBytes,
	}
}

// SetTimeout allows customizing the HTTP timeout
func (c *Client) SetTimeout(timeout time.Duration) {
	c.httpClient.Timeout = timeout
}

// SetHTTPClient allows injecting a custom http.Client (e.g. for testing with custom transports).
func (c *Client) SetHTTPClient(client *http.Client) {
	if client != nil {
		c.httpClient = client
	}
}

// SetMaxResponseBytes customizes the maximum allowed response size for this client.
func (c *Client) SetMaxResponseBytes(limit int64) {
	c.maxResponseBytes = limit
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float32       `json:"temperature"`
}

type ChatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (c *Client) ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	// Validate inputs
	if c.baseURL == "" {
		return "", fmt.Errorf("base URL is not configured")
	}
	if c.apiKey == "" {
		return "", fmt.Errorf("API key is not configured")
	}
	if c.model == "" {
		return "", fmt.Errorf("model is not configured")
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return "", fmt.Errorf("system prompt cannot be empty")
	}
	if strings.TrimSpace(userPrompt) == "" {
		return "", fmt.Errorf("user prompt cannot be empty")
	}

	gate := GateFromContext(ctx)
	if gate != nil {
		release, err := gate.Acquire(ctx)
		if err != nil {
			return "", err
		}
		defer release()
	}

	maxBytes := int64(DefaultLLMResponseMaxBytes)
	if gate != nil && gate.MaxResponseBytes() > 0 {
		maxBytes = gate.MaxResponseBytes()
	} else if c.maxResponseBytes > 0 {
		maxBytes = c.maxResponseBytes
	}

	reqBody := ChatRequest{
		Model: c.model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.1,
	}

	payloadBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if int64(len(bodyBytes)) > maxBytes {
		return "", fmt.Errorf("llm response body exceeds limit of %d bytes", maxBytes)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("api error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
		return "", fmt.Errorf("failed to unmarshal chat response: %w, raw: %s", err, string(bodyBytes))
	}

	if chatResp.Error != nil {
		return "", fmt.Errorf("llm returned error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("no response choices returned from model")
	}

	content := chatResp.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("empty assistant response content")
	}

	return content, nil
}
