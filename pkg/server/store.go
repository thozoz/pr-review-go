package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ghclient "github.com/thozoz/pr-review-go/pkg/github"
	"go.etcd.io/bbolt"
)

var (
	ErrUnknownSchemaVersion = errors.New("unknown store schema version")
	ErrStoreClosed          = errors.New("job store is closed")
	ErrJobNotFound          = errors.New("job not found")
	ErrDeliveryNotFound     = errors.New("delivery not found")
	ErrPRNotFound           = errors.New("pr state not found")
	ErrIntentNotFound       = errors.New("output intent not found")
	ErrDatabaseLocked       = errors.New("database is locked by another process")
	ErrGenerationChanged    = errors.New("pr generation changed before publication outcome commit")
)

const (
	StoreSchemaVersion = 1

	MaxJobBytes        = 32 * 1024  // 32 KiB normalized job bound
	MaxReviewBodyBytes = 128 * 1024 // 128 KiB review output bound

	// EstimatedJobStorageReserve is the logical capacity reservation per job
	EstimatedJobStorageReserve = int64(MaxJobBytes + MaxReviewBodyBytes)
)

var (
	bucketMeta       = []byte("meta")
	bucketDeliveries = []byte("deliveries")
	bucketJobs       = []byte("jobs")
	bucketPRs        = []byte("prs")
	bucketIntents    = []byte("intents")
	bucketCounters   = []byte("counters")
	bucketComments   = []byte("comments")
	bucketDecisions  = []byte("decisions")

	keySchemaVersion = []byte("schema_version")
)

// PRKey represents the unique identification for PR actions:
// canonical API host + base repository ID + PR number.
type PRKey struct {
	Host   string `json:"host"`
	RepoID int64  `json:"repo_id"`
	Number int    `json:"number"`
}

func (k PRKey) String() string {
	return fmt.Sprintf("%s/%d/%d", k.Host, k.RepoID, k.Number)
}

func MakePRKey(host string, repoID int64, number int) (PRKey, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return PRKey{}, fmt.Errorf("host must not be empty")
	}
	if repoID <= 0 {
		return PRKey{}, fmt.Errorf("repoID must be positive, got %d", repoID)
	}
	if number <= 0 {
		return PRKey{}, fmt.Errorf("pr number must be positive, got %d", number)
	}
	return PRKey{
		Host:   host,
		RepoID: repoID,
		Number: number,
	}, nil
}

// Delivery records an authenticated incoming webhook delivery receipt.
type Delivery struct {
	Host        string    `json:"host"`
	RepoID      int64     `json:"repo_id"`
	DeliveryID  string    `json:"delivery_id"`
	EventKind   string    `json:"event_kind"`
	PayloadHash string    `json:"payload_hash"` // SHA-256 hex
	ReceivedAt  time.Time `json:"received_at"`
	JobIDs      []string  `json:"job_ids,omitempty"`
}

func (d *Delivery) Key() string {
	return fmt.Sprintf("%s/%d/%s", d.Host, d.RepoID, d.DeliveryID)
}

