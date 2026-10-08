package config

import (
	"os"
	"testing"
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

