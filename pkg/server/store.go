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
}

func (d *Delivery) Key() string {
	return fmt.Sprintf("%s/%d/%s", d.Host, d.RepoID, d.DeliveryID)
}

// Job represents a unit of work admitted by the durable ledger.
type Job struct {
	ID              string     `json:"id"`
	Sequence        uint64     `json:"sequence"`
	Kind            string     `json:"kind"`    // "review", "labels", "describe", "summary", "docs", "changelog", "improve", "assistant"
	Trigger         string     `json:"trigger"` // "automatic", "explicit"
	Author          string     `json:"author"`
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
	Payload         string     `json:"payload,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// OutputIntent records intent before external GitHub mutations.
type OutputIntent struct {
	Marker        string    `json:"marker"`
	JobID         string    `json:"job_id"`
	Action        string    `json:"action"`
	PRKey         PRKey     `json:"pr_key"`
	Owner         string    `json:"owner"`
	Repo          string    `json:"repo"`
	PRNumber      int       `json:"pr_number"`
	ExactHead     string    `json:"exact_head"`
	BodyDigest    string    `json:"body_digest"` // hex SHA-256
	Body          string    `json:"body"`
	VerifiedActor string    `json:"verified_actor"`
	Status        string    `json:"status"` // "pending", "in_progress", "completed", "uncertain", "needs_attention"
	CommentID     int64     `json:"comment_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// PRState tracks PR-level generation and active execution.
type PRState struct {
	PRKey            PRKey  `json:"pr_key"`
	Owner            string `json:"owner"`
	Repo             string `json:"repo"`
	Number           int    `json:"number"`
	Generation       uint64 `json:"generation"`
	LastReviewedHead string `json:"last_reviewed_head,omitempty"`
	ActiveJobID      string `json:"active_job_id,omitempty"`
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
	RecoverInterruptedJobs(ctx context.Context) ([]*Job, error)
}

type BoltJobStore struct {
	db   *bbolt.DB
	opts StoreOptions
	mu   sync.RWMutex
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
		for _, bName := range [][]byte{bucketDeliveries, bucketJobs, bucketPRs, bucketIntents, bucketCounters} {
			if _, err := tx.CreateBucketIfNotExists(bName); err != nil {
				return err
			}
		}
		return nil
	})
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
	}

	var result AdmitResult
	err := s.db.Update(func(tx *bbolt.Tx) error {
		delivBucket := tx.Bucket(bucketDeliveries)
		jobsBucket := tx.Bucket(bucketJobs)
		prsBucket := tx.Bucket(bucketPRs)
		countersBucket := tx.Bucket(bucketCounters)

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

		// 2. Check Delivery limit
		delivCount := delivBucket.Stats().KeyN
		if delivCount+1 > s.opts.DeliveryLimit {
			result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("delivery limit %d reached", s.opts.DeliveryLimit)}
			return nil
		}

		// 3. Count currently queued jobs to enforce Backlog limit
		queuedCount := 0
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

		if queuedCount+len(jobs) > s.opts.BacklogLimit {
			result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("queue backlog capacity %d reached", s.opts.BacklogLimit)}
			return nil
		}

		// 4. Check State Max Bytes (logical retained values + reservations)
		neededBytes := int64(0)
		for range jobs {
			neededBytes += EstimatedJobStorageReserve
		}
		if estimatedBytes+neededBytes > s.opts.StateMaxBytes {
			result = AdmitResult{Status: AdmitCapacityFull, Reason: fmt.Sprintf("state max bytes limit %d reached", s.opts.StateMaxBytes)}
			return nil
		}

		// 5. Commit Delivery receipt
		delivBytes, err := json.Marshal(delivery)
		if err != nil {
			return err
		}
		if err := delivBucket.Put(delivKey, delivBytes); err != nil {
			return err
		}

		// 6. Commit Jobs and PR state
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

				updatedPRBytes, err := json.Marshal(state)
				if err != nil {
					return err
				}
				if err := prsBucket.Put(prStateKey, updatedPRBytes); err != nil {
					return err
				}
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

// RecoverInterruptedJobs inspects uncompleted jobs on startup and sets their recovery phase.
// Running jobs that were interrupted are held as uncertain.
func (s *BoltJobStore) RecoverInterruptedJobs(ctx context.Context) ([]*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, ErrStoreClosed
	}

	var recovered []*Job
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketJobs)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var j Job
			if err := json.Unmarshal(v, &j); err == nil {
				if j.Status == "running" {
					// Crashed while running: hold as uncertain
					j.Status = "uncertain"
					j.RecoveryPhase = "recovered_after_crash"
					jBytes, err := json.Marshal(j)
					if err != nil {
						return err
					}
					if err := b.Put(k, jBytes); err != nil {
						return err
					}
					copyJob := j
					recovered = append(recovered, &copyJob)
				}
			}
		}
		return nil
	})
	return recovered, err
}
