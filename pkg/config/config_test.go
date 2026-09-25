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
