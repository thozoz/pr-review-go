package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultEffortLevel(t *testing.T) {
	// Set required env vars
	os.Setenv("GITHUB_TOKEN", "test-token")
	os.Setenv("LLM_API_KEY", "test-key")
	os.Setenv("LLM_MODEL", "test-model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("EFFORT_LEVEL")
	}()

	cfg := Load()
	if cfg.EffortLevel != "balanced" {
		t.Fatalf("expected default EffortLevel 'balanced', got %s", cfg.EffortLevel)
	}
	if !cfg.EnableSandbox {
		t.Fatalf("expected sandbox enabled for balanced mode")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
}

func TestLoadLiteEffortLevel(t *testing.T) {
	// Set required env vars
	os.Setenv("GITHUB_TOKEN", "test-token")
	os.Setenv("LLM_API_KEY", "test-key")
	os.Setenv("LLM_MODEL", "test-model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	os.Setenv("EFFORT_LEVEL", "lite")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("EFFORT_LEVEL")
	}()

	cfg := Load()
	if cfg.EffortLevel != "lite" {
		t.Fatalf("expected EffortLevel 'lite', got %s", cfg.EffortLevel)
	}
	if cfg.EnableSandbox {
		t.Fatalf("expected sandbox disabled for lite mode")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
}

func TestValidateInvalidEffortLevel(t *testing.T) {
	cfg := &Config{EffortLevel: "fast", GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u"}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error for invalid EffortLevel")
	}
}

func TestAutoActionsDefaultsAndCanBeDisabled(t *testing.T) {
	cfg := &Config{GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if !cfg.AutoActionEnabled("review") || !cfg.AutoActionEnabled("labels") {
		t.Fatal("expected default review and labels actions")
	}

	disabled := &Config{GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u", AutoActions: []string{}}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("validation failed: %v", err)
	}
	if disabled.AutoActionEnabled("review") {
		t.Fatal("empty actions should disable auto review")
	}
}

func TestValidateInvalidAutoAction(t *testing.T) {
	cfg := &Config{GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u", AutoActions: []string{"review", "deploy"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid auto action")
	}
}

func TestValidateGitHubAppModeRequiresLLMSettings(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *Config
		expectedErr string
	}{
		{
			name: "missing LLM_API_KEY",
			cfg: &Config{
				GitHubAppID:             12345,
				GitHubAppPrivateKeyPath: "/path/to/key.pem",
				LLMModel:                "gpt-4o",
				LLMBaseURL:              "https://api.openai.com/v1",
			},
			expectedErr: "LLM_API_KEY or OPENAI_API_KEY is required",
		},
		{
			name: "missing LLM_MODEL",
			cfg: &Config{
				GitHubAppID:             12345,
				GitHubAppPrivateKeyPath: "/path/to/key.pem",
				LLMAPIKey:               "test-key",
				LLMBaseURL:              "https://api.openai.com/v1",
			},
			expectedErr: "LLM_MODEL is required",
		},
		{
			name: "missing LLM_BASE_URL",
			cfg: &Config{
				GitHubAppID:             12345,
				GitHubAppPrivateKeyPath: "/path/to/key.pem",
				LLMAPIKey:               "test-key",
				LLMModel:                "gpt-4o",
			},
			expectedErr: "LLM_BASE_URL is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error %q, got nil", tt.expectedErr)
			}
			if err.Error() != tt.expectedErr {
				t.Fatalf("expected error %q, got %q", tt.expectedErr, err.Error())
			}
		})
	}
}

func TestValidateGitHubAppModePositive(t *testing.T) {
	cfg := &Config{
		GitHubAppID:             12345,
		GitHubAppPrivateKeyPath: "/path/to/key.pem",
		LLMAPIKey:               "test-key",
		LLMModel:                "gpt-4o",
		LLMBaseURL:              "https://api.openai.com/v1",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config for App mode with LLM settings, got: %v", err)
	}
}

func TestValidateSetupModeBypassesAuthAndLLM(t *testing.T) {
	cfg := &Config{
		GitHubAppSetupToken: "setup-token",
		PublicURL:           "https://example.com",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected setup mode to bypass auth and LLM validation, got: %v", err)
	}
}

