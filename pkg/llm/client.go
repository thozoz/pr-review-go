package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thozoz/pr-review-go/pkg/retry"
)

// Bounds for LLM request gate configuration.
const (
	DefaultLLMConcurrency      = 2
	MinLLMConcurrency          = 1
	MaxLLMConcurrency          = 16
	DefaultLLMMinInterval      = 1 * time.Second
	MinLLMMinInterval          = 1 * time.Millisecond
	MaxLLMMinInterval          = 1 * time.Minute
	DefaultLLMResponseMaxBytes = 1048576  // 1 MiB
	MinLLMResponseMaxBytes     = 65536    // 64 KiB
	MaxLLMResponseMaxBytes     = 16777216 // 16 MiB
)

// Token-budget bounds for the empty-content recovery policy.
const (
	// DefaultLLMMaxTokens is the request budget assumed when none is configured.
	// It is also the base for the single doubled-budget recovery retry.
	DefaultLLMMaxTokens = 4096
	// MaxLLMMaxTokens caps the recovery retry budget.
	MaxLLMMaxTokens = 131072
)

// Clock defines the time interface used for spacing request starts.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
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
	maxTokens        int
	retryPolicy      retry.Policy
	fallbackModels   []string
}

func NewClient(baseURL, apiKey, model string) *Client {
	baseURL = strings.TrimSuffix(baseURL, "/")
	client := &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		maxResponseBytes: DefaultLLMResponseMaxBytes,
		maxTokens:        maxTokensFromEnv(),
		retryPolicy:      retry.DefaultPolicy(),
	}
	client.SetFallbackModels(strings.Split(os.Getenv("LLM_FALLBACK_MODELS"), ","))
	return client
}

// maxTokensFromEnv returns the LLM_MAX_TOKENS request budget, or 0 when the
// variable is unset or unparseable so the provider default applies. Values above
// MaxLLMMaxTokens are clamped: an absurd budget would only be rejected by the
// provider and must never widen the bounded recovery retry.
func maxTokensFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("LLM_MAX_TOKENS"))
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > MaxLLMMaxTokens {
		return MaxLLMMaxTokens
	}
	return n
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

// SetMaxTokens configures the request token budget. Zero (or negative) leaves
// max_tokens out of the payload so the provider's default applies.
func (c *Client) SetMaxTokens(tokens int) {
	if tokens < 0 {
		tokens = 0
	}
	c.maxTokens = tokens
}

// MaxTokens returns the configured request token budget, or 0 when unset.
func (c *Client) MaxTokens() int {
	return c.maxTokens
}

// SetRetryPolicy customizes retry policy for LLM operations.
func (c *Client) SetRetryPolicy(p retry.Policy) {
	c.retryPolicy = p
}

// SetClock allows injecting a fake clock for testing.
func (c *Client) SetClock(clk retry.Clock) {
	c.retryPolicy.Clock = clk
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ReasoningContent carries an internal thinking scratchpad from reasoning
	// models. It is omitempty so request messages never send it, and it is
	// never returned to callers as a completion fallback.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float32       `json:"temperature"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
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

// ErrEmptyAssistantContent is the stable sentinel wrapped by every empty
// completion failure so callers can detect it with errors.Is.
var ErrEmptyAssistantContent = errors.New("empty assistant response content")

// EmptyAssistantContentError reports why a completion produced no publishable
// content. ReasoningContent is deliberately never returned as a fallback: it
// is an internal scratchpad that may echo prompts or private code.
type EmptyAssistantContentError struct {
	Model              string
	FinishReason       string
	HasReasoning       bool
	MaxTokensAttempted int
	Retried            bool
}

func (e *EmptyAssistantContentError) Error() string {
	reasoning := "absent"
	if e.HasReasoning {
		reasoning = "present"
	}
	attempted := "unset"
	if e.MaxTokensAttempted > 0 {
		attempted = strconv.Itoa(e.MaxTokensAttempted)
	}
	retried := ""
	if e.Retried {
		retried = " after one retry with a doubled token budget"
	}
	return fmt.Sprintf(
		"empty assistant response content%s: model %q returned no content "+
			"(finish_reason=%q, reasoning_content=%s, attempted max_tokens=%s); "+
			"raise LLM_MAX_TOKENS or use a non-reasoning model",
		retried, e.Model, e.FinishReason, reasoning, attempted,
	)
}

// Unwrap exposes the stable sentinel for errors.Is checks.
func (e *EmptyAssistantContentError) Unwrap() error { return ErrEmptyAssistantContent }

// SetFallbackModels configures an ordered, opt-in chain at the same endpoint.
// Configure before serving concurrent requests. Empty and duplicate names are ignored.
func (c *Client) SetFallbackModels(models []string) {
	seen := map[string]bool{c.model: true}
	c.fallbackModels = nil
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			c.fallbackModels = append(c.fallbackModels, model)
		}
	}
}

type fallbackHTTPError struct {
	status int
	body   string
}

func (e *fallbackHTTPError) Error() string {
	return fmt.Sprintf("api error (status %d): %s", e.status, e.body)
}

