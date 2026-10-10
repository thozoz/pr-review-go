package sandbox

import (
	"runtime"
	"testing"
	"time"

	"github.com/thozoz/pr-review-go/pkg/config"
)

func TestPlatformComponents_Gating(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"nil config", nil},
		{"disabled", &config.Config{}},
		{"enabled but no image", &config.Config{EnableSandbox: true, SandboxSlotDir: "/slots/slot-01"}},
		{"enabled but no slot dir", &config.Config{EnableSandbox: true, SandboxImage: "sha256:abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, sm := PlatformComponents(tc.cfg)
			if backend != nil || sm != nil {
				t.Fatalf("expected (nil, nil) for %s, got (%v, %v)", tc.name, backend, sm)
			}
		})
	}
}

func TestPodmanBackendForConfig_Mapping(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("backend wiring is Linux-only by design")
	}
	cfg := &config.Config{
		EnableSandbox:           true,
		SandboxImage:            "sha256:deadbeef",
		SandboxSlotDir:          "/slots/slot-01",
		SandboxCPUs:             2.0,
		SandboxMemoryBytes:      2147483648,
		SandboxPidsLimit:        256,
		SandboxTimeoutExecution: 5 * time.Minute,
		SandboxTimeoutCleanup:   15 * time.Second,
	}
	backend := PodmanBackendForConfig(cfg)
	if backend == nil {
		t.Fatal("expected non-nil backend on Linux with full sandbox config")
	}
	if backend.cfg.ImageDigest != "sha256:deadbeef" {
		t.Errorf("image digest not mapped: %q", backend.cfg.ImageDigest)
	}
	if backend.cfg.MemoryBytes != 2147483648 {
		t.Errorf("memory bytes not mapped: %d", backend.cfg.MemoryBytes)
	}
}

func TestSlotManagerForConfig_Gating(t *testing.T) {
	if sm := SlotManagerForConfig(nil); sm != nil {
		t.Error("expected nil slot manager for nil config")
	}
	if sm := SlotManagerForConfig(&config.Config{}); sm != nil {
		t.Error("expected nil slot manager when sandbox disabled")
	}
}
