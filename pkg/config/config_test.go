package config

import (
	"os"
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