func TestValidateSandboxResourceLimits(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken:   "token",
			LLMAPIKey:     "key",
			LLMModel:      "model",
			LLMBaseURL:    "https://example.com",
			EnableSandbox: true,
		}
	}

	// 1. Defaults are set and valid
	cfg := baseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default sandbox config should be valid: %v", err)
	}
	if cfg.SandboxCPUs != DefaultSandboxCPUs {
		t.Errorf("expected default CPUs %v, got %v", DefaultSandboxCPUs, cfg.SandboxCPUs)
	}
	if cfg.SandboxMemoryBytes != DefaultSandboxMemoryBytes {
		t.Errorf("expected default memory %v, got %v", DefaultSandboxMemoryBytes, cfg.SandboxMemoryBytes)
	}
	if cfg.SandboxDiskBytes != DefaultSandboxDiskBytes {
		t.Errorf("expected default disk %v, got %v", DefaultSandboxDiskBytes, cfg.SandboxDiskBytes)
	}

	// 2. Reject negative/zero CPU
	cfg = baseCfg()
	cfg.SandboxCPUs = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative SandboxCPUs")
	}

	// 3. Reject negative/zero Memory
	cfg = baseCfg()
	cfg.SandboxMemoryBytes = 0
	cfg.SandboxCPUs = 2.0
	// 0 memory will be defaulted unless we explicitly test invalid
	cfg.SandboxMemoryBytes = -100
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative SandboxMemoryBytes")
	}

	// 4. Reject negative/zero PIDs
	cfg = baseCfg()
	cfg.SandboxPidsLimit = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative SandboxPidsLimit")
	}

	// 5. Reject negative/zero Disk
	cfg = baseCfg()
	cfg.SandboxDiskBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative SandboxDiskBytes")
	}

	// 6. Reject Disk > 3 GiB
	cfg = baseCfg()
	cfg.SandboxDiskBytes = 3221225473 // 3 GiB + 1 byte
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for SandboxDiskBytes exceeding 3 GiB")
	}

	// 7. Reject negative timeouts
	cfg = baseCfg()
	cfg.SandboxTimeoutExecution = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative SandboxTimeoutExecution")
	}
}

func TestValidateWebhookResourceLimits(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken:   "token",
			LLMAPIKey:     "key",
			LLMModel:      "model",
			LLMBaseURL:    "https://example.com",
			EnableSandbox: false,
		}
	}

	// 1. Defaults are populated and valid
	cfg := baseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default webhook config should be valid: %v", err)
	}
	if cfg.WebhookWorkers != DefaultWebhookWorkers {
		t.Errorf("expected default workers %d, got %d", DefaultWebhookWorkers, cfg.WebhookWorkers)
	}
	if cfg.WebhookBacklog != DefaultWebhookBacklog {
		t.Errorf("expected default backlog %d, got %d", DefaultWebhookBacklog, cfg.WebhookBacklog)
	}
	if cfg.WebhookDeliveryTTL != DefaultWebhookDeliveryTTL {
		t.Errorf("expected default delivery TTL %v, got %v", DefaultWebhookDeliveryTTL, cfg.WebhookDeliveryTTL)
	}
	if cfg.WebhookDeliveryLimit != DefaultWebhookDeliveryLimit {
		t.Errorf("expected default delivery limit %d, got %d", DefaultWebhookDeliveryLimit, cfg.WebhookDeliveryLimit)
	}
	if cfg.WebhookStateMaxBytes != DefaultWebhookStateMaxBytes {
		t.Errorf("expected default state max bytes %d, got %d", DefaultWebhookStateMaxBytes, cfg.WebhookStateMaxBytes)
	}
	if cfg.WebhookBodyMaxBytes != DefaultWebhookBodyMaxBytes {
		t.Errorf("expected default body max bytes %d, got %d", DefaultWebhookBodyMaxBytes, cfg.WebhookBodyMaxBytes)
	}

	// 2. Reject out of range workers
	cfg = baseCfg()
	cfg.WebhookWorkers = 0 // will be defaulted
	cfg.WebhookWorkers = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative WebhookWorkers")
	}
	cfg.WebhookWorkers = 17
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookWorkers > 16")
	}

	// 3. Reject out of range backlog
	cfg = baseCfg()
	cfg.WebhookBacklog = 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookBacklog < 2")
	}
	cfg.WebhookBacklog = 10001
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookBacklog > 10000")
	}

	// 4. Reject out of range DeliveryTTL
	cfg = baseCfg()
	cfg.WebhookDeliveryTTL = 30 * time.Minute
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookDeliveryTTL < 1h")
	}
	cfg.WebhookDeliveryTTL = 721 * time.Hour
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookDeliveryTTL > 720h")
	}

	// 5. Reject out of range StateMaxBytes
	cfg = baseCfg()
	cfg.WebhookStateMaxBytes = 1024 // 1 KiB (< 16 MiB)
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookStateMaxBytes < 16 MiB")
	}

	// 6. Reject out of range BodyMaxBytes
	cfg = baseCfg()
	cfg.WebhookBodyMaxBytes = 1024 // 1 KiB (< 64 KiB)
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for WebhookBodyMaxBytes < 64 KiB")
	}
}

