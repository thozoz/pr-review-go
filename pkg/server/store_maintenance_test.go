package server

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreTerminalMaintenance(t *testing.T) {
	ctx := context.Background()

	t.Run("TTLBoundaryAndTerminalEligibility", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "jobs.db")
		ttl := 24 * time.Hour
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  50,
			DeliveryLimit: 50,
			DeliveryTTL:   ttl,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		baseTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
		prKey1, _ := MakePRKey("github.com", 100, 1)
		prKey2, _ := MakePRKey("github.com", 100, 2)
		prKey3, _ := MakePRKey("github.com", 100, 3)
		prKey4, _ := MakePRKey("github.com", 100, 4)

		// 1. Terminal job finished 1 hour before TTL boundary (TTL - 1h): should remain
		notExpiredJob := seedTestJob(t, store, prKey1, "base", "head1")
		notExpiredJob.Status = "completed"
		finishedAt1 := baseTime.Add(-23 * time.Hour)
		notExpiredJob.FinishedAt = &finishedAt1
		notExpiredJob.Reservations = 1000
		_ = store.UpdateJob(ctx, notExpiredJob)
		if in, err := store.GetOutputIntent(ctx, fmt.Sprintf("<!-- pr-review-status:%s -->", notExpiredJob.ID)); err == nil && in != nil {
			in.Status = "completed"
			_ = store.UpdateOutputIntent(ctx, in)
		}

		// 2. Terminal job finished 25 hours ago (TTL + 1h): should be reclaimed
		expiredJob := seedTestJob(t, store, prKey2, "base", "head2")
		expiredJob.Status = "completed"
		finishedAt2 := baseTime.Add(-25 * time.Hour)
		expiredJob.FinishedAt = &finishedAt2
		expiredJob.Reservations = 2000
		_ = store.UpdateJob(ctx, expiredJob)
		if in, err := store.GetOutputIntent(ctx, fmt.Sprintf("<!-- pr-review-status:%s -->", expiredJob.ID)); err == nil && in != nil {
			in.Status = "completed"
			_ = store.UpdateOutputIntent(ctx, in)
		}

		// 3. Active (queued) job created 30 hours ago: must be PINNED
		activeJob := seedTestJob(t, store, prKey3, "base", "head3")
		activeJob.Status = "queued"
		activeJob.CreatedAt = baseTime.Add(-30 * time.Hour)
		_ = store.UpdateJob(ctx, activeJob)

		// 4. Uncertain job created 30 hours ago: must be PINNED
		uncertainJob := seedTestJob(t, store, prKey4, "base", "head4")
		uncertainJob.Status = "uncertain"
		uncertainJob.CreatedAt = baseTime.Add(-30 * time.Hour)
		_ = store.UpdateJob(ctx, uncertainJob)

		// Run maintenance at baseTime
		res, err := store.MaintainTerminalRecords(ctx, baseTime, 128)
		if err != nil {
			t.Fatalf("MaintainTerminalRecords failed: %v", err)
		}

		if res.Released < 1 {
			t.Errorf("expected at least 1 released reservation, got %d", res.Released)
		}
		if res.Pinned < 2 {
			t.Errorf("expected at least 2 pinned active/uncertain records, got %d", res.Pinned)
		}

		// Verify non-expired job still exists
		j1, err := store.GetJob(ctx, notExpiredJob.ID)
		if err != nil || j1 == nil {
			t.Errorf("not-expired job should still exist: %v", err)
		}

		// Verify active job still exists and is queued
		jActive, err := store.GetJob(ctx, activeJob.ID)
		if err != nil || jActive.Status != "queued" {
			t.Errorf("active job should still exist and remain queued: %v", err)
		}

		// Verify uncertain job still exists
		jUncertain, err := store.GetJob(ctx, uncertainJob.ID)
		if err != nil || jUncertain.Status != "uncertain" {
			t.Errorf("uncertain job should still exist: %v", err)
		}

		// Reopen database and verify pinning survives restart
		store.Close()
		reopenedStore, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  50,
			DeliveryLimit: 50,
			DeliveryTTL:   ttl,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer reopenedStore.Close()

		jActive2, err := reopenedStore.GetJob(ctx, activeJob.ID)
		if err != nil || jActive2 == nil || jActive2.Status != "queued" {
			t.Errorf("active job must survive restart: %v", err)
		}
		jUncertain2, err := reopenedStore.GetJob(ctx, uncertainJob.ID)
		if err != nil || jUncertain2 == nil || jUncertain2.Status != "uncertain" {
			t.Errorf("uncertain job must survive restart: %v", err)
		}
	})

	t.Run("CapacityReclamationUnderSequentialPressure", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "pressure.db")
		ttl := 1 * time.Hour
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  5,
			DeliveryLimit: 5,
			DeliveryTTL:   ttl,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		baseTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

		// Fill delivery limit (5 deliveries)
		for i := 1; i <= 5; i++ {
			prKeyI, _ := MakePRKey("github.com", 200, i)
			deliv := Delivery{
				Host:        prKeyI.Host,
				RepoID:      prKeyI.RepoID,
				DeliveryID:  fmt.Sprintf("deliv-%d", i),
				EventKind:   "pull_request",
				PayloadHash: fmt.Sprintf("hash-%d", i),
				ReceivedAt:  baseTime.Add(-2 * time.Hour), // Expired
			}
			job := Job{
				Kind:      "review",
				Trigger:   "automatic",
				PRKey:     prKeyI,
				Owner:     "o",
				Repo:      "r",
				PRNumber:  i,
				HeadSHA:   fmt.Sprintf("head-%d", i),
				BaseSHA:   "base",
				CreatedAt: baseTime.Add(-2 * time.Hour),
			}
			res, err := store.Admit(ctx, deliv, []Job{job})
			if err != nil || res.Status != AdmitAccepted {
				t.Fatalf("admit %d failed: status=%v err=%v", i, res.Status, err)
			}

			// Mark admitted job as terminal and expired
			qJobs, _ := store.ListQueuedJobs(ctx)
			j := qJobs[len(qJobs)-1]
			j.Status = "completed"
			fin := baseTime.Add(-2 * time.Hour)
			j.FinishedAt = &fin
			_ = store.UpdateJob(ctx, j)
			if in, err := store.GetOutputIntent(ctx, fmt.Sprintf("<!-- pr-review-status:%s -->", j.ID)); err == nil && in != nil {
				in.Status = "completed"
				_ = store.UpdateOutputIntent(ctx, in)
			}
		}

		// 6th delivery: at limit=5, but previous 5 deliveries are expired and their jobs terminal.
		// Pre-admission pressure cleanup should reclaim expired capacity and accept the 6th delivery!
		prKey6, _ := MakePRKey("github.com", 200, 6)
		deliv6 := Delivery{
			Host:        prKey6.Host,
			RepoID:      prKey6.RepoID,
			DeliveryID:  "deliv-6",
			EventKind:   "pull_request",
			PayloadHash: "hash-6",
			ReceivedAt:  baseTime,
		}
		job6 := Job{
			Kind:      "review",
			Trigger:   "automatic",
			PRKey:     prKey6,
			Owner:     "o",
			Repo:      "r",
			PRNumber:  6,
			HeadSHA:   "head-6",
			BaseSHA:   "base",
			CreatedAt: baseTime,
		}
		res6, err := store.Admit(ctx, deliv6, []Job{job6})
		if err != nil {
			t.Fatalf("admit 6 error: %v", err)
		}
		if res6.Status != AdmitAccepted {
			t.Fatalf("expected 6th delivery to be accepted after pressure cleanup, got %v (%s)", res6.Status, res6.Reason)
		}
	})

	t.Run("ScanBatchBoundsAndCursorProgression", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "batch.db")
		store, err := OpenJobStore(dbPath, StoreOptions{
			BacklogLimit:  200,
			DeliveryLimit: 200,
			DeliveryTTL:   1 * time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		baseTime := time.Now().UTC()
		prKey, _ := MakePRKey("github.com", 300, 1)

		// Seed 150 jobs
		for i := 1; i <= 150; i++ {
			_ = seedTestJob(t, store, prKey, "base", fmt.Sprintf("sha-%d", i))
		}

		// Run maintenance with batch limit 50
		mRes, err := store.MaintainTerminalRecords(ctx, baseTime, 50)
		if err != nil {
			t.Fatal(err)
		}

		if mRes.Scanned > 50 {
			t.Fatalf("scanned records %d exceeded batch limit 50", mRes.Scanned)
		}
		if store.mCursorKey == nil {
			t.Fatalf("expected cursor key to be preserved after partial batch")
		}
	})
}
