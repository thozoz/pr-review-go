package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/thozoz/pr-review-go/pkg/config"
)

type Classification string

const (
	ClassTransient      Classification = "transient"
	ClassPermanent      Classification = "permanent"
	ClassUncertainWrite Classification = "uncertain-write"
)

type OpKind string

const (
	OpKindRead  OpKind = "read"
	OpKindWrite OpKind = "write"
)

var (
	ErrExceedsWorkerBudget = errors.New("retry wait exceeds worker budget")
	ErrUncertainWrite      = errors.New("operation result is uncertain; write will not be retried")
	ErrJobBudgetExhausted  = errors.New("retry job budget exhausted")
	ErrPermanentFailure    = errors.New("permanent failure; will not retry")
)

// ExceedsWorkerBudgetError signals that required retry wait exceeds worker capacity.
type ExceedsWorkerBudgetError struct {
	RequiredWait time.Duration
	MaxWait      time.Duration
	RetryAfter   time.Time
}

func (e *ExceedsWorkerBudgetError) Error() string {
	return fmt.Sprintf("required retry wait %v exceeds max worker wait %v", e.RequiredWait, e.MaxWait)
}

func (e *ExceedsWorkerBudgetError) Is(target error) bool {
	return target == ErrExceedsWorkerBudget
}

// Clock allows mocking time in tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Budget tracks total allowed retry wait time across operations in a job.
type Budget struct {
	mu        sync.Mutex
	total     time.Duration
	spent     time.Duration
	clock     Clock
}

func NewBudget(total time.Duration, clock Clock) *Budget {
	if clock == nil {
		clock = RealClock{}
	}
	return &Budget{
		total: total,
		clock: clock,
	}
}

func (b *Budget) Remaining() time.Duration {
	if b == nil {
		return time.Hour // unconstrained if no budget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent >= b.total {
		return 0
	}
	return b.total - b.spent
}

func (b *Budget) Deduct(d time.Duration) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spent+d > b.total {
		b.spent = b.total
		return ErrJobBudgetExhausted
	}
	b.spent += d
	return nil
}

type budgetContextKey struct{}

// WithBudget attaches a shared Budget to context.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetContextKey{}, b)
}

// BudgetFromContext returns the Budget from context, if any.
func BudgetFromContext(ctx context.Context) *Budget {
	if ctx == nil {
		return nil
	}
	if b, ok := ctx.Value(budgetContextKey{}).(*Budget); ok {
		return b
	}
	return nil
}

// Policy specifies retry limits and backoff timing.
type Policy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxWait     time.Duration
	JobBudget   time.Duration
	Clock       Clock
	JitterFn    func(d time.Duration) time.Duration
}

// DefaultPolicy builds a Policy from config defaults.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts: config.DefaultRetryMaxAttempts,
		BaseDelay:   config.DefaultRetryBaseDelay,
		MaxDelay:    config.DefaultRetryMaxDelay,
		MaxWait:     config.DefaultRetryMaxWait,
		JobBudget:   config.DefaultRetryJobBudget,
		Clock:       RealClock{},
	}
}

// PolicyFromConfig builds a Policy from loaded config.
func PolicyFromConfig(cfg *config.Config) Policy {
	p := DefaultPolicy()
	if cfg == nil {
		return p
	}
	if cfg.RetryMaxAttempts > 0 {
		p.MaxAttempts = cfg.RetryMaxAttempts
	}
	if cfg.RetryBaseDelay > 0 {
		p.BaseDelay = cfg.RetryBaseDelay
	}
	if cfg.RetryMaxDelay > 0 {
		p.MaxDelay = cfg.RetryMaxDelay
	}
	if cfg.RetryMaxWait > 0 {
		p.MaxWait = cfg.RetryMaxWait
	}
	if cfg.RetryJobBudget > 0 {
		p.JobBudget = cfg.RetryJobBudget
	}
	return p
}

type ClassifyInput struct {
	StatusCode int
	Headers    http.Header
	Body       string
	Err        error
	Kind       OpKind
}

