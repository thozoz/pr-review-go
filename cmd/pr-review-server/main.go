package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
	"github.com/thozoz/pr-review-go/pkg/server"
	"github.com/thozoz/pr-review-go/pkg/version"
)

func main() {
	showVersion := flag.Bool("version", false, "Print release version")
	queueInspect := flag.Bool("queue-inspect", false, "Inspect persistent queue jobs and exit")
	queueResolve := flag.String("queue-resolve", "", "Job ID to resolve")
	queueResolution := flag.String("queue-resolution", "", "Resolution disposition: confirmed, rerun, or cancel")
	ackDuplicateRisk := flag.Bool("acknowledge-duplicate-risk", false, "Acknowledge duplicate execution risk when rerunning a job")
	setupSandbox := flag.Bool("setup-sandbox", false, "Provision sandbox prerequisites (podman, 3 GiB slot, image) and exit")
	setupImage := flag.String("sandbox-image", "", "Sandbox image to pull/build (default SANDBOX_IMAGE or GHCR latest)")
	setupSlotDir := flag.String("sandbox-slot-dir", "", "Slot mount directory (default SANDBOX_SLOT_DIR or /var/lib/pr-review/slots/slot-01)")
	setupSlotSize := flag.String("sandbox-slot-size", "3G", "Backing file size for a new slot (e.g. 3G)")
	setupBuild := flag.Bool("build-from-source", false, "Build the image locally with --network=host instead of pulling")
	flag.Parse()

	if *showVersion {
		fmt.Printf("pr-review-server %s (%s, %s)\n", version.Version, version.Commit, version.Date)
		return
	}

	cfg := config.Load()

	// Sandbox provisioning helper: checks podman, prepares the 3 GiB loop
	// ext4 slot, and pulls (or locally builds) the trusted image, then exits.
	if *setupSandbox {
		os.Exit(runSetupSandbox(cfg, *setupImage, *setupSlotDir, *setupSlotSize, *setupBuild))
	}

	// Offline queue inspection and resolution tools require stopping the service
	// because bbolt holds an exclusive file lock.
	if *queueInspect || *queueResolve != "" {
		dbPath := filepath.Join(cfg.WebhookStateDir, "jobs.db")
		storeOpts := server.StoreOptions{
			BacklogLimit:  cfg.WebhookBacklog,
			DeliveryLimit: cfg.WebhookDeliveryLimit,
			StateMaxBytes: cfg.WebhookStateMaxBytes,
			DeliveryTTL:   cfg.WebhookDeliveryTTL,
			OpenTimeout:   1 * time.Second,
		}

		store, err := server.OpenJobStore(dbPath, storeOpts)
		if err != nil {
			if errors.Is(err, server.ErrDatabaseLocked) {
				log.Fatalf("Error: database is locked by another process (stop service first before queue operations)")
			}
			log.Fatalf("Failed to open job store at %s: %v", dbPath, err)
		}
		defer store.Close()

		ctx := context.Background()

		if *queueInspect {
			jobs, err := store.InspectJobs(ctx)
			if err != nil {
				log.Fatalf("Failed to inspect jobs: %v", err)
			}
			fmt.Printf("Persistent Queue Inspection (%s): %d total jobs\n", dbPath, len(jobs))
			fmt.Printf("%-15s %-8s %-12s %-10s %-16s %-32s %-10s %s\n", "JOB ID", "SEQ", "KIND", "TRIGGER", "STATUS", "QUEUE STATE", "PR", "ERROR/PHASE")
			fmt.Println(strings.Repeat("-", 120))
			for _, j := range jobs {
				errOrPhase := j.RecoveryPhase
				if j.Error != "" {
					errOrPhase = j.Error
				}
				fmt.Printf("%-15s %-8d %-12s %-10s %-16s %-32s %-10s %s\n",
					j.ID, j.Sequence, j.Kind, j.Trigger, j.Status, server.DescribeQueueState(j, time.Now().UTC()), j.PRKey.String(), errOrPhase)
			}
			return
		}

		if *queueResolve != "" {
			if *queueResolution == "" {
				log.Fatalf("Error: --queue-resolution must be specified when using --queue-resolve (confirmed, rerun, cancel)")
			}
			err := store.ResolveJob(ctx, *queueResolve, *queueResolution, *ackDuplicateRisk)
			if err != nil {
				log.Fatalf("Failed to resolve job %s: %v", *queueResolve, err)
			}
			fmt.Printf("Job %s successfully resolved with disposition: %s\n", *queueResolve, *queueResolution)
			return
		}
	}

	// Normal server mode with signal handling and graceful shutdown
	srv := server.NewServer(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		log.Fatalf("Failed to start durable runtime: %v", err)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
	}

	serverErrCh := make(chan error, 1)
	go func() {
		log.Printf("🚀 PR Review Server listening on http://%s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
		}
		close(serverErrCh)
	}()

	select {
	case err := <-serverErrCh:
		if err != nil {
			log.Fatalf("Server stopped with error: %v", err)
		}
	case <-ctx.Done():
		log.Println("Received termination signal, shutting down gracefully...")
		shutdownTimeout := cfg.WebhookShutdownTimeout
		if shutdownTimeout <= 0 {
			shutdownTimeout = 30 * time.Second
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()

		// Stop HTTP server first to reject new requests
		_ = httpServer.Shutdown(shutdownCtx)

		// Drain and shutdown workers and close store
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("Graceful shutdown failed: %v", err)
		}
		log.Println("Server shut down successfully.")
	}
}