// Job represents a unit of work admitted by the durable ledger.
type Job struct {
	ID              string     `json:"id"`
	Sequence        uint64     `json:"sequence"`
	Kind            string     `json:"kind"`    // "review", "labels", "describe", "summary", "docs", "changelog", "improve", "assistant", "edit"
	Trigger         string     `json:"trigger"` // "automatic", "explicit"
	Author          string     `json:"author"`
	CommentID       int64      `json:"comment_id,omitempty"`
	PRKey           PRKey      `json:"pr_key"`
	Owner           string     `json:"owner"`
	Repo            string     `json:"repo"`
	PRNumber        int        `json:"pr_number"`
	BaseSHA         string     `json:"base_sha"`
	HeadSHA         string     `json:"head_sha"`
	Generation      uint64     `json:"generation"`
	Status          string     `json:"status"` // "queued", "running", "completed", "failed", "superseded", "uncertain", "needs_attention"
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	RecoveryPhase   string     `json:"recovery_phase,omitempty"`
	Reservations    int64      `json:"reservations"`
	StatusCommentID int64      `json:"status_comment_id,omitempty"`
	OutputMarker    string     `json:"output_marker,omitempty"`
	// NotBeforeAt parks a deferred attempt in the durable queue until the due
	// time arrives. Zero value means immediately eligible; old rows unmarshal
	// unchanged and schema version stays 1.
	NotBeforeAt     time.Time  `json:"not_before_at,omitempty"`
	Payload         string     `json:"payload,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// OutputIntent records intent before external GitHub mutations.
type OutputIntent struct {
	Marker        string `json:"marker"`
	JobID         string `json:"job_id"`
	Action        string `json:"action"`
	PRKey         PRKey  `json:"pr_key"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	PRNumber      int    `json:"pr_number"`
	ExactHead     string `json:"exact_head"`
	BodyDigest    string `json:"body_digest"` // hex SHA-256
	Body          string `json:"body"`
	VerifiedActor string `json:"verified_actor"`
	Status        string `json:"status"` // "pending", "in_progress", "completed", "uncertain", "needs_attention"
	CommentID     int64  `json:"comment_id,omitempty"`
	// SupersededBody/SupersededDigest record the intended updated body of an owned
	// comment before it is edited as superseded, so restart can match either the
	// original or the updated owned content (additive, optional; schema stays v1).
	SupersededBody   string    `json:"superseded_body,omitempty"`
	SupersededDigest string    `json:"superseded_digest,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PRState tracks PR-level generation and active execution.
type PRState struct {
	PRKey                PRKey  `json:"pr_key"`
	Owner                string `json:"owner"`
	Repo                 string `json:"repo"`
	Number               int    `json:"number"`
	Generation           uint64 `json:"generation"`
	LastReviewedHead     string `json:"last_reviewed_head,omitempty"`
	ActiveJobID          string `json:"active_job_id,omitempty"`
	HasReservedSuccessor bool   `json:"has_reserved_successor,omitempty"`
	PendingAutoJobID     string `json:"pending_auto_job_id,omitempty"`
	LatestHeadSHA        string `json:"latest_head_sha,omitempty"`
	HasBlockedAction     bool   `json:"has_blocked_action,omitempty"`
	BlockedReason        string `json:"blocked_reason,omitempty"`
	// LastBotCommitSHA records the commit OID created by the most recent
	// successful edit push on this PR, so the next edit job can surface the
	// prior bot diff as sticky context. Empty when no bot edit landed yet.
	LastBotCommitSHA string `json:"last_bot_commit_sha,omitempty"`
	// LastBotBaseSHA records the PR head the last bot edit applied to.
	LastBotBaseSHA string `json:"last_bot_base_sha,omitempty"`
	// EditHistory carries the most recent user edit instructions (oldest
	// first), capped at MaxEditHistoryEntries with oldest dropped on
	// overflow. Additive omitempty evolution; schema version stays 1.
	EditHistory []string `json:"edit_history,omitempty"`
}

// MaxEditHistoryEntries caps PRState.EditHistory; overflow drops oldest.
const MaxEditHistoryEntries = 5

// appendEditHistory appends instruction and drops oldest entries beyond the cap.
func appendEditHistory(history []string, instruction string) []string {
	history = append(history, instruction)
	if len(history) > MaxEditHistoryEntries {
		history = history[len(history)-MaxEditHistoryEntries:]
	}
	return history
}

type HealthCounts struct {
	Queued         int  `json:"queued"`
	Running        int  `json:"running"`
	Uncertain      int  `json:"uncertain"`
	NeedsAttention int  `json:"needs_attention"`
	Deferred       int  `json:"deferred"`
	OldWaitWarning bool `json:"old_wait_warning"`
}

type AdmitStatus int

const (
	AdmitAccepted AdmitStatus = iota
	AdmitDuplicate
	AdmitCollision
	AdmitCapacityFull
)

type AdmitResult struct {
	Status AdmitStatus
	Reason string
}

type StoreOptions struct {
	BacklogLimit  int
	DeliveryLimit int
	StateMaxBytes int64
	DeliveryTTL   time.Duration
	OpenTimeout   time.Duration
}

// JobStore defines the storage boundary interface.
type JobStore interface {
	Close() error
	Admit(ctx context.Context, delivery Delivery, jobs []Job) (AdmitResult, error)
	GetDelivery(ctx context.Context, key string) (*Delivery, error)
	GetJob(ctx context.Context, id string) (*Job, error)
	ListQueuedJobs(ctx context.Context) ([]*Job, error)
	UpdateJob(ctx context.Context, job *Job) error
	GetPRState(ctx context.Context, key PRKey) (*PRState, error)
	UpdatePRState(ctx context.Context, state *PRState) error
	SaveOutputIntent(ctx context.Context, intent *OutputIntent) error
	GetOutputIntent(ctx context.Context, marker string) (*OutputIntent, error)
	UpdateOutputIntent(ctx context.Context, intent *OutputIntent) error
	// CommitPublicationOutcome atomically commits intent, job and (for confirmed
	// current-head review publication) PRState.LastReviewedHead in one transaction.
	CommitPublicationOutcome(ctx context.Context, intent *OutputIntent, job *Job, prState *PRState) error
	// CommitSupersededOutcome atomically commits a superseded outcome and the
	// latest-head successor obligation (when successor head is non-empty).
	CommitSupersededOutcome(ctx context.Context, intent *OutputIntent, job *Job, successorBaseSHA, successorHeadSHA string) error
	RecoverInterruptedJobs(ctx context.Context) ([]*Job, error)
	RecoverJobs(ctx context.Context) ([]*Job, error)
	InspectJobs(ctx context.Context) ([]*Job, error)
	ResolveJob(ctx context.Context, jobID string, resolution string, ackDuplicateRisk bool) error
	GetHealthCounts(ctx context.Context, oldWaitWarning time.Duration) (HealthCounts, error)
	ClaimNextJob(ctx context.Context, activePRs map[string]bool) (*Job, error)
	ReleasePR(ctx context.Context, prKey PRKey) error
	ScheduleSuccessorReview(ctx context.Context, prKey PRKey, owner, repo string, prNum int, baseSHA, headSHA string) (*Job, error)
	// DeferJob atomically parks a job as a deferred attempt due at dueAt: the
	// job returns to queued with NotBeforeAt set so workers are freed while
	// the wait persists durably. Repeating the same deferral is idempotent.
	DeferJob(ctx context.Context, jobID string, dueAt time.Time, reason string) (*Job, error)
	// EarliestDeferredDue reports the earliest future due time among queued
	// deferred attempts, so the scheduler can bound its idle poll.
	EarliestDeferredDue(ctx context.Context) (time.Time, bool, error)
	ListPendingStatusIntents(ctx context.Context, limit int) ([]*OutputIntent, error)
	MaintainTerminalRecords(ctx context.Context, now time.Time, limit int) (MaintenanceResult, error)
}

type BoltJobStore struct {
	db           *bbolt.DB
	opts         StoreOptions
	mu           sync.RWMutex
	mBucketIndex int
	mCursorKey   []byte
}

// OpenJobStore opens or initializes a bbolt-backed JobStore at the specified path.
func OpenJobStore(path string, opts StoreOptions) (*BoltJobStore, error) {
	if opts.BacklogLimit <= 0 {
		opts.BacklogLimit = 100
	}
	if opts.DeliveryLimit <= 0 {
		opts.DeliveryLimit = 20000
	}
	if opts.StateMaxBytes <= 0 {
		opts.StateMaxBytes = 67108864
	}
	if opts.DeliveryTTL <= 0 {
		opts.DeliveryTTL = 168 * time.Hour
	}
	if opts.OpenTimeout <= 0 {
		opts.OpenTimeout = 1 * time.Second
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	boltOpts := &bbolt.Options{
		Timeout: opts.OpenTimeout,
	}

	db, err := bbolt.Open(path, 0600, boltOpts)
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return nil, ErrDatabaseLocked
		}
		return nil, fmt.Errorf("failed to open bolt database: %w", err)
	}

	store := &BoltJobStore{
		db:   db,
		opts: opts,
	}

	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *BoltJobStore) initSchema() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		metaBucket := tx.Bucket(bucketMeta)
		if metaBucket == nil {
			// Brand new database: create buckets and set schema version
			b, err := tx.CreateBucket(bucketMeta)
			if err != nil {
				return err
			}
			versionBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(versionBytes, uint32(StoreSchemaVersion))
			if err := b.Put(keySchemaVersion, versionBytes); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketDeliveries); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketJobs); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketPRs); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketIntents); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketCounters); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketComments); err != nil {
				return err
			}
			if _, err := tx.CreateBucketIfNotExists(bucketDecisions); err != nil {
				return err
			}
			return nil
		}

		// Existing database: check schema version. Reject unknown schema without rewriting.
		verBytes := metaBucket.Get(keySchemaVersion)
		if len(verBytes) < 4 {
			return fmt.Errorf("%w: invalid schema version format", ErrUnknownSchemaVersion)
		}
		ver := binary.BigEndian.PutUint32
		_ = ver
		version := binary.BigEndian.Uint32(verBytes)
		if version != StoreSchemaVersion {
			return fmt.Errorf("%w: found version %d, expected %d", ErrUnknownSchemaVersion, version, StoreSchemaVersion)
		}

		// Ensure all buckets exist
		for _, bName := range [][]byte{bucketDeliveries, bucketJobs, bucketPRs, bucketIntents, bucketCounters, bucketComments, bucketDecisions} {
			if _, err := tx.CreateBucketIfNotExists(bName); err != nil {
				return err
			}
		}

		// Repair missing queued status intents for legacy queued review jobs
		jobsB := tx.Bucket(bucketJobs)
		intentsB := tx.Bucket(bucketIntents)
		if jobsB != nil && intentsB != nil {
			c := jobsB.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var j Job
				if err := json.Unmarshal(v, &j); err != nil {
					continue
				}
				if j.Status == "queued" && (j.Kind == "review" || j.Kind == "approve" || j.Kind == "request_changes") {
					marker := fmt.Sprintf("<!-- pr-review-status:%s -->", j.ID)
					if intentsB.Get([]byte(marker)) == nil {
						bodyWithMarker := fmt.Sprintf("⏳ Review queued; waiting for capacity\n\n%s", marker)
						h := sha256.Sum256([]byte(bodyWithMarker))
						statusIntent := OutputIntent{
							Marker:     marker,
							JobID:      j.ID,
							Action:     "status",
							PRKey:      j.PRKey,
							Owner:      j.Owner,
							Repo:       j.Repo,
							PRNumber:   j.PRNumber,
							ExactHead:  j.HeadSHA,
							Body:       bodyWithMarker,
							BodyDigest: hex.EncodeToString(h[:]),
							Status:     "pending",
							CreatedAt:  time.Now().UTC(),
							UpdatedAt:  time.Now().UTC(),
						}
						if raw, err := json.Marshal(&statusIntent); err == nil {
							_ = intentsB.Put([]byte(marker), raw)
						}
					}
				}
			}
		}
		return nil
	})
}

// DB returns the underlying bbolt.DB instance.
func (s *BoltJobStore) DB() *bbolt.DB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db
}

func (s *BoltJobStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Admit atomically checks delivery uniqueness and capacity, enqueues jobs,
// records receipt, and increments PR generation in a single transaction.
func (s *BoltJobStore) Admit(ctx context.Context, delivery Delivery, jobs []Job) (AdmitResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return AdmitResult{}, ErrStoreClosed
	}

	// Validate job bounds prior to admission
	for _, j := range jobs {
		raw, err := json.Marshal(j)
		if err != nil {
			return AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("malformed job: %v", err)}, nil
		}
		if len(raw) > MaxJobBytes {
			return AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("job payload size %d exceeds 32 KiB bound", len(raw))}, nil
		}
		// Edit jobs carry the CAS base for the gated push path (D-31): both
		// SHAs must be present and well-formed at admission, failing closed
		// before any queue state is touched. Other kinds keep their existing
		// admission contract unchanged.
		if j.Kind == "edit" {
			if err := ghclient.ValidateCommitOID(j.HeadSHA); err != nil {
				return AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("edit job requires valid head SHA: %v", err)}, nil
			}
			if err := ghclient.ValidateCommitOID(j.BaseSHA); err != nil {
				return AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("edit job requires valid base SHA: %v", err)}, nil
			}
		}
	}

	var result AdmitResult
	err := s.db.Update(func(tx *bbolt.Tx) error {
		delivBucket := tx.Bucket(bucketDeliveries)
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)
		countersBucket := tx.Bucket(bucketCounters)
		commentsBucket := tx.Bucket(bucketComments)

		// 0. Check comment uniqueness (one commentID is one request)
		for _, j := range jobs {
			if j.CommentID > 0 {
				commentKey := fmt.Sprintf("%d/%d/created", j.PRKey.RepoID, j.CommentID)
				if commentsBucket.Get([]byte(commentKey)) != nil {
					result = AdmitResult{Status: AdmitDuplicate, Reason: "comment already processed"}
					return nil
				}
			}
		}

		// 1. Check Delivery uniqueness / collision
		delivKey := []byte(delivery.Key())
		if existingBytes := delivBucket.Get(delivKey); existingBytes != nil {
			var existing Delivery
			if err := json.Unmarshal(existingBytes, &existing); err == nil {
				if existing.PayloadHash == delivery.PayloadHash {
					result = AdmitResult{Status: AdmitDuplicate, Reason: "delivery already processed"}
					return nil
				}
				result = AdmitResult{Status: AdmitCollision, Reason: "delivery ID reused with changed payload"}
				return nil
			}
		}

		now := delivery.ReceivedAt
		if now.IsZero() {
			now = time.Now().UTC()
		}

		// 2. Check Delivery limit
		delivCount := delivBucket.Stats().KeyN
		if delivCount+1 > s.opts.DeliveryLimit {
			var mRes MaintenanceResult
			intentsBucket := tx.Bucket(bucketIntents)
			_ = s.maintainTerminalRecordsInTx(tx, delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket, now, s.opts.DeliveryTTL, 128, &mRes)
			delivCount = 0
			c := delivBucket.Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				delivCount++
			}
			if delivCount+1 > s.opts.DeliveryLimit {
				result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("delivery limit %d reached", s.opts.DeliveryLimit)}
				return nil
			}
		}

		// 3. Count currently queued jobs, reserved successors, and estimated bytes
		queuedCount := 0
		reservedSuccessorCount := 0
		estimatedBytes := int64(0)
		cursor := jobsBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				estimatedBytes += int64(len(v)) + j.Reservations
				if j.Status == "queued" {
					queuedCount++
				}
			}
		}

		prCursor := prsBucket.Cursor()
		for k, v := prCursor.First(); k != nil; k, v = prCursor.Next() {
			var st PRState
			if err := json.Unmarshal(v, &st); err == nil {
				if st.HasReservedSuccessor && st.ActiveJobID != "" {
					reservedSuccessorCount++
					estimatedBytes += EstimatedJobStorageReserve
				}
			}
		}

		// Check which jobs can coalesce or use reserved successor
		netNewQueued := 0
		type coalescedTarget struct {
			key []byte
			job Job
		}
		coalescedJobs := make(map[int]coalescedTarget) // index in jobs -> target
		usesReservedSlot := make(map[int]bool)

		for i, j := range jobs {
			if j.Trigger == "automatic" && j.Kind == "review" {
				prKeyStr := j.PRKey.String()
				var state PRState
				if prBytes := prsBucket.Get([]byte(prKeyStr)); prBytes != nil {
					_ = json.Unmarshal(prBytes, &state)
				}

				// Check if there is an existing queued automatic review for this PR
				foundExisting := false
				c := jobsBucket.Cursor()
				for k, v := c.First(); k != nil; k, v = c.Next() {
					var qj Job
					if err := json.Unmarshal(v, &qj); err == nil {
						if qj.PRKey == j.PRKey && qj.Trigger == "automatic" && qj.Kind == "review" && qj.Status == "queued" {
							coalescedJobs[i] = coalescedTarget{
								key: append([]byte(nil), k...),
								job: qj,
							}
							foundExisting = true
							break
						}
					}
				}

				if !foundExisting {
					if state.ActiveJobID != "" && state.HasReservedSuccessor {
						// Uses the already reserved successor slot
						usesReservedSlot[i] = true
					} else {
						netNewQueued++
					}
				}
			} else {
				netNewQueued++
			}
		}

		// Enforce Backlog capacity (only for net new jobs not using reservations or coalescing)
		if queuedCount+netNewQueued > s.opts.BacklogLimit {
			result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("queue backlog capacity %d reached", s.opts.BacklogLimit)}
			return nil
		}

		// 4. Check State Max Bytes (logical retained values + reservations)
		neededBytes := int64(netNewQueued) * EstimatedJobStorageReserve
		if estimatedBytes+neededBytes > s.opts.StateMaxBytes {
			var mRes MaintenanceResult
			intentsBucket := tx.Bucket(bucketIntents)
			_ = s.maintainTerminalRecordsInTx(tx, delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket, now, s.opts.DeliveryTTL, 128, &mRes)
			estimatedBytes = 0
			c := jobsBucket.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var j Job
				if err := json.Unmarshal(v, &j); err == nil {
					estimatedBytes += int64(len(v)) + j.Reservations
				}
			}
			if estimatedBytes+neededBytes > s.opts.StateMaxBytes {
				result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("state max bytes limit %d reached", s.opts.StateMaxBytes)}
				return nil
			}
		}

		// 5. Defer Delivery commit until jobs are assigned IDs below
		prGenerations := make(map[string]uint64)
		for i := range jobs {
			job := &jobs[i]
			prKeyStr := job.PRKey.String()

			gen, exists := prGenerations[prKeyStr]
			if !exists {
				// Fetch current PR state
				prStateKey := []byte(prKeyStr)
				var state PRState
				if prBytes := prsBucket.Get(prStateKey); prBytes != nil {
					_ = json.Unmarshal(prBytes, &state)
				} else {
					state = PRState{
						PRKey:  job.PRKey,
						Owner:  job.Owner,
						Repo:   job.Repo,
						Number: job.PRNumber,
					}
				}
				state.Generation++
				gen = state.Generation
				prGenerations[prKeyStr] = gen

				if job.HeadSHA != "" {
					state.LatestHeadSHA = job.HeadSHA
				}

				updatedPRBytes, err := json.Marshal(state)
				if err != nil {
					return err
				}
				if err := prsBucket.Put(prStateKey, updatedPRBytes); err != nil {
					return err
				}
			}

			// If this job coalesces into an existing queued job:
			if target, ok := coalescedJobs[i]; ok {
				target.job.HeadSHA = job.HeadSHA
				target.job.BaseSHA = job.BaseSHA
				target.job.Generation = gen
				jobBytes, err := json.Marshal(target.job)
				if err != nil {
					return err
				}
				if err := jobsBucket.Put(target.key, jobBytes); err != nil {
					return err
				}

				// Update PR state pending auto job ID
				prStateKey := []byte(prKeyStr)
				var state PRState
				if prBytes := prsBucket.Get(prStateKey); prBytes != nil {
					_ = json.Unmarshal(prBytes, &state)
					state.PendingAutoJobID = target.job.ID
					state.LatestHeadSHA = job.HeadSHA
					if updatedPRBytes, err := json.Marshal(state); err == nil {
						_ = prsBucket.Put(prStateKey, updatedPRBytes)
					}
				}

				// Coalescing updates that same queued intent/head instead of creating another status record
				marker := fmt.Sprintf("<!-- pr-review-status:%s -->", target.job.ID)
				if raw := tx.Bucket(bucketIntents).Get([]byte(marker)); raw != nil {
					var in OutputIntent
					if err := json.Unmarshal(raw, &in); err == nil {
						in.ExactHead = job.HeadSHA
						in.UpdatedAt = time.Now().UTC()
						if updatedRaw, err := json.Marshal(&in); err == nil {
							_ = tx.Bucket(bucketIntents).Put([]byte(marker), updatedRaw)
						}
					}
				}
				continue
			}

			seq, err := countersBucket.NextSequence()
			if err != nil {
				return err
			}

			job.Sequence = seq
			job.ID = fmt.Sprintf("job-%08d", seq)
			job.Generation = gen
			job.Status = "queued"
			job.CreatedAt = time.Now().UTC()
			job.Reservations = EstimatedJobStorageReserve

			jobBytes, err := json.Marshal(job)
			if err != nil {
				return err
			}

			jobKey := make([]byte, 8)
			binary.BigEndian.PutUint64(jobKey, seq)
			if err := jobsBucket.Put(jobKey, jobBytes); err != nil {
				return err
			}

			if job.Kind == "review" || job.Kind == "approve" || job.Kind == "request_changes" {
				marker := fmt.Sprintf("<!-- pr-review-status:%s -->", job.ID)
				bodyWithMarker := fmt.Sprintf("⏳ Review queued; waiting for capacity\n\n%s", marker)
				h := sha256.Sum256([]byte(bodyWithMarker))
				statusIntent := OutputIntent{
					Marker:     marker,
					JobID:      job.ID,
					Action:     "status",
					PRKey:      job.PRKey,
					Owner:      job.Owner,
					Repo:       job.Repo,
					PRNumber:   job.PRNumber,
					ExactHead:  job.HeadSHA,
					Body:       bodyWithMarker,
					BodyDigest: hex.EncodeToString(h[:]),
					Status:     "pending",
					CreatedAt:  time.Now().UTC(),
					UpdatedAt:  time.Now().UTC(),
				}
				intentRaw, err := json.Marshal(&statusIntent)
				if err != nil {
					return err
				}
				if err := tx.Bucket(bucketIntents).Put([]byte(marker), intentRaw); err != nil {
					return err
				}
			}

			if job.Trigger == "automatic" && job.Kind == "review" {
				prStateKey := []byte(prKeyStr)
				var state PRState
				if prBytes := prsBucket.Get(prStateKey); prBytes != nil {
					_ = json.Unmarshal(prBytes, &state)
					state.PendingAutoJobID = job.ID
					state.LatestHeadSHA = job.HeadSHA
					if usesReservedSlot[i] {
						state.HasReservedSuccessor = true
					}
					if updatedPRBytes, err := json.Marshal(state); err == nil {
						_ = prsBucket.Put(prStateKey, updatedPRBytes)
					}
				}
			}
		}

		// 7. Commit Delivery receipt with associated job IDs
		var admittedJobIDs []string
		for _, j := range jobs {
			if j.ID != "" {
				admittedJobIDs = append(admittedJobIDs, j.ID)
			}
		}
		for _, target := range coalescedJobs {
			if target.job.ID != "" {
				admittedJobIDs = append(admittedJobIDs, target.job.ID)
			}
		}
		delivery.JobIDs = admittedJobIDs
		delivBytes, err := json.Marshal(delivery)
		if err != nil {
			return err
		}
		if err := delivBucket.Put(delivKey, delivBytes); err != nil {
			return err
		}

		// 8. Register comment keys
		for _, j := range jobs {
			if j.CommentID > 0 {
				commentKey := fmt.Sprintf("%d/%d/created", j.PRKey.RepoID, j.CommentID)
				_ = commentsBucket.Put([]byte(commentKey), []byte(j.ID))
			}
		}

		result = AdmitResult{Status: AdmitAccepted, Reason: "admitted"}
		return nil
	})

	if err != nil {
		return AdmitResult{}, err
	}
	return result, nil
}

func (s *BoltJobStore) GetDelivery(ctx context.Context, key string) (*Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var deliv *Delivery
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDeliveries)
		bytes := b.Get([]byte(key))
		if bytes == nil {
			return ErrDeliveryNotFound
		}
		var d Delivery
		if err := json.Unmarshal(bytes, &d); err != nil {
			return err
		}
		deliv = &d
		return nil
	})
	return deliv, err
}

func (s *BoltJobStore) GetJob(ctx context.Context, id string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var found *Job
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				if j.ID == id {
					copyJob := j
					found = &copyJob
					return nil
				}
			}
		}
		return ErrJobNotFound
	})
	return found, err
}

func (s *BoltJobStore) ListQueuedJobs(ctx context.Context) ([]*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var list []*Job
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				if j.Status == "queued" {
					copyJob := j
					list = append(list, &copyJob)
				}
			}
		}
		return nil
	})
	return list, err
}

func (s *BoltJobStore) UpdateJob(ctx context.Context, job *Job) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		jobKey := make([]byte, 8)
		binary.BigEndian.PutUint64(jobKey, job.Sequence)

		if b.Get(jobKey) == nil {
			// Find by ID if sequence lookup fails
			c := b.Cursor()
			found := false
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var j Job
				if err := json.Unmarshal(v, &j); err == nil && j.ID == job.ID {
					copy(jobKey, k)
					found = true
					break
				}
			}
			if !found {
				return ErrJobNotFound
			}
		}

		raw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		return b.Put(jobKey, raw)
	})
}

func (s *BoltJobStore) GetPRState(ctx context.Context, key PRKey) (*PRState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var state *PRState
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPRs)
		v := b.Get([]byte(key.String()))
		if v == nil {
			return ErrPRNotFound
		}
		var st PRState
		if err := json.Unmarshal(v, &st); err != nil {
			return err
		}
		state = &st
		return nil
	})
	return state, err
}

func (s *BoltJobStore) UpdatePRState(ctx context.Context, state *PRState) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPRs)
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return b.Put([]byte(state.PRKey.String()), raw)
	})
}

func (s *BoltJobStore) SaveOutputIntent(ctx context.Context, intent *OutputIntent) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	if len(intent.Body) > MaxReviewBodyBytes {
		return fmt.Errorf("intent body size %d exceeds 128 KiB limit", len(intent.Body))
	}

	if intent.BodyDigest == "" && intent.Body != "" {
		h := sha256.Sum256([]byte(intent.Body))
		intent.BodyDigest = hex.EncodeToString(h[:])
	}
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = time.Now().UTC()
	}
	intent.UpdatedAt = time.Now().UTC()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIntents)
		raw, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		return b.Put([]byte(intent.Marker), raw)
	})
}

func (s *BoltJobStore) GetOutputIntent(ctx context.Context, marker string) (*OutputIntent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var intent *OutputIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIntents)
		v := b.Get([]byte(marker))
		if v == nil {
			return ErrIntentNotFound
		}
		var it OutputIntent
		if err := json.Unmarshal(v, &it); err != nil {
			return err
		}
		intent = &it
		return nil
	})
	return intent, err
}

func (s *BoltJobStore) UpdateOutputIntent(ctx context.Context, intent *OutputIntent) error {
	return s.SaveOutputIntent(ctx, intent)
}

// ListPendingStatusIntents returns up to limit pending status output intents.
func (s *BoltJobStore) ListPendingStatusIntents(ctx context.Context, limit int) ([]*OutputIntent, error) {
	if limit <= 0 {
		limit = 32
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var results []*OutputIntent
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIntents)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var in OutputIntent
			if err := json.Unmarshal(v, &in); err != nil {
				continue
			}
			if in.Action == "status" && in.Status == "pending" {
				copyIn := in
				results = append(results, &copyIn)
				if len(results) >= limit {
					break
				}
			}
		}
		return nil
	})
	return results, err
}

// RecoverJobs inspects uncompleted jobs on startup and sets their recovery phase:
// - Review with saved output intent: queued for ReconcileOutput without regeneration.
// - Interrupted review generation: requeued for generation.
// - Idempotent actions (labels): requeued for idempotent set-add.
// - Static actions (improve): requeued with owned intent.
// - Non-idempotent actions (describe, summary, docs, changelog, assistant, edit): marked needs_attention if unproven.
// - Uncertain and needs_attention jobs release worker slots while blocking subsequent PR actions.
func (s *BoltJobStore) RecoverJobs(ctx context.Context) ([]*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var recovered []*Job
	err := s.db.Update(func(tx *bbolt.Tx) error {
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)
		intentsBucket := tx.Bucket(bucketIntents)

		c := jobsBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}

			if j.Status == "running" {
				switch j.Kind {
				case "review":
					marker := fmt.Sprintf("<!-- pr-review-output:%s -->", j.ID)
					hasSavedOutput := false
					if itBytes := intentsBucket.Get([]byte(marker)); itBytes != nil {
						var it OutputIntent
						if err := json.Unmarshal(itBytes, &it); err == nil && it.Body != "" {
							hasSavedOutput = true
						}
					}

					if hasSavedOutput {
						j.Status = "queued"
						j.RecoveryPhase = "reconcile_output"
					} else {
						j.Status = "queued"
						j.RecoveryPhase = "requeued_after_interrupt"
					}

				case "labels":
					j.Status = "queued"
					j.RecoveryPhase = "requeued_idempotent"

				case "improve":
					j.Status = "queued"
					j.RecoveryPhase = "requeued_static"

				case "describe", "summary", "docs", "changelog", "assistant", "edit":
					// Non-idempotent action, routed explicitly per kind (no silent
					// default swallow for edit). Edit jobs perform remote writes
					// (status comments plus the gated push), so an interrupted edit
					// can never prove its external outcome and parks as
					// needs_attention for operator resolution via --queue-resolve.
					outputProven := false
					marker := fmt.Sprintf("<!-- pr-%s-output:%s -->", j.Kind, j.ID)
					if itBytes := intentsBucket.Get([]byte(marker)); itBytes != nil {
						var it OutputIntent
						if err := json.Unmarshal(itBytes, &it); err == nil && it.Status == "completed" {
							outputProven = true
						}
					}

					if outputProven {
						j.Status = "completed"
					} else {
						j.Status = "needs_attention"
						j.RecoveryPhase = "interrupted_unproven"
						j.Error = "interrupted action cannot prove external outcome; requires operator resolution"
					}

				default:
					// Unknown future kinds fail closed: only a proven completed
					// output intent may complete them, otherwise operator
					// resolution via --queue-resolve.
					unknownProven := false
					unknownMarker := fmt.Sprintf("<!-- pr-%s-output:%s -->", j.Kind, j.ID)
					if itBytes := intentsBucket.Get([]byte(unknownMarker)); itBytes != nil {
						var it OutputIntent
						if err := json.Unmarshal(itBytes, &it); err == nil && it.Status == "completed" {
							unknownProven = true
						}
					}
					if unknownProven {
						j.Status = "completed"
					} else {
						j.Status = "needs_attention"
						j.RecoveryPhase = "interrupted_unproven"
						j.Error = "interrupted action cannot prove external outcome; requires operator resolution"
					}
				}

				jBytes, err := json.Marshal(j)
				if err != nil {
					return err
				}
				if err := jobsBucket.Put(k, jBytes); err != nil {
					return err
				}
				copyJob := j
				recovered = append(recovered, &copyJob)
			}
		}

		// Update PR states: release active slots for running/uncertain jobs, and block PRs with uncertain/needs_attention jobs
		blockedPRs := make(map[string]string)
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				if j.Status == "uncertain" || j.Status == "needs_attention" {
					blockedPRs[j.PRKey.String()] = fmt.Sprintf("job %s is %s", j.ID, j.Status)
				}
			}
		}

		prCursor := prsBucket.Cursor()
		for k, v := prCursor.First(); k != nil; k, v = prCursor.Next() {
			var st PRState
			if err := json.Unmarshal(v, &st); err == nil {
				st.ActiveJobID = ""
				if reason, blocked := blockedPRs[st.PRKey.String()]; blocked {
					st.HasBlockedAction = true
					st.BlockedReason = reason
				} else {
					st.HasBlockedAction = false
					st.BlockedReason = ""
				}
				if stBytes, err := json.Marshal(st); err == nil {
					_ = prsBucket.Put(k, stBytes)
				}
			}
		}

		return nil
	})
	return recovered, err
}

// RecoverInterruptedJobs delegates to RecoverJobs for backward compatibility.
func (s *BoltJobStore) RecoverInterruptedJobs(ctx context.Context) ([]*Job, error) {
	return s.RecoverJobs(ctx)
}

func (s *BoltJobStore) InspectJobs(ctx context.Context) ([]*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var list []*Job
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				copyJob := j
				list = append(list, &copyJob)
			}
		}
		return nil
	})
	return list, err
}

func (s *BoltJobStore) ResolveJob(ctx context.Context, jobID string, resolution string, ackDuplicateRisk bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution != "confirmed" && resolution != "rerun" && resolution != "cancel" {
		return fmt.Errorf("invalid resolution %q: must be confirmed, rerun, or cancel", resolution)
	}
	if resolution == "rerun" && !ackDuplicateRisk {
		return errors.New("rerun requires --acknowledge-duplicate-risk")
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)

		var targetKey []byte
		var targetJob Job
		found := false

		c := jobsBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil && j.ID == jobID {
				targetKey = make([]byte, len(k))
				copy(targetKey, k)
				targetJob = j
				found = true
				break
			}
		}

		if !found {
			return ErrJobNotFound
		}

		now := time.Now().UTC()
		switch resolution {
		case "confirmed":
			targetJob.Status = "completed"
			targetJob.Error = ""
			targetJob.FinishedAt = &now
			targetJob.RecoveryPhase = "operator_confirmed"
		case "rerun":
			targetJob.Status = "queued"
			targetJob.Error = ""
			targetJob.StartedAt = nil
			targetJob.FinishedAt = nil
			// An operator rerun means run now: clear any parked due time so
			// the job is immediately eligible instead of staying deferred.
			targetJob.NotBeforeAt = time.Time{}
			targetJob.RecoveryPhase = "operator_rerun"
		case "cancel":
			targetJob.Status = "cancelled"
			targetJob.Error = "cancelled by operator"
			targetJob.FinishedAt = &now
			targetJob.RecoveryPhase = "operator_cancelled"
		}

		jBytes, err := json.Marshal(targetJob)
		if err != nil {
			return err
		}
		if err := jobsBucket.Put(targetKey, jBytes); err != nil {
			return err
		}

		// Re-evaluate PR state for HasBlockedAction
		prKeyStr := targetJob.PRKey.String()
		hasOtherBlocked := false
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil && j.PRKey.String() == prKeyStr && j.ID != jobID {
				if j.Status == "uncertain" || j.Status == "needs_attention" {
					hasOtherBlocked = true
					break
				}
			}
		}

		if stBytes := prsBucket.Get([]byte(prKeyStr)); stBytes != nil {
			var st PRState
			if err := json.Unmarshal(stBytes, &st); err == nil {
				st.HasBlockedAction = hasOtherBlocked
				if !hasOtherBlocked {
					st.BlockedReason = ""
				}
				if updatedBytes, err := json.Marshal(st); err == nil {
					_ = prsBucket.Put([]byte(prKeyStr), updatedBytes)
				}
			}
		}

		return nil
	})
}

func (s *BoltJobStore) GetHealthCounts(ctx context.Context, oldWaitWarning time.Duration) (HealthCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return HealthCounts{}, ErrStoreClosed
	}

	var counts HealthCounts
	now := time.Now().UTC()
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				switch j.Status {
				case "queued":
					counts.Queued++
					if !j.NotBeforeAt.IsZero() && now.Before(j.NotBeforeAt) {
						counts.Deferred++
					}
					if oldWaitWarning > 0 && now.Sub(j.CreatedAt) > oldWaitWarning {
						counts.OldWaitWarning = true
					}
				case "running":
					counts.Running++
				case "uncertain":
					counts.Uncertain++
				case "needs_attention":
					counts.NeedsAttention++
				}
			}
		}
		return nil
	})
	return counts, err
}

// ClaimNextJob claims the oldest eligible queued job while respecting per-PR exclusion
// and reserving a successor slot when starting an automatic review.
func (s *BoltJobStore) ClaimNextJob(ctx context.Context, activePRs map[string]bool) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var claimed *Job
	err := s.db.Update(func(tx *bbolt.Tx) error {
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)

		// 1. Calculate current capacity metrics
		queuedCount := 0
		reservedCount := 0
		estimatedBytes := int64(0)
		c := jobsBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				estimatedBytes += int64(len(v)) + j.Reservations
				if j.Status == "queued" {
					queuedCount++
				}
			}
		}

		prCursor := prsBucket.Cursor()
		for k, v := prCursor.First(); k != nil; k, v = prCursor.Next() {
			var st PRState
			if err := json.Unmarshal(v, &st); err == nil {
				if st.HasReservedSuccessor && st.ActiveJobID != "" {
					reservedCount++
					estimatedBytes += EstimatedJobStorageReserve
				}
			}
		}

		// 2. Scan queued jobs in FIFO sequence order
		claimNow := time.Now().UTC()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err != nil || j.Status != "queued" {
				continue
			}
			// Not-due deferred attempts stay parked without blocking other
			// PRs; they become eligible once the due time arrives.
			if !j.NotBeforeAt.IsZero() && claimNow.Before(j.NotBeforeAt) {
				continue
			}

			prKeyStr := j.PRKey.String()
			if activePRs[prKeyStr] {
				continue
			}

			// Fetch PR state
			var state PRState
			if stBytes := prsBucket.Get([]byte(prKeyStr)); stBytes != nil {
				_ = json.Unmarshal(stBytes, &state)
			} else {
				state = PRState{
					PRKey:  j.PRKey,
					Owner:  j.Owner,
					Repo:   j.Repo,
					Number: j.PRNumber,
				}
			}

			if state.ActiveJobID != "" || state.HasBlockedAction {
				continue
			}

			// If automatic review, reserve 1 successor slot and storage
			if j.Trigger == "automatic" && j.Kind == "review" {
				// Capacity check: since j was queued and becomes running,
				// queuedCount becomes queuedCount - 1, and reservedCount becomes reservedCount + 1.
				if queuedCount+reservedCount > s.opts.BacklogLimit ||
					estimatedBytes+EstimatedJobStorageReserve > s.opts.StateMaxBytes {
					// Cannot acquire reservation; leave review queued
					continue
				}

				state.ActiveJobID = j.ID
				state.HasReservedSuccessor = true
				if state.PendingAutoJobID == j.ID {
					state.PendingAutoJobID = ""
				}
			} else {
				state.ActiveJobID = j.ID
			}

			now := time.Now().UTC()
			j.Status = "running"
			j.StartedAt = &now

			// Update job and PR state
			jBytes, err := json.Marshal(j)
			if err != nil {
				return err
			}
			if err := jobsBucket.Put(k, jBytes); err != nil {
				return err
			}

			stBytes, err := json.Marshal(state)
			if err != nil {
				return err
			}
			if err := prsBucket.Put([]byte(prKeyStr), stBytes); err != nil {
				return err
			}

			copyJob := j
			claimed = &copyJob
			return nil
		}
		return nil
	})

	return claimed, err
}

// ReleasePR releases active execution for a PR, transferring or releasing successor reservations.
func (s *BoltJobStore) ReleasePR(ctx context.Context, prKey PRKey) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		prsBucket := tx.Bucket(bucketPRs)
		jobsBucket := tx.Bucket(bucketJobs)

		prKeyBytes := []byte(prKey.String())
		v := prsBucket.Get(prKeyBytes)
		if v == nil {
			return nil
		}

		var state PRState
		if err := json.Unmarshal(v, &state); err != nil {
			return err
		}

		state.ActiveJobID = ""

		// Check if a queued automatic review successor exists
		hasQueuedSuccessor := false
		if state.PendingAutoJobID != "" {
			c := jobsBucket.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var j Job
				if err := json.Unmarshal(v, &j); err == nil {
					if j.ID == state.PendingAutoJobID && j.Status == "queued" {
						hasQueuedSuccessor = true
						break
					}
				}
			}
		}

		if hasQueuedSuccessor {
			// Transfer unused/newly-used reservation on succession
			state.HasReservedSuccessor = true
		} else {
			// Release when no follow-up is necessary
			state.HasReservedSuccessor = false
			state.PendingAutoJobID = ""
		}

		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return prsBucket.Put(prKeyBytes, raw)
	})
}

// DeferJob atomically parks a job as a deferred attempt due at dueAt. The job
// returns to queued with NotBeforeAt set, StartedAt cleared, and the reason
// recorded, so the worker is freed while the wait persists durably across
// restarts. Only queued, running, or uncertain jobs may be parked; repeating
// the same deferral is a no-op that returns the current state.
func (s *BoltJobStore) DeferJob(ctx context.Context, jobID string, dueAt time.Time, reason string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}
	if jobID == "" {
		return nil, ErrJobNotFound
	}
	if dueAt.IsZero() {
		return nil, fmt.Errorf("deferral due time must not be zero")
	}
	dueAt = dueAt.UTC()

	var deferred *Job
	err := s.db.Update(func(tx *bbolt.Tx) error {
		jobsBucket := tx.Bucket(bucketJobs)
		c := jobsBucket.Cursor()
		var targetKey []byte
		var target Job
		found := false
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil && j.ID == jobID {
				targetKey = append([]byte(nil), k...)
				target = j
				found = true
				break
			}
		}
		if !found {
			return ErrJobNotFound
		}
		switch target.Status {
		case "queued", "running", "uncertain":
			// Parkable states: requeue below.
		default:
			return fmt.Errorf("cannot defer job %s in terminal status %q", jobID, target.Status)
		}
		if target.Status == "queued" && target.NotBeforeAt.Equal(dueAt) {
			copyTarget := target
			deferred = &copyTarget
			return nil
		}
		target.Status = "queued"
		target.NotBeforeAt = dueAt
		target.StartedAt = nil
		target.FinishedAt = nil
		target.RecoveryPhase = "deferred_retry"
		if reason != "" {
			target.Error = reason
		}
		raw, err := json.Marshal(target)
		if err != nil {
			return err
		}
		if err := jobsBucket.Put(targetKey, raw); err != nil {
			return err
		}
		copyTarget := target
		deferred = &copyTarget
		return nil
	})
	return deferred, err
}

// EarliestDeferredDue reports the earliest future due time among queued
// deferred attempts. ok is false when no parked attempt is waiting.
func (s *BoltJobStore) EarliestDeferredDue(ctx context.Context) (due time.Time, ok bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return time.Time{}, false, ErrStoreClosed
	}
	now := time.Now().UTC()
	err = s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err != nil || j.Status != "queued" {
				continue
			}
			if j.NotBeforeAt.IsZero() || !now.Before(j.NotBeforeAt) {
				continue
			}
			if !ok || j.NotBeforeAt.Before(due) {
				due, ok = j.NotBeforeAt, true
			}
		}
		return nil
	})
	return due, ok, err
}

// ScheduleSuccessorReview creates or updates a queued automatic review intent for the latest head,
// using the reserved successor capacity.
func (s *BoltJobStore) ScheduleSuccessorReview(ctx context.Context, prKey PRKey, owner, repo string, prNum int, baseSHA, headSHA string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var scheduled *Job
	err := s.db.Update(func(tx *bbolt.Tx) error {
		j, err := scheduleSuccessorTx(tx, prKey, owner, repo, prNum, baseSHA, headSHA)
		scheduled = j
		return err
	})
	return scheduled, err
}

// scheduleSuccessorTx is the transactional core of ScheduleSuccessorReview.
func scheduleSuccessorTx(tx *bbolt.Tx, prKey PRKey, owner, repo string, prNum int, baseSHA, headSHA string) (*Job, error) {
	var scheduled *Job
	err := func() error {
		prsBucket := tx.Bucket(bucketPRs)
		jobsBucket := tx.Bucket(bucketJobs)
		countersBucket := tx.Bucket(bucketCounters)

		prKeyBytes := []byte(prKey.String())
		var state PRState
		if v := prsBucket.Get(prKeyBytes); v != nil {
			_ = json.Unmarshal(v, &state)
		} else {
			state = PRState{
				PRKey:  prKey,
				Owner:  owner,
				Repo:   repo,
				Number: prNum,
			}
		}

		// Check if queued automatic review already exists
		c := jobsBucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				if j.PRKey == prKey && j.Trigger == "automatic" && j.Kind == "review" && j.Status == "queued" {
					j.HeadSHA = headSHA
					j.BaseSHA = baseSHA
					state.Generation++
					j.Generation = state.Generation
					jBytes, err := json.Marshal(j)
					if err != nil {
						return err
					}
					if err := jobsBucket.Put(k, jBytes); err != nil {
						return err
					}
					state.PendingAutoJobID = j.ID
					state.LatestHeadSHA = headSHA
					state.HasReservedSuccessor = true
					stBytes, err := json.Marshal(state)
					if err != nil {
						return err
					}
					if err := prsBucket.Put(prKeyBytes, stBytes); err != nil {
						return err
					}

					// Update status intent head
					marker := fmt.Sprintf("<!-- pr-review-status:%s -->", j.ID)
					if raw := tx.Bucket(bucketIntents).Get([]byte(marker)); raw != nil {
						var in OutputIntent
						if err := json.Unmarshal(raw, &in); err == nil {
							in.ExactHead = headSHA
							in.UpdatedAt = time.Now().UTC()
							if updatedRaw, err := json.Marshal(&in); err == nil {
								_ = tx.Bucket(bucketIntents).Put([]byte(marker), updatedRaw)
							}
						}
					}

					copyJob := j
					scheduled = &copyJob
					return nil
				}
			}
		}

		// Create fresh latest-head review intent using reserved successor
		state.Generation++
		seq, err := countersBucket.NextSequence()
		if err != nil {
			return err
		}

		job := Job{
			ID:           fmt.Sprintf("job-%08d", seq),
			Sequence:     seq,
			Kind:         "review",
			Trigger:      "automatic",
			PRKey:        prKey,
			Owner:        owner,
			Repo:         repo,
			PRNumber:     prNum,
			BaseSHA:      baseSHA,
			HeadSHA:      headSHA,
			Generation:   state.Generation,
			Status:       "queued",
			CreatedAt:    time.Now().UTC(),
			Reservations: EstimatedJobStorageReserve,
		}

		jobBytes, err := json.Marshal(job)
		if err != nil {
			return err
		}
		jobKey := make([]byte, 8)
		binary.BigEndian.PutUint64(jobKey, seq)
		if err := jobsBucket.Put(jobKey, jobBytes); err != nil {
			return err
		}

		marker := fmt.Sprintf("<!-- pr-review-status:%s -->", job.ID)
		bodyWithMarker := fmt.Sprintf("⏳ Review queued; waiting for capacity\n\n%s", marker)
		h := sha256.Sum256([]byte(bodyWithMarker))
		statusIntent := OutputIntent{
			Marker:     marker,
			JobID:      job.ID,
			Action:     "status",
			PRKey:      job.PRKey,
			Owner:      job.Owner,
			Repo:       job.Repo,
			PRNumber:   job.PRNumber,
			ExactHead:  job.HeadSHA,
			Body:       bodyWithMarker,
			BodyDigest: hex.EncodeToString(h[:]),
			Status:     "pending",
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
		}
		if intentRaw, err := json.Marshal(&statusIntent); err == nil {
			_ = tx.Bucket(bucketIntents).Put([]byte(marker), intentRaw)
		}

		state.PendingAutoJobID = job.ID
		state.LatestHeadSHA = headSHA
		state.HasReservedSuccessor = true

		stBytes, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := prsBucket.Put(prKeyBytes, stBytes); err != nil {
			return err
		}

		scheduled = &job
		return nil
	}()
	return scheduled, err
}

// CommitPublicationOutcome atomically commits a publication outcome: the saved
// intent, the job and — only for a confirmed current-head review publication —
// PRState.LastReviewedHead. It reads the current persisted job and PR generation
// inside the same transaction, rejects stale success after generation movement and
// preserves active/latest/pending-successor fields rather than overwriting them
// from a stale PRState copy.
func (s *BoltJobStore) CommitPublicationOutcome(ctx context.Context, intent *OutputIntent, job *Job, prState *PRState) error {
	return s.commitOutcome(ctx, intent, job, prState, "", "")
}

// CommitSupersededOutcome commits a superseded outcome together with the durable
// latest-head successor obligation in one transaction, so a failed successor save
// can never leave a terminal job without a recoverable latest-head review.
func (s *BoltJobStore) CommitSupersededOutcome(ctx context.Context, intent *OutputIntent, job *Job, successorBaseSHA, successorHeadSHA string) error {
	return s.commitOutcome(ctx, intent, job, nil, successorBaseSHA, successorHeadSHA)
}

func (s *BoltJobStore) commitOutcome(ctx context.Context, intent *OutputIntent, job *Job, prState *PRState, succBase, succHead string) error {
	if intent == nil || job == nil {
		return errors.New("nil intent or job for publication outcome")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return ErrStoreClosed
	}

	intentCopy := *intent
	if intentCopy.BodyDigest == "" && intentCopy.Body != "" {
		h := sha256.Sum256([]byte(intentCopy.Body))
		intentCopy.BodyDigest = hex.EncodeToString(h[:])
	}
	if intentCopy.CreatedAt.IsZero() {
		intentCopy.CreatedAt = time.Now().UTC()
	}
	intentCopy.UpdatedAt = time.Now().UTC()

	err := s.db.Update(func(tx *bbolt.Tx) error {
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)
		prKeyBytes := []byte(job.PRKey.String())

		// Locate the persisted job.
		jobKey := make([]byte, 8)
		binary.BigEndian.PutUint64(jobKey, job.Sequence)
		if jobsBucket.Get(jobKey) == nil {
			found := false
			c := jobsBucket.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var j Job
				if err := json.Unmarshal(v, &j); err == nil && j.ID == job.ID {
					copy(jobKey, k)
					found = true
					break
				}
			}
			if !found {
				return ErrJobNotFound
			}
		}

		var current *PRState
		if v := prsBucket.Get(prKeyBytes); v != nil {
			var st PRState
			if err := json.Unmarshal(v, &st); err != nil {
				return err
			}
			current = &st
		}

		if job.Status == "completed" && current != nil && current.Generation > job.Generation {
			return ErrGenerationChanged
		}

		jobRaw, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if err := jobsBucket.Put(jobKey, jobRaw); err != nil {
			return err
		}
		intentRaw, err := json.Marshal(&intentCopy)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketIntents).Put([]byte(intentCopy.Marker), intentRaw); err != nil {
			return err
		}

		if job.Status == "completed" && intentCopy.Action == "review_output" {
			head := intentCopy.ExactHead
			if prState != nil && prState.LastReviewedHead != "" {
				head = prState.LastReviewedHead
			}
			next := PRState{PRKey: job.PRKey, Owner: job.Owner, Repo: job.Repo, Number: job.PRNumber, Generation: job.Generation}
			if current != nil {
				next = *current
			}
			next.LastReviewedHead = head
			raw, err := json.Marshal(&next)
			if err != nil {
				return err
			}
			if err := prsBucket.Put(prKeyBytes, raw); err != nil {
				return err
			}
		}

		if succHead != "" {
			if _, err := scheduleSuccessorTx(tx, job.PRKey, job.Owner, job.Repo, job.PRNumber, succBase, succHead); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		*intent = intentCopy
	}
	return err
}
