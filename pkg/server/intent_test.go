package server

import (
	"context"
	"fmt"
	"testing"
)

type fakeLLMCaller struct {
	response string
	err      error
}

func (f *fakeLLMCaller) ChatCompletion(ctx context.Context, systemPrompt string, userMessage string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func TestClassifyEditIntent(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		llmResponse string
		llmErr      error
		wantIntent  string
		wantMinConf float64
		wantSummary bool
		wantErr     bool
	}{
		{
			name:        "TR commit fixture: hatayı düzelt ve gönder",
			input:       "hatayı düzelt ve gönder",
			llmResponse: `{"intent": "commit", "confidence": 0.95, "summary": "Fix the error and commit"}`,
			wantIntent:  "commit",
			wantMinConf: 0.70,
			wantSummary: true,
			wantErr:     false,
		},
		{
			name:        "Typo fixture: dzüelt",
			input:       "dzüelt",
			llmResponse: `{"intent": "commit", "confidence": 0.88, "summary": "Fix the reported bug"}`,
			wantIntent:  "commit",
			wantMinConf: 0.70,
			wantSummary: true,
			wantErr:     false,
		},
		{
			name:        "EN question fixture",
			input:       "Why is this mutex used here?",
			llmResponse: `{"intent": "question", "confidence": 0.92, "summary": ""}`,
			wantIntent:  "question",
			wantMinConf: 0.70,
			wantSummary: false,
			wantErr:     false,
		},
		{
			name:        "Empty comment body",
			input:       "   ",
			llmResponse: ``,
			wantIntent:  "unclear",
			wantMinConf: 0.0,
			wantSummary: false,
			wantErr:     true,
		},
		{
			name:        "Malformed LLM JSON",
			input:       "fix this",
			llmResponse: `I cannot decide what to do here.`,
			wantIntent:  "unclear",
			wantMinConf: 0.0,
			wantSummary: false,
			wantErr:     true,
		},
		{
			name:        "LLM error returns unclear",
			input:       "fix this",
			llmErr:      fmt.Errorf("connection refused"),
			wantIntent:  "unclear",
			wantMinConf: 0.0,
			wantSummary: false,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeLLMCaller{
				response: tt.llmResponse,
				err:      tt.llmErr,
			}
			verdict, err := ClassifyEditIntent(context.Background(), fake, tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ClassifyEditIntent() error = %v, wantErr %v", err, tt.wantErr)
			}
			if verdict.Intent != tt.wantIntent {
				t.Errorf("got Intent = %q, want %q", verdict.Intent, tt.wantIntent)
			}
			if verdict.Confidence < tt.wantMinConf {
				t.Errorf("got Confidence = %f, want at least %f", verdict.Confidence, tt.wantMinConf)
			}
			if tt.wantSummary && verdict.Summary == "" {
				t.Errorf("expected non-empty English summary, got empty")
			}
		})
	}
}
