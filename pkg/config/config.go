package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// EffortLevel determines the speed/rigor of the review process.
	// Accepted values are "lite" (fast, no sandbox) or "balanced" (default, with sandbox verification).
	EffortLevel string `json:"effort_level"` // lite or balanced

	GitHubToken   string
	WebhookSecret string
	Port          int
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	EnableSandbox bool
	// AutoActions run when a PR opens. Nil uses the backward-compatible default:
	// review,labels. An empty slice disables all automatic actions.
	AutoActions []string
}

// Validate checks if required configuration is present and validates EffortLevel
func (c *Config) Validate() error {
	// Set default EffortLevel if empty
	if c.EffortLevel == "" {
		c.EffortLevel = "balanced"
	}
	if c.EffortLevel != "lite" && c.EffortLevel != "balanced" {
		return fmt.Errorf("invalid EffortLevel: %s (must be 'lite' or 'balanced')", c.EffortLevel)
	}
	if c.AutoActions == nil {
		c.AutoActions = []string{"review", "labels"}
	}
	for _, action := range c.AutoActions {
		switch action {
		case "review", "labels", "describe", "improve":
		default:
			return fmt.Errorf("invalid AUTO_ACTIONS value: %s", action)
		}
	}

	if c.GitHubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN or GH_TOKEN is required")
	}
	if c.LLMAPIKey == "" {
		return fmt.Errorf("LLM_API_KEY or OPENAI_API_KEY is required")
	}
	if c.LLMModel == "" {
		return fmt.Errorf("LLM_MODEL is required")
	}
	if c.LLMBaseURL == "" {
		return fmt.Errorf("LLM_BASE_URL is required")
	}
	return nil
}

func (c *Config) AutoActionEnabled(action string) bool {
	for _, configured := range c.AutoActions {
		if configured == action {
			return true
		}
	}
	return false
}

func Load() *Config {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}

	webhookSecret := os.Getenv("WEBHOOK_SECRET")
	if webhookSecret == "" {
		webhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	}

	port := 3000
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			port = p
		}
	}

	llmBaseURL := os.Getenv("LLM_BASE_URL")
	if llmBaseURL == "" {
		llmBaseURL = "https://api.openai.com/v1"
	}

	llmAPIKey := os.Getenv("LLM_API_KEY")
	if llmAPIKey == "" {
		llmAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	llmModel := os.Getenv("LLM_MODEL")
	if llmModel == "" {
		llmModel = "gpt-4o"
	}

	// Determine effort level and sandbox enablement
	effort := os.Getenv("EFFORT_LEVEL")
	if effort == "" {
		effort = "balanced"
	}
	enableSandbox := true
	if effort == "lite" {
		enableSandbox = false
	}

	var autoActions []string
	if raw, set := os.LookupEnv("AUTO_ACTIONS"); set {
		autoActions = []string{}
		for _, action := range strings.Split(raw, ",") {
			action = strings.TrimSpace(strings.ToLower(action))
			if action != "" {
				autoActions = append(autoActions, action)
			}
		}
	}

	return &Config{
		GitHubToken:   token,
		WebhookSecret: webhookSecret,
		Port:          port,
		LLMBaseURL:    llmBaseURL,
		LLMAPIKey:     llmAPIKey,
		LLMModel:      llmModel,
		EffortLevel:   effort,
		EnableSandbox: enableSandbox,
		AutoActions:   autoActions,
	}
}