// Classify categorizes a failure into transient, permanent, or uncertain-write.
func Classify(in ClassifyInput) Classification {
	// 1. If an error occurred on a write operation that timed out or is a network error, outcome is uncertain
	if in.Kind == OpKindWrite {
		if in.StatusCode >= 500 && in.StatusCode != http.StatusNotImplemented {
			return ClassUncertainWrite
		}
		if in.Err != nil && isTimeoutOrNetwork(in.Err) {
			return ClassUncertainWrite
		}
	}

	status := in.StatusCode
	bodyLower := strings.ToLower(in.Body)

	// If status was not directly provided, inspect error
	if status == 0 && in.Err != nil {
		status, in.Headers, bodyLower = extractErrorDetails(in.Err)
	}

	// 2. HTTP Status Code Checks
	switch status {
	case http.StatusUnauthorized, http.StatusUnprocessableEntity: // 401, 422
		return ClassPermanent

	case http.StatusNotFound: // 404
		return ClassPermanent

	case http.StatusForbidden: // 403
		// D-07: GitHub secondary rate limit markers vs auth/permission 403
		if isSecondaryRateLimit(bodyLower, in.Headers) {
			return ClassTransient
		}
		return ClassPermanent

	case http.StatusTooManyRequests: // 429
		// D-07: Permanent auth/input/quota failures never retry as transient
		if isLLMQuotaExhaustion(bodyLower) {
			return ClassPermanent
		}
		return ClassTransient

	case http.StatusNotImplemented: // 501
		return ClassPermanent

	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 500, 502, 503, 504
		if in.Kind == OpKindWrite {
			return ClassUncertainWrite
		}
		return ClassTransient
	}

	// Check if status is in 5xx range
	if status >= 500 && status <= 599 {
		if in.Kind == OpKindWrite {
			return ClassUncertainWrite
		}
		return ClassTransient
	}

	// 3. Network or Timeout errors
	if in.Err != nil {
		if isTimeoutOrNetwork(in.Err) {
			if in.Kind == OpKindWrite {
				return ClassUncertainWrite
			}
			return ClassTransient
		}
		// String analysis fallback for LLM or other unstructured error strings
		errStr := strings.ToLower(in.Err.Error())
		if isLLMQuotaExhaustion(errStr) {
			return ClassPermanent
		}
		if strings.Contains(errStr, "rate limit") || strings.Contains(errStr, "too many requests") || strings.Contains(errStr, "429") {
			return ClassTransient
		}
		if strings.Contains(errStr, "502") || strings.Contains(errStr, "503") || strings.Contains(errStr, "504") || strings.Contains(errStr, "500") {
			if in.Kind == OpKindWrite {
				return ClassUncertainWrite
			}
			return ClassTransient
		}
	}

	return ClassPermanent
}

// ClassifyError convenience helper.
func ClassifyError(err error, kind OpKind) Classification {
	if err == nil {
		return ClassPermanent
	}
	return Classify(ClassifyInput{Err: err, Kind: kind})
}

func isTimeoutOrNetwork(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "broken pipe")
}

func isSecondaryRateLimit(bodyLower string, headers http.Header) bool {
	if headers != nil && headers.Get("Retry-After") != "" {
		return true
	}
	if strings.Contains(bodyLower, "secondary rate limit") ||
		strings.Contains(bodyLower, "please wait a few minutes before you try again") ||
		strings.Contains(bodyLower, "you have exceeded a secondary rate limit") ||
		strings.Contains(bodyLower, "abuse detection mechanism") {
		return true
	}
	if headers != nil && headers.Get("X-Ratelimit-Remaining") == "0" {
		return true
	}
	return false
}

func isLLMQuotaExhaustion(textLower string) bool {
	return strings.Contains(textLower, "insufficient_quota") ||
		strings.Contains(textLower, "quota exceeded") ||
		strings.Contains(textLower, "exceeded your current quota") ||
		strings.Contains(textLower, "billing") ||
		strings.Contains(textLower, "account deactivated") ||
		strings.Contains(textLower, "credit limit")
}

func extractErrorDetails(err error) (int, http.Header, string) {
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) {
		status := 0
		var headers http.Header
		if ghErr.Response != nil {
			status = ghErr.Response.StatusCode
			headers = ghErr.Response.Header
		}
		return status, headers, strings.ToLower(ghErr.Message)
	}

	var rateErr *github.RateLimitError
	if errors.As(err, &rateErr) {
		status := 403
		var headers http.Header
		if rateErr.Response != nil {
			status = rateErr.Response.StatusCode
			headers = rateErr.Response.Header
		}
		return status, headers, strings.ToLower(rateErr.Message)
	}

	errStr := err.Error()
	// Parse status code from strings like "api error (status 429): ..."
	if idx := strings.Index(errStr, "status "); idx != -1 {
		rest := errStr[idx+7:]
		if closeParen := strings.Index(rest, ")"); closeParen != -1 {
			codeStr := rest[:closeParen]
			if code, parseErr := strconv.Atoi(codeStr); parseErr == nil {
				return code, nil, strings.ToLower(errStr)
			}
		}
	}

	return 0, nil, strings.ToLower(errStr)
}