func TestValidateDiffLimits(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken:   "token",
			LLMAPIKey:     "key",
			LLMModel:      "model",
			LLMBaseURL:    "https://example.com",
			EnableSandbox: false,
		}
	}

	// 1. Defaults are populated and valid
	cfg := baseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default diff config should be valid: %v", err)
	}
	if cfg.DiffMaxBytes != DefaultDiffMaxBytes {
		t.Errorf("expected default DiffMaxBytes %d, got %d", DefaultDiffMaxBytes, cfg.DiffMaxBytes)
	}
	if cfg.DiffMaxFiles != DefaultDiffMaxFiles {
		t.Errorf("expected default DiffMaxFiles %d, got %d", DefaultDiffMaxFiles, cfg.DiffMaxFiles)
	}
	if cfg.DiffMaxHunks != DefaultDiffMaxHunks {
		t.Errorf("expected default DiffMaxHunks %d, got %d", DefaultDiffMaxHunks, cfg.DiffMaxHunks)
	}

	// 2. Reject negative/out-of-range DiffMaxBytes
	cfg = baseCfg()
	cfg.DiffMaxBytes = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative DiffMaxBytes")
	}
	cfg = baseCfg()
	cfg.DiffMaxBytes = 100 // < MinDiffMaxBytes (1024)
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for DiffMaxBytes < 1024")
	}
	cfg = baseCfg()
	cfg.DiffMaxBytes = MaxDiffMaxBytes + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for DiffMaxBytes > MaxDiffMaxBytes")
	}

	// 3. Reject negative/out-of-range DiffMaxFiles
	cfg = baseCfg()
	cfg.DiffMaxFiles = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative DiffMaxFiles")
	}
	cfg = baseCfg()
	cfg.DiffMaxFiles = MaxDiffMaxFiles + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for DiffMaxFiles > MaxDiffMaxFiles")
	}

	// 4. Reject negative/out-of-range DiffMaxHunks
	cfg = baseCfg()
	cfg.DiffMaxHunks = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative DiffMaxHunks")
	}
	cfg = baseCfg()
	cfg.DiffMaxHunks = MaxDiffMaxHunks + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for DiffMaxHunks > MaxDiffMaxHunks")
	}
}

func TestLoadDiffEnvRejectsZeroAndNegative(t *testing.T) {
	os.Setenv("GITHUB_TOKEN", "token")
	os.Setenv("LLM_API_KEY", "key")
	os.Setenv("LLM_MODEL", "model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	os.Setenv("DIFF_MAX_BYTES", "0")
	os.Setenv("DIFF_MAX_FILES", "-5")
	os.Setenv("DIFF_MAX_HUNKS", "0")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("DIFF_MAX_BYTES")
		os.Unsetenv("DIFF_MAX_FILES")
		os.Unsetenv("DIFF_MAX_HUNKS")
	}()

	cfg := Load()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error when DIFF_* env vars are 0 or negative")
	}
}

