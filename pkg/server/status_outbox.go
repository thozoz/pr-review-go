package server

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
)

// StatusOutbox is a fixed, bounded background dispatcher that publishes queued status
// comments for admitted review jobs independently of business worker availability.
type StatusOutbox struct {
	store   JobStore
	gh      *ghclient.Client
	pub     *Publication
	wakeCh  chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}
	mu      sync.Mutex
	running bool
}

// NewStatusOutbox creates a new StatusOutbox.
func NewStatusOutbox(store JobStore, gh *ghclient.Client, pub *Publication) *StatusOutbox {
	if pub == nil && store != nil && gh != nil {
		pub = NewPublication(store, gh)
	}
	return &StatusOutbox{
		store:  store,
		gh:     gh,
		pub:    pub,
		wakeCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start begins the outbox dispatcher goroutine.
func (o *StatusOutbox) Start(ctx context.Context) {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return
	}
	o.running = true
	o.mu.Unlock()

	go o.dispatchLoop(ctx)
}

// Stop terminates the dispatcher loop and waits for it to exit.
func (o *StatusOutbox) Stop() {
	o.mu.Lock()
	if !o.running {
		o.mu.Unlock()
		return
	}
	o.running = false
	close(o.stopCh)
	o.mu.Unlock()

	<-o.doneCh
}

// Wake signals the outbox to immediately check for pending status intents.
func (o *StatusOutbox) Wake() {
	select {
	case o.wakeCh <- struct{}{}:
	default:
	}
}

func (o *StatusOutbox) dispatchLoop(ctx context.Context) {
	defer close(o.doneCh)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-o.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.drainBatch(ctx)
		case <-o.wakeCh:
			o.drainBatch(ctx)
		}
	}
}

func (o *StatusOutbox) drainBatch(ctx context.Context) {
	if o.store == nil || o.pub == nil || o.gh == nil {
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	intents, err := o.store.ListPendingStatusIntents(reqCtx, 32)
	if err != nil || len(intents) == 0 {
		return
	}

	for _, intent := range intents {
		select {
		case <-o.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		o.processIntent(reqCtx, intent)
	}
}

func (o *StatusOutbox) processIntent(ctx context.Context, intent *OutputIntent) {
	job, err := o.store.GetJob(ctx, intent.JobID)
	if err != nil || job == nil {
		return
	}

	// Persist version/state checks: if the job has already moved past "queued",
	// a delayed queued write must not overwrite running or terminal status.
	if job.Status != "queued" {
		intent.Status = "completed"
		_ = o.store.UpdateOutputIntent(ctx, intent)
		return
	}

	// If a status comment ID was already set on the job, mark the intent complete.
	if job.StatusCommentID > 0 {
		intent.Status = "completed"
		intent.CommentID = job.StatusCommentID
		_ = o.store.UpdateOutputIntent(ctx, intent)
		return
	}

	statusText := "⏳ Review queued; waiting for capacity"
	if strings.TrimSpace(intent.Body) != "" {
		lines := strings.Split(intent.Body, "\n\n<!--")
		if len(lines) > 0 && strings.TrimSpace(lines[0]) != "" {
			statusText = strings.TrimSpace(lines[0])
		}
	}

	commentID, pubErr := o.pub.PublishStatus(ctx, job, statusText)
	if pubErr != nil {
		log.Printf("[outbox] Failed publishing queued status for job %s: %v", job.ID, pubErr)
		return
	}

	if commentID > 0 {
		job.StatusCommentID = commentID
		_ = o.store.UpdateJob(ctx, job)
	}
}
