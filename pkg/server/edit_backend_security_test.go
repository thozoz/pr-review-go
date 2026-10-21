package server

import (
	"context"
	"github.com/thozoz/pr-review-go/pkg/config"
	"strings"
	"testing"
)

func TestEditCommandBackendRefusesHostFallback(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}, {EnableSandbox: true, SandboxMode: config.SandboxModePodman}} {
		out, stderr, code, err := editCommandBackend(cfg)(context.Background(), t.TempDir(), []string{"go", "test", "-race", "./..."})
		if err == nil || !strings.Contains(err.Error(), "host fallback refused") || code != -1 || out != "" || stderr != "" {
			t.Fatalf("backend must refuse before launching tools: %q %q %d %v", out, stderr, code, err)
		}
	}
}