func TestValidateAgentDefaultsAndRanges(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken: "token",
			LLMAPIKey:   "key",
			LLMModel:    "model",
			LLMBaseURL:  "https://example.com",
		}
	}

	// 1. Defaults populated
	cfg := baseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default agent config should be valid: %v", err)
	}
	if cfg.AgentMaxTurns != DefaultAgentMaxTurns {
		t.Errorf("expected default AgentMaxTurns %d, got %d", DefaultAgentMaxTurns, cfg.AgentMaxTurns)
	}
	if cfg.AgentMaxToolBytes != DefaultAgentMaxToolBytes {
		t.Errorf("expected default AgentMaxToolBytes %d, got %d", DefaultAgentMaxToolBytes, cfg.AgentMaxToolBytes)
	}
	if cfg.AgentFileReadBytes != DefaultAgentFileReadBytes {
		t.Errorf("expected default AgentFileReadBytes %d, got %d", DefaultAgentFileReadBytes, cfg.AgentFileReadBytes)
	}
	if cfg.AgentSearchMaxMatches != DefaultAgentSearchMaxMatches {
		t.Errorf("expected default AgentSearchMaxMatches %d, got %d", DefaultAgentSearchMaxMatches, cfg.AgentSearchMaxMatches)
	}
	if cfg.AgentModelViolationLimit != DefaultAgentModelViolationLimit {
		t.Errorf("expected default AgentModelViolationLimit %d, got %d", DefaultAgentModelViolationLimit, cfg.AgentModelViolationLimit)
	}

	// 2. Reject out of range turns
	cfg = baseCfg()
	cfg.AgentMaxTurns = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative AgentMaxTurns")
	}
	cfg = baseCfg()
	cfg.AgentMaxTurns = MaxAgentMaxTurns + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentMaxTurns > MaxAgentMaxTurns")
	}

	// 3. Reject out of range tool bytes
	cfg = baseCfg()
	cfg.AgentMaxToolBytes = MinAgentMaxToolBytes - 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentMaxToolBytes < MinAgentMaxToolBytes")
	}
	cfg = baseCfg()
	cfg.AgentMaxToolBytes = MaxAgentMaxToolBytes + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentMaxToolBytes > MaxAgentMaxToolBytes")
	}

	// 4. Reject out of range read bytes
	cfg = baseCfg()
	cfg.AgentFileReadBytes = MinAgentFileReadBytes - 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentFileReadBytes < MinAgentFileReadBytes")
	}
	cfg = baseCfg()
	cfg.AgentFileReadBytes = MaxAgentFileReadBytes + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentFileReadBytes > MaxAgentFileReadBytes")
	}

	// 5. Reject out of range search matches
	cfg = baseCfg()
	cfg.AgentSearchMaxMatches = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative AgentSearchMaxMatches")
	}
	cfg = baseCfg()
	cfg.AgentSearchMaxMatches = MaxAgentSearchMaxMatches + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentSearchMaxMatches > MaxAgentSearchMaxMatches")
	}

	// 6. Reject out of range violation limit
	cfg = baseCfg()
	cfg.AgentModelViolationLimit = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative AgentModelViolationLimit")
	}
	cfg = baseCfg()
	cfg.AgentModelViolationLimit = MaxAgentModelViolationLimit + 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for AgentModelViolationLimit > MaxAgentModelViolationLimit")
	}
}

func TestLoadAgentEnvRejectsZeroAndNegative(t *testing.T) {
	os.Setenv("GITHUB_TOKEN", "token")
	os.Setenv("LLM_API_KEY", "key")
	os.Setenv("LLM_MODEL", "model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	os.Setenv("AGENT_MAX_TURNS", "0")
	os.Setenv("AGENT_MAX_TOOL_BYTES", "-1")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("AGENT_MAX_TURNS")
		os.Unsetenv("AGENT_MAX_TOOL_BYTES")
	}()

	cfg := Load()
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected validation error when AGENT_* env vars are 0 or negative")
	}
}

