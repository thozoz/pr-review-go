package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// MaintenanceResult contains counters for a bounded maintenance pass.
type MaintenanceResult struct {
	Scanned  int `json:"scanned"`
	Deleted  int `json:"deleted"`
	Released int `json:"released"`
	Pinned   int `json:"pinned"`
}

// MaintainTerminalRecords reclaims expired terminal job reservations and state,
// removes expired receipts, and pins any active, queued, or uncertain work.
func (s *BoltJobStore) MaintainTerminalRecords(ctx context.Context, now time.Time, limit int) (MaintenanceResult, error) {
	if limit <= 0 {
		limit = 128
	}
	ttl := s.opts.DeliveryTTL
	if ttl <= 0 {
		ttl = 168 * time.Hour
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return MaintenanceResult{}, ErrStoreClosed
	}

	var res MaintenanceResult
	err := s.db.Update(func(tx *bbolt.Tx) error {
		delivBucket := tx.Bucket(bucketDeliveries)
		jobsBucket := tx.Bucket(bucketJobs)
		intentsBucket := tx.Bucket(bucketIntents)
		commentsBucket := tx.Bucket(bucketComments)
		prsBucket := tx.Bucket(bucketPRs)

		return s.maintainTerminalRecordsInTx(tx, delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket, now, ttl, limit, &res)
	})
	return res, err
}

func (s *BoltJobStore) maintainTerminalRecordsInTx(
	tx *bbolt.Tx,
	delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket *bbolt.Bucket,
	now time.Time,
	ttl time.Duration,
	limit int,
	res *MaintenanceResult,
) error {
	buckets := []*bbolt.Bucket{jobsBucket, delivBucket}
	bucketCount := len(buckets)

	for pass := 0; pass < bucketCount && res.Scanned < limit; pass++ {
		bIdx := s.mBucketIndex % bucketCount
		b := buckets[bIdx]
		if b == nil {
			s.mBucketIndex = (s.mBucketIndex + 1) % bucketCount
			s.mCursorKey = nil
			continue
		}

		c := b.Cursor()
		var k, v []byte
		if len(s.mCursorKey) > 0 {
			k, v = c.Seek(s.mCursorKey)
		} else {
			k, v = c.First()
		}

		type itemToProcess struct {
			k []byte
			v []byte
		}
		var items []itemToProcess
		for k != nil && res.Scanned < limit {
			res.Scanned++
			items = append(items, itemToProcess{k: bytes.Clone(k), v: bytes.Clone(v)})
			k, v = c.Next()
		}

		for _, item := range items {
			if bIdx == 0 {
				// Process Job in bucketJobs
				s.maintainJobRecord(delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket, item.k, item.v, now, ttl, res)
			} else if bIdx == 1 {
				// Process Delivery in bucketDeliveries
				s.maintainDeliveryRecord(delivBucket, jobsBucket, item.k, item.v, now, ttl, res)
			}
		}

		if k == nil {
			// Finished current bucket, advance to next bucket
			s.mBucketIndex = (s.mBucketIndex + 1) % bucketCount
			s.mCursorKey = nil
		} else {
			// Retain cursor position in current bucket
			s.mCursorKey = bytes.Clone(k)
			break
		}
	}

	return nil
}