func (c *Client) ChatCompletion(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	models := append([]string{c.model}, c.fallbackModels...)
	var lastErr error
	for _, model := range models {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		result, err := c.chatCompletionModel(ctx, systemPrompt, userPrompt, model)
		if err == nil {
			return result, nil
		}
		lastErr = err
		var statusErr *fallbackHTTPError
		if !errors.As(err, &statusErr) || (statusErr.status != 429 && statusErr.status < 500) {
			return "", err
		}
	}
	return "", lastErr
}

func (c *Client) chatCompletionModel(ctx context.Context, systemPrompt, userPrompt, model string) (string, error) {
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

	return c.completeWithRecovery(ctx, model, systemPrompt, userPrompt, maxBytes)
}

// completionResult is the publishable outcome of one provider round trip.
type completionResult struct {
	Content      string
	FinishReason string
	HasReasoning bool
}

// completeWithRecovery applies the empty-content recovery policy:
//   - non-empty content is returned as-is; reasoning_content is never returned;
//   - empty content with a non-empty reasoning scratchpad means the token budget
//     was consumed while thinking, so exactly one retry is made with a doubled
//     budget (capped at MaxLLMMaxTokens). The provider's finish_reason is not
//     reliable here: reasoning exhaustion is reported as "length" by some
//     backends and as "stop" by others, and gating on "length" alone left
//     finish_reason=stop reasoning-only completions failing with no retry;
//   - any remaining empty content fails with a typed, actionable error.
func (c *Client) completeWithRecovery(ctx context.Context, model, systemPrompt, userPrompt string, maxBytes int64) (string, error) {
	baseTokens := c.maxTokens
	effectiveTokens := baseTokens
	if effectiveTokens <= 0 {
		effectiveTokens = DefaultLLMMaxTokens
	}

	result, err := c.doChatCompletion(ctx, model, systemPrompt, userPrompt, baseTokens, maxBytes)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Content) != "" {
		return result.Content, nil
	}

	if result.HasReasoning && effectiveTokens < MaxLLMMaxTokens {
		retryTokens := effectiveTokens * 2
		if retryTokens > MaxLLMMaxTokens {
			retryTokens = MaxLLMMaxTokens
		}
		if retryTokens > effectiveTokens {
			retried, retryErr := c.doChatCompletion(ctx, model, systemPrompt, userPrompt, retryTokens, maxBytes)
			if retryErr != nil {
				return "", retryErr
			}
			if strings.TrimSpace(retried.Content) != "" {
				return retried.Content, nil
			}
			finishReason := retried.FinishReason
			if finishReason == "" {
				finishReason = result.FinishReason
			}
			return "", &EmptyAssistantContentError{
				Model:              model,
				FinishReason:       finishReason,
				HasReasoning:       true,
				MaxTokensAttempted: retryTokens,
				Retried:            true,
			}
		}
	}

	return "", &EmptyAssistantContentError{
		Model:              model,
		FinishReason:       result.FinishReason,
		HasReasoning:       result.HasReasoning,
		MaxTokensAttempted: baseTokens,
	}
}

// doChatCompletion performs one logical completion (with transport-level retries
// from the client retry policy) under the caller's admission gate.
func (c *Client) doChatCompletion(ctx context.Context, model, systemPrompt, userPrompt string, maxTokens int, maxBytes int64) (completionResult, error) {
	reqBody := ChatRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.1,
	}
	if maxTokens > 0 {
		budget := maxTokens
		reqBody.MaxTokens = &budget
	}

	payloadBytes, err := json.Marshal(reqBody)
	if err != nil {
		return completionResult{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", c.baseURL)

	return retry.Do(ctx, c.retryPolicy, retry.OpKindRead, func(attemptCtx context.Context) (completionResult, error) {
		req, err := http.NewRequestWithContext(attemptCtx, "POST", url, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return completionResult{}, fmt.Errorf("failed to create http request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return completionResult{}, fmt.Errorf("http request failed: %w", err)
		}
		defer resp.Body.Close()

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return completionResult{}, fmt.Errorf("failed to read response: %w", err)
		}

		if int64(len(bodyBytes)) > maxBytes {
			return completionResult{}, fmt.Errorf("llm response body exceeds limit of %d bytes", maxBytes)
		}

		if resp.StatusCode >= 400 {
			return completionResult{}, &fallbackHTTPError{status: resp.StatusCode, body: string(bodyBytes)}
		}

		var chatResp ChatResponse
		if err := json.Unmarshal(bodyBytes, &chatResp); err != nil {
			return completionResult{}, fmt.Errorf("failed to unmarshal chat response: %w, raw: %s", err, string(bodyBytes))
		}

		if chatResp.Error != nil {
			return completionResult{}, fmt.Errorf("llm returned error: %s", chatResp.Error.Message)
		}

		if len(chatResp.Choices) == 0 {
			return completionResult{}, fmt.Errorf("no response choices returned from model")
		}

		choice := chatResp.Choices[0]
		return completionResult{
			Content:      choice.Message.Content,
			FinishReason: choice.FinishReason,
			HasReasoning: strings.TrimSpace(choice.Message.ReasoningContent) != "",
		}, nil
	})
}