func TestMermaidConfig(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken: "token",
			LLMAPIKey:   "key",
			LLMModel:    "model",
			LLMBaseURL:  "https://example.com",
		}
	}

	// 1. Defaults populated
	cfg := baseCfg()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default mermaid config should be valid: %v", err)
	}
	if cfg.EnableMermaid != DefaultEnableMermaid {
		t.Errorf("expected default EnableMermaid false, got %v", cfg.EnableMermaid)
	}
	if cfg.MermaidDirectionThreshold != DefaultMermaidDirectionThreshold {
		t.Errorf("expected default direction threshold %d, got %d", DefaultMermaidDirectionThreshold, cfg.MermaidDirectionThreshold)
	}
	if cfg.MermaidMaxNodes != DefaultMermaidMaxNodes {
		t.Errorf("expected default max nodes %d, got %d", DefaultMermaidMaxNodes, cfg.MermaidMaxNodes)
	}

	// 2. Reject out of range direction threshold
	cfg = baseCfg()
	cfg.MermaidDirectionThreshold = -1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for negative MermaidDirectionThreshold")
	}

	// 3. Reject out of range nodes
	cfg = baseCfg()
	cfg.MermaidMaxNodes = 1
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for MermaidMaxNodes < MinMermaidMaxNodes")
	}

	// 4. Reject out of range bytes
	cfg = baseCfg()
	cfg.MermaidMaxBytes = 100
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected error for MermaidMaxBytes < MinMermaidMaxBytes")
	}
}

func TestLoadAllowRootfulSandboxEnv(t *testing.T) {
	os.Setenv("GITHUB_TOKEN", "token")
	os.Setenv("LLM_API_KEY", "key")
	os.Setenv("LLM_MODEL", "model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("ALLOW_ROOTFUL_SANDBOX")
	}()

	// 1. Default is opt-in off
	cfg := Load()
	if cfg.AllowRootfulSandbox {
		t.Errorf("expected AllowRootfulSandbox default false")
	}

	// 2. "1" and "true" enable it
	for _, raw := range []string{"1", "true", "TRUE"} {
		os.Setenv("ALLOW_ROOTFUL_SANDBOX", raw)
		cfg = Load()
		if !cfg.AllowRootfulSandbox {
			t.Errorf("expected AllowRootfulSandbox true for %q", raw)
		}
	}

	// 3. Other values stay off
	os.Setenv("ALLOW_ROOTFUL_SANDBOX", "0")
	if cfg = Load(); cfg.AllowRootfulSandbox {
		t.Errorf("expected AllowRootfulSandbox false for %q", "0")
	}
}

func TestLoadSandboxModeEnv(t *testing.T) {
	os.Setenv("GITHUB_TOKEN", "token")
	os.Setenv("LLM_API_KEY", "key")
	os.Setenv("LLM_MODEL", "model")
	os.Setenv("LLM_BASE_URL", "https://example.com")
	defer func() {
		os.Unsetenv("GITHUB_TOKEN")
		os.Unsetenv("LLM_API_KEY")
		os.Unsetenv("LLM_MODEL")
		os.Unsetenv("LLM_BASE_URL")
		os.Unsetenv("SANDBOX_MODE")
	}()

	// 1. Unset and blank values default to direct.
	t.Setenv("SANDBOX_MODE", "")
	cfg := Load()
	if cfg.SandboxMode != SandboxModeDirect {
		t.Errorf("expected default SandboxMode direct, got %q", cfg.SandboxMode)
	}
	t.Setenv("SANDBOX_MODE", "  ")
	if cfg = Load(); cfg.SandboxMode != SandboxModeDirect {
		t.Errorf("expected blank SandboxMode direct, got %q", cfg.SandboxMode)
	}
	cfg.SandboxMode = ""
	if err := cfg.Validate(); err != nil || cfg.SandboxMode != SandboxModeDirect {
		t.Errorf("expected Validate to default to direct, got %q, err=%v", cfg.SandboxMode, err)
	}
	t.Setenv("SANDBOX_MODE", " Podman ")
	if cfg = Load(); cfg.SandboxMode != SandboxModePodman {
		t.Errorf("expected explicit Podman opt-in, got %q", cfg.SandboxMode)
	}

	// 2. direct is accepted (case-insensitive, trimmed)
	os.Setenv("SANDBOX_MODE", "direct")
	if cfg = Load(); cfg.SandboxMode != SandboxModeDirect {
		t.Errorf("expected SandboxMode direct, got %q", cfg.SandboxMode)
	}
	os.Setenv("SANDBOX_MODE", " Direct ")
	if cfg = Load(); cfg.SandboxMode != SandboxModeDirect {
		t.Errorf("expected normalized SandboxMode direct, got %q", cfg.SandboxMode)
	}

	// 3. Invalid values fail validation
	os.Setenv("SANDBOX_MODE", "docker")
	if cfg = Load(); cfg.Validate() == nil {
		t.Errorf("expected validation error for SandboxMode %q", "docker")
	}
}

