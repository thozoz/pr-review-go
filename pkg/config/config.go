package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	GitHubToken   string
	WebhookSecret string
	Port          int
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	EnableSandbox bool
}

// Validate checks if required configuration is present
func (c *Config) Validate() error {
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

	return &Config{
		GitHubToken:   token,
		WebhookSecret: webhookSecret,
		Port:          port,
		LLMBaseURL:    llmBaseURL,
		LLMAPIKey:     llmAPIKey,
		LLMModel:      llmModel,
		EnableSandbox: true,
	}
}
