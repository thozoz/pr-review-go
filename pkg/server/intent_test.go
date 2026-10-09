package server

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeIntentLLM struct {
	response string
	err      error
	calls    int
	lastUser string
}

func (f *fakeIntentLLM) ChatCompletion(_ context.Context, _ string, userMessage string) (string, error) {
	f.calls++
	f.lastUser = userMessage
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func TestClassifyEditIntent(t *testing.T) {
	if IntentCommitThreshold != 0.70 {
		t.Fatalf("IntentCommitThreshold = %v, want 0.70", IntentCommitThreshold)
	}

	cases := []struct {
		name         string
		body         string
		llmResponse  string
		llmErr       error
		wantIntent   string
		wantErr      bool
		wantSummary  bool
		minConf      float64
		expectNoCall bool
	}{
		{
			name:        "TR commit instruction",
			body:        "hatayı düzelt ve gönder",
			llmResponse: `{"intent":"commit","confidence":0.92,"summary":"Fix the bug and submit"}`,
			wantIntent:  "commit",
			wantSummary: true,
			minConf:     IntentCommitThreshold,
		},
		{
			name:        "typo slang commit instruction",
			body:        "dzüelt",
			llmResponse: `{"intent":"commit","confidence":0.78,"summary":"Fix it"}`,
			wantIntent:  "commit",
			wantSummary: true,
			minConf:     IntentCommitThreshold,
		},
		{
			name:        "EN question",
			body:        "what does this function do?",
			llmResponse: `{"intent":"question","confidence":0.95,"summary":"Asks what the function does"}`,
			wantIntent:  "question",
			wantSummary: true,
			minConf:     0,
		},
		{
			name:         "empty body resolves to unclear with error",
			body:         "   ",
			wantIntent:   "unclear",
			wantErr:      true,
			expectNoCall: true,
		},
		{
			name:        "malformed LLM JSON resolves to unclear with error",
			body:        "please fix this",
			llmResponse: `not json at all`,
			wantIntent:  "unclear",
			wantErr:     true,
		},
		{
			name:       "LLM error resolves to unclear with error",
			body:       "fix the bug",
			llmErr:     errors.New("upstream 503"),
			wantIntent: "unclear",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeIntentLLM{response: tc.llmResponse, err: tc.llmErr}
			verdict, err := ClassifyEditIntent(context.Background(), fake, tc.body)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil verdict %+v", verdict)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if verdict.Intent != tc.wantIntent {
				t.Errorf("intent = %q, want %q", verdict.Intent, tc.wantIntent)
			}
			if tc.expectNoCall && fake.calls != 0 {
				t.Errorf("LLM called %d times for empty body, want 0", fake.calls)
			}
			if tc.wantErr {
				if verdict.Confidence != 0 {
					t.Errorf("error-path confidence = %v, want 0", verdict.Confidence)
				}
				if verdict.Summary != "" {
					t.Errorf("error-path summary = %q, want empty", verdict.Summary)
				}
				return
			}
			if verdict.Confidence < tc.minConf {
				t.Errorf("confidence = %v, want >= %v", verdict.Confidence, tc.minConf)
			}
			if tc.wantSummary && strings.TrimSpace(verdict.Summary) == "" {
				t.Errorf("expected non-empty English summary, got %q", verdict.Summary)
			}
		})
	}
}

func TestClassifyEditIntentRejectsBadShapes(t *testing.T) {
	for _, raw := range []string{
		`{"intent":"push","confidence":0.9,"summary":"x"}`,
		`{"intent":"commit","confidence":7,"summary":"x"}`,
		`{"intent":"commit","confidence":0.9}`,
		``,
	} {
		fake := &fakeIntentLLM{response: raw}
		verdict, err := ClassifyEditIntent(context.Background(), fake, "fix it")
		if raw == `{"intent":"commit","confidence":0.9}` {
			// Missing summary decodes to empty string: shape is still valid.
			if err != nil {
				t.Errorf("raw %q: unexpected error: %v", raw, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("raw %q: expected error, got %+v", raw, verdict)
		}
		if verdict.Intent != "unclear" || verdict.Confidence != 0 {
			t.Errorf("raw %q: error path must be unclear/0, got %+v", raw, verdict)
		}
	}
}

func TestClassifyEditIntentTruncatesLongInput(t *testing.T) {
	fake := &fakeIntentLLM{response: `{"intent":"question","confidence":0.8,"summary":"Long question"}`}
	long := strings.Repeat("a", maxIntentInputChars+100)
	if _, err := ClassifyEditIntent(context.Background(), fake, long); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.lastUser) > maxIntentInputChars+64 {
		t.Errorf("input not bounded: %d chars sent", len(fake.lastUser))
	}
	if !strings.Contains(fake.lastUser, "[truncated]") {
		t.Errorf("expected truncation marker in bounded input")
	}
}