// ParseRetryAfter parses Retry-After or X-Ratelimit-Reset header into a duration and target time.
func ParseRetryAfter(headers http.Header, clock Clock) (time.Duration, time.Time, bool) {
	if headers == nil || clock == nil {
		return 0, time.Time{}, false
	}

	now := clock.Now()

	// 1. Retry-After header (seconds or RFC1123 date)
	if val := headers.Get("Retry-After"); val != "" {
		if sec, err := strconv.Atoi(val); err == nil && sec >= 0 {
			d := time.Duration(sec) * time.Second
			return d, now.Add(d), true
		}
		if t, err := http.ParseTime(val); err == nil {
			d := t.Sub(now)
			if d < 0 {
				d = 0
			}
			return d, t, true
		}
	}

	// 2. X-Ratelimit-Reset header (Unix timestamp seconds)
	if val := headers.Get("X-Ratelimit-Reset"); val != "" {
		if sec, err := strconv.ParseInt(val, 10, 64); err == nil {
			targetTime := time.Unix(sec, 0)
			d := targetTime.Sub(now)
			if d < 0 {
				d = 0
			}
			return d, targetTime, true
		}
	}

	return 0, time.Time{}, false
}

// CalculateDelay calculates backoff with jitter and checks against bounds.
func CalculateDelay(attempt int, policy Policy, headers http.Header) (time.Duration, error) {
	clock := policy.Clock
	if clock == nil {
		clock = RealClock{}
	}

	// Check explicit Retry-After / Reset
	if d, targetTime, ok := ParseRetryAfter(headers, clock); ok {
		if policy.MaxWait > 0 && d > policy.MaxWait {
			return 0, &ExceedsWorkerBudgetError{
				RequiredWait: d,
				MaxWait:      policy.MaxWait,
				RetryAfter:   targetTime,
			}
		}
		return d, nil
	}

	// Exponential backoff: base * 2^(attempt - 1)
	base := policy.BaseDelay
	if base <= 0 {
		base = config.DefaultRetryBaseDelay
	}

	shift := attempt - 1
	if shift > 10 {
		shift = 10
	}
	delay := base * time.Duration(1<<shift)

	maxDelay := policy.MaxDelay
	if maxDelay <= 0 {
		maxDelay = config.DefaultRetryMaxDelay
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	// Jitter: + rand [0, delay/4]
	if policy.JitterFn != nil {
		delay = policy.JitterFn(delay)
	} else {
		// default jitter: up to 25% extra
		jitter := time.Duration(rand.Int63n(int64(delay / 4 + 1)))
		delay += jitter
	}

	if policy.MaxWait > 0 && delay > policy.MaxWait {
		return 0, &ExceedsWorkerBudgetError{
			RequiredWait: delay,
			MaxWait:      policy.MaxWait,
			RetryAfter:   clock.Now().Add(delay),
		}
	}

	return delay, nil
}

// Do executes an operation with bounded retry.
func Do[T any](ctx context.Context, policy Policy, kind OpKind, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if ctx == nil {
		ctx = context.Background()
	}

	clock := policy.Clock
	if clock == nil {
		clock = RealClock{}
		policy.Clock = clock
	}

	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = config.DefaultRetryMaxAttempts
	}

	budget := BudgetFromContext(ctx)
	if budget == nil && policy.JobBudget > 0 {
		budget = NewBudget(policy.JobBudget, clock)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}

		res, err := fn(ctx)
		if err == nil {
			return res, nil
		}

		lastErr = err

		// Classify error
		class := ClassifyError(err, kind)
		switch class {
		case ClassPermanent:
			return zero, err

		case ClassUncertainWrite:
			return zero, fmt.Errorf("%w: %v", ErrUncertainWrite, err)

		case ClassTransient:
			if attempt == maxAttempts {
				return zero, lastErr
			}

			// Extract headers if available from error
			_, headers, _ := extractErrorDetails(err)

			delay, delayErr := CalculateDelay(attempt, policy, headers)
			if delayErr != nil {
				return zero, delayErr
			}

			// Check and deduct budget
			if budget != nil {
				if budget.Remaining() <= 0 || delay > budget.Remaining() {
					return zero, ErrJobBudgetExhausted
				}
				if err := budget.Deduct(delay); err != nil {
					return zero, err
				}
			}

			// Sleep using clock
			if err := clock.Sleep(ctx, delay); err != nil {
				return zero, err
			}
		}
	}

	return zero, lastErr
}
