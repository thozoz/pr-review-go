package config

import (
	"os"
)

type Config struct {
	GitHubToken   string
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	EnableSandbox bool
}

func Load() *Config {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}

	llmBaseURL := os.Getenv("LLM_BASE_URL")
	if llmBaseURL == "" {
		llmBaseURL = "http://192.168.1.129:4000/v1" // default local LiteLLM proxy
	}

	llmAPIKey := os.Getenv("LLM_API_KEY")
	if llmAPIKey == "" {
		llmAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	llmModel := os.Getenv("LLM_MODEL")
	if llmModel == "" {
		llmModel = "openai/agy/claude-sonnet-4-6"
	}

	return &Config{
		GitHubToken:   token,
		LLMBaseURL:    llmBaseURL,
		LLMAPIKey:     llmAPIKey,
		LLMModel:      llmModel,
		EnableSandbox: true,
	}
}