func TestValidateSandboxModeRejectsUnknown(t *testing.T) {
	cfg := &Config{GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u", SandboxMode: "lxc"}
	if err := cfg.Validate(); err == nil {
		t.Errorf("expected validation error for unknown SandboxMode")
	}
}

// TestConfigDirectRoots covers ROOT-01: env loading, state-dir-relative
// defaults, and rejection of a root inside the shared host temporary
// directory.
func TestConfigDirectRoots(t *testing.T) {
	// 1. Both knobs load verbatim from the environment.
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("LLM_API_KEY", "key")
	t.Setenv("LLM_MODEL", "model")
	t.Setenv("LLM_BASE_URL", "https://example.com")
	t.Setenv("SANDBOX_MODE", SandboxModeDirect)
	t.Setenv("SANDBOX_DIRECT_WORK_ROOT", "/var/lib/pr-review/direct")
	t.Setenv("SANDBOX_DIRECT_CACHE_DIR", "/var/lib/pr-review/cache")
	cfg := Load()
	if cfg.SandboxDirectWorkRoot != "/var/lib/pr-review/direct" {
		t.Errorf("SANDBOX_DIRECT_WORK_ROOT not loaded: %q", cfg.SandboxDirectWorkRoot)
	}
	if cfg.SandboxDirectCacheDir != "/var/lib/pr-review/cache" {
		t.Errorf("SANDBOX_DIRECT_CACHE_DIR not loaded: %q", cfg.SandboxDirectCacheDir)
	}

	// 2. Empty roots default under WebhookStateDir (never host /tmp).
	t.Setenv("SANDBOX_DIRECT_WORK_ROOT", "")
	t.Setenv("SANDBOX_DIRECT_CACHE_DIR", "")
	// Deliberately not a t.TempDir(): that lives under os.TempDir(), which the
	// validator rejects by design.
	const stateDir = "/var/lib/pr-review-test/state"
	base := &Config{
		GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u",
		EnableSandbox: true, SandboxMode: SandboxModeDirect, WebhookStateDir: stateDir,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("direct-mode Validate with empty roots failed: %v", err)
	}
	wantWork := filepath.Join(stateDir, "direct")
	wantCache := filepath.Join(stateDir, "direct", "cache")
	if base.SandboxDirectWorkRoot != wantWork {
		t.Errorf("default work root = %q, want %q", base.SandboxDirectWorkRoot, wantWork)
	}
	if base.SandboxDirectCacheDir != wantCache {
		t.Errorf("default cache root = %q, want %q", base.SandboxDirectCacheDir, wantCache)
	}

	// 3. A root inside the shared host temporary directory is rejected with
	// the literal requirement text, naming the offending field.
	bad := &Config{
		GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u",
		EnableSandbox: true, SandboxMode: SandboxModeDirect, WebhookStateDir: stateDir,
		SandboxDirectCacheDir: filepath.Join(os.TempDir(), "pr-review-direct"),
	}
	err := bad.Validate()
	if err == nil {
		t.Fatalf("expected shared-tmp cache root to be rejected")
	}
	if !strings.Contains(err.Error(), "must not be the shared host temporary directory") {
		t.Errorf("shared-tmp error text missing: %v", err)
	}
	if !strings.Contains(err.Error(), "SandboxDirectCacheDir") {
		t.Errorf("shared-tmp error must name the offending field: %v", err)
	}

	// 4. Podman mode is untouched: empty roots stay empty.
	podman := &Config{
		GitHubToken: "t", LLMAPIKey: "k", LLMModel: "m", LLMBaseURL: "u",
		EnableSandbox: true, SandboxMode: SandboxModePodman, WebhookStateDir: stateDir,
	}
	if err := podman.Validate(); err != nil {
		t.Fatalf("podman-mode Validate failed: %v", err)
	}
	if podman.SandboxDirectWorkRoot != "" || podman.SandboxDirectCacheDir != "" {
		t.Errorf("podman mode must not default direct roots: %q %q", podman.SandboxDirectWorkRoot, podman.SandboxDirectCacheDir)
	}
}

// TestValidateWebhookFailsClosedWithoutUserConfigDir pins 08-REVIEW WR-02:
// an unset WebhookStateDir with no user config directory is an error, never a
// silent fallback to the shared host temporary directory.
func TestValidateWebhookFailsClosedWithoutUserConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")
	if _, err := os.UserConfigDir(); err == nil {
		t.Skip("this platform still provides a user config dir with HOME/AppData cleared")
	}
	cfg := &Config{}
	err := cfg.ValidateWebhook()
	if err == nil {
		t.Fatal("unset WebhookStateDir without a user config dir must fail closed")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_STATE_DIR") {
		t.Errorf("error must name WEBHOOK_STATE_DIR: %v", err)
	}
	if cfg.WebhookStateDir != "" {
		t.Errorf("fail-closed validation must not invent a temp fallback, got %q", cfg.WebhookStateDir)
	}
}