func (s *BoltJobStore) maintainJobRecord(
	delivBucket, jobsBucket, intentsBucket, commentsBucket, prsBucket *bbolt.Bucket,
	k, v []byte,
	now time.Time,
	ttl time.Duration,
	res *MaintenanceResult,
) {
	var j Job
	if err := json.Unmarshal(v, &j); err != nil || j.ID == "" || j.CreatedAt.IsZero() {
		res.Pinned++
		return
	}

	// Non-terminal jobs are always pinned
	if j.Status == "queued" || j.Status == "running" || j.Status == "uncertain" || j.Status == "needs_attention" {
		res.Pinned++
		return
	}

	// Terminal jobs: completed, failed, superseded, cancelled
	if j.FinishedAt == nil || now.Sub(*j.FinishedAt) < ttl {
		res.Pinned++
		return
	}

	// Expired terminal job: clear stale PR state references if pointing to this terminal job
	prKeyBytes := []byte(j.PRKey.String())
	if prBytes := prsBucket.Get(prKeyBytes); prBytes != nil {
		var prState PRState
		if err := json.Unmarshal(prBytes, &prState); err == nil {
			modified := false
			if prState.PendingAutoJobID == j.ID {
				prState.PendingAutoJobID = ""
				prState.HasReservedSuccessor = false
				modified = true
			}
			if prState.ActiveJobID == j.ID {
				prState.ActiveJobID = ""
				modified = true
			}
			if modified {
				if updatedPRBytes, err := json.Marshal(prState); err == nil {
					_ = prsBucket.Put(prKeyBytes, updatedPRBytes)
				}
			}
		}
	}

	// Check if any intent for this job is in non-terminal state
	outputMarker := fmt.Sprintf("<!-- pr-review-output:%s -->", j.ID)
	statusMarker := fmt.Sprintf("<!-- pr-review-status:%s -->", j.ID)
	for _, marker := range []string{outputMarker, statusMarker} {
		if intentBytes := intentsBucket.Get([]byte(marker)); intentBytes != nil {
			var in OutputIntent
			if err := json.Unmarshal(intentBytes, &in); err == nil {
				if in.Status == "pending" || in.Status == "in_progress" || in.Status == "uncertain" {
					res.Pinned++
					return
				}
			}
		}
	}

	// Release reservations
	if j.Reservations > 0 {
		j.Reservations = 0
		if updatedBytes, err := json.Marshal(&j); err == nil {
			_ = jobsBucket.Put(k, updatedBytes)
		}
		res.Released++
	}

	// Clear large saved bodies on completed intents, preserving markers and digests
	for _, marker := range []string{outputMarker, statusMarker} {
		if intentBytes := intentsBucket.Get([]byte(marker)); intentBytes != nil {
			var in OutputIntent
			if err := json.Unmarshal(intentBytes, &in); err == nil && in.Status == "completed" {
				if len(in.Body) > 0 {
					in.Body = ""
					if updatedIntent, err := json.Marshal(&in); err == nil {
						_ = intentsBucket.Put([]byte(marker), updatedIntent)
					}
				}
			}
		}
	}

	// Delete fully expired terminal job
	_ = jobsBucket.Delete(k)
	res.Deleted++

	// Clean up comment uniqueness keys
	if j.CommentID > 0 {
		commentKey := fmt.Sprintf("%d/%d/created", j.PRKey.RepoID, j.CommentID)
		if commentsBucket.Get([]byte(commentKey)) != nil {
			_ = commentsBucket.Delete([]byte(commentKey))
			res.Deleted++
		}
	}

	// Clean up completed intents
	for _, marker := range []string{outputMarker, statusMarker} {
		if intentsBucket.Get([]byte(marker)) != nil {
			_ = intentsBucket.Delete([]byte(marker))
			res.Deleted++
		}
	}
}

func (s *BoltJobStore) maintainDeliveryRecord(
	delivBucket, jobsBucket *bbolt.Bucket,
	k, v []byte,
	now time.Time,
	ttl time.Duration,
	res *MaintenanceResult,
) {
	var d Delivery
	if err := json.Unmarshal(v, &d); err != nil || d.ReceivedAt.IsZero() {
		res.Pinned++
		return
	}

	// Check if delivery has reached expiration TTL
	if now.Sub(d.ReceivedAt) < ttl {
		res.Pinned++
		return
	}

	// Conservatively pin legacy receipts without job association metadata
	if len(d.JobIDs) == 0 {
		res.Pinned++
		return
	}

	// Check if all associated jobs are terminal
	allTerminal := true
	c := jobsBucket.Cursor()
	for _, jID := range d.JobIDs {
		// Locate job in jobsBucket
		found := false
		for jk, jv := c.First(); jk != nil; jk, jv = c.Next() {
			var j Job
			if err := json.Unmarshal(jv, &j); err == nil && j.ID == jID {
				found = true
				if j.Status == "queued" || j.Status == "running" || j.Status == "uncertain" || j.Status == "needs_attention" {
					allTerminal = false
				}
				break
			}
		}
		if !allTerminal {
			break
		}
		_ = found // If job was already deleted by maintenance, it is terminal
	}

	if !allTerminal {
		res.Pinned++
		return
	}

	// All jobs terminal and delivery expired: delete delivery receipt
	_ = delivBucket.Delete(k)
	res.Deleted++
}
