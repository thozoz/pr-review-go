package sandbox

import (
	"context"
	"fmt"
	"sync"
)

// AdmissionGate limits concurrent isolated snapshot lifecycles.
type AdmissionGate interface {
	Acquire(ctx context.Context) (func(), error)
	Concurrency() int
}

// DefaultAdmissionGate manages snapshot permits using a buffered channel.
type DefaultAdmissionGate struct {
	concurrency int
	permits     chan struct{}
}

// NewAdmissionGate creates a new AdmissionGate validating bounds (1-16).
func NewAdmissionGate(concurrency int) (*DefaultAdmissionGate, error) {
	if concurrency < 1 || concurrency > 16 {
		return nil, fmt.Errorf("invalid sandbox concurrency %d: must be between 1 and 16", concurrency)
	}
	permits := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		permits <- struct{}{}
	}
	return &DefaultAdmissionGate{
		concurrency: concurrency,
		permits:     permits,
	}, nil
}

func (g *DefaultAdmissionGate) Concurrency() int {
	return g.concurrency
}

func (g *DefaultAdmissionGate) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.permits:
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			g.permits <- struct{}{}
		})
	}
	return release, nil
}

type contextKey string

const (
	admissionContextKey contextKey = "sandbox.admission_gate"
	activePermitKey     contextKey = "sandbox.active_permit"
)

// WithAdmission injects an AdmissionGate into the context.
func WithAdmission(ctx context.Context, gate AdmissionGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, admissionContextKey, gate)
}

// GateFromContext retrieves an AdmissionGate from the context if present.
func GateFromContext(ctx context.Context) AdmissionGate {
	if ctx == nil {
		return nil
	}
	if g, ok := ctx.Value(admissionContextKey).(AdmissionGate); ok {
		return g
	}
	return nil
}

// WithActivePermit returns a context indicating that a snapshot admission permit is already held.
func WithActivePermit(ctx context.Context) context.Context {
	return context.WithValue(ctx, activePermitKey, true)
}

// HasActivePermit checks if the context already holds a snapshot admission permit.
func HasActivePermit(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	active, _ := ctx.Value(activePermitKey).(bool)
	return active
}