// TestValidateDirectRootPathResolvesSymlink pins the second half of
// 08-REVIEW WR-02: a root that is lexically outside the temp directory but
// resolves into it through a symlink is refused.
func TestValidateDirectRootPathResolvesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	target, err := os.MkdirTemp("", "cfg-symlink-target-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	parent, err := os.MkdirTemp(".", ".cfg-symlink-parent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	link := filepath.Join(parent, "linked-root")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	value := link
	err = validateDirectRootPath("SandboxDirectWorkRoot", &value)
	if err == nil {
		t.Fatal("a symlink resolving into the shared temp directory must be refused")
	}
	if !strings.Contains(err.Error(), "shared host temporary directory") {
		t.Errorf("symlink refusal must use the shared-tmp error text: %v", err)
	}
}

func TestLLMMaxTokens(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			GitHubToken: "token",
			LLMAPIKey:   "key",
			LLMModel:    "model",
			LLMBaseURL:  "https://example.com",
		}
	}

	// 1. Direct validation tests
	validCases := []int{0, MinLLMMaxTokens, 4096, MaxLLMMaxTokens}
	for _, tc := range validCases {
		cfg := baseCfg()
		cfg.LLMMaxTokens = tc
		if err := cfg.Validate(); err != nil {
			t.Errorf("expected LLMMaxTokens=%d to be valid, got: %v", tc, err)
		}
	}

	invalidCases := []int{-1, MinLLMMaxTokens - 1, 100, MaxLLMMaxTokens + 1, 200000}
	for _, tc := range invalidCases {
		cfg := baseCfg()
		cfg.LLMMaxTokens = tc
		if err := cfg.Validate(); err == nil {
			t.Errorf("expected error for invalid LLMMaxTokens=%d, got nil", tc)
		}
	}

	// 2. Load() environment variable parsing tests
	envTests := []struct {
		name      string
		raw       string
		want      int
		shouldErr bool
	}{
		{name: "unset", raw: "", want: 0, shouldErr: false},
		{name: "whitespace", raw: "   ", want: 0, shouldErr: false},
		{name: "invalid non-numeric", raw: "invalid", want: 0, shouldErr: false},
		{name: "negative defaults to 0", raw: "-50", want: 0, shouldErr: false},
		{name: "valid 4096", raw: "4096", want: 4096, shouldErr: false},
		{name: "valid min boundary", raw: "256", want: 256, shouldErr: false},
		{name: "valid max boundary", raw: "131072", want: 131072, shouldErr: false},
		{name: "out-of-bounds low", raw: "100", want: 100, shouldErr: true},
		{name: "out-of-bounds high", raw: "200000", want: 200000, shouldErr: true},
	}

	for _, tt := range envTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", "token")
			t.Setenv("LLM_API_KEY", "key")
			t.Setenv("LLM_MODEL", "model")
			t.Setenv("LLM_BASE_URL", "https://example.com")
			t.Setenv("LLM_MAX_TOKENS", tt.raw)

			cfg := Load()
			if cfg.LLMMaxTokens != tt.want {
				t.Fatalf("Load() LLMMaxTokens = %d, want %d", cfg.LLMMaxTokens, tt.want)
			}

			err := cfg.Validate()
			if tt.shouldErr && err == nil {
				t.Fatalf("expected validation error for LLM_MAX_TOKENS=%q, got nil", tt.raw)
			}
			if !tt.shouldErr && err != nil {
				t.Fatalf("unexpected validation error for LLM_MAX_TOKENS=%q: %v", tt.raw, err)
			}
		})
	}
}
