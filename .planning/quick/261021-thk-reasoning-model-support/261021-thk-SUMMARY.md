---
phase: 261021-thk-reasoning-model-support
plan: "01"
subsystem: llm
tags: [llm, reasoning, recovery, token-budget, config]
requires: []
provides:
  - "Reasoning and thinking LLM model support (evren/deepseek-v4.1-flash, DeepSeek R1) across all bot subsystems"
  - "ChatMessage with ReasoningContent and ChatRequest with MaxTokens"
  - "Automatic single bounded retry with doubled max_tokens (capped at 131072) when token budget is exhausted during thinking"
  - "Strict reasoning_content non-leakage to public reviews, comments, and summaries"
  - "Typed EmptyAssistantContentError sentinel unwrapping ErrEmptyAssistantContent with actionable remediation"
  - "LLMMaxTokens config setting and LLM_MAX_TOKENS environment parsing with bounds checking"
affects:
  - pkg/llm
  - pkg/config
tech-stack:
  added: []
  patterns:
    - "Separate request and response message semantics with omitempty for reasoning_content"
    - "Bounded 1-retry exponential recovery for length-exhausted thinking models"
    - "Typed error wrapping stable sentinel for auditability and errors.Is compatibility"
key-files:
  created:
    - pkg/llm/reasoning_test.go
    - .planning/quick/261021-thk-reasoning-model-support/261021-thk-SUMMARY.md
  modified:
    - pkg/llm/client.go
    - pkg/config/config.go
    - pkg/config/config_test.go
decisions:
  - "Reasoning scratchpad text is strictly discarded rather than used as a fallback to prevent JSON corruption and confidential prompt/code leakage."
  - "Token budget recovery is strictly bounded to 1 retry with a 2x multiplier capped at 131072 (MaxLLMMaxTokens) to prevent runaway request loops."
  - "Config.LLMMaxTokens defaults to 0 when unset or invalid, letting provider defaults apply unless explicitly configured."
  - "ValidateCapacity validates LLMMaxTokens within [MinLLMMaxTokens (256), MaxLLMMaxTokens (131072)] when > 0, treating 0 as valid unset/default."
metrics:
  completed: "2026-10-21"
status: complete
---

# Quick Task 261021-thk: Reasoning Model Support Summary

Resolved the production defect observed in job-00000074 ("empty assistant response content") by introducing first-class support for reasoning models (e.g. `evren/deepseek-v4.1-flash`, DeepSeek R1) with bounded token recovery, non-leakage guarantees, and `LLM_MAX_TOKENS` configuration.

## Root Cause & Defect Resolution

When calling reasoning/thinking models without an explicit or sufficient `max_tokens` budget, the model consumes its entire token allowance generating internal thinking steps. Upon hitting the limit, it returns:
```json
{
  "finish_reason": "length",
  "choices": [{
    "message": {
      "role": "assistant",
      "content": null,
      "reasoning_content": "..."
    }
  }]
}
```
Previously, `content: null` unmarshaled to `""`, `reasoning_content` was dropped, and `client.go` failed immediately with a generic `empty assistant response content` error.

## Implementation Details

### 1. `pkg/llm/client.go`
- Added `ReasoningContent string json:"reasoning_content,omitempty"` to `ChatMessage`.
- Added `MaxTokens *int json:"max_tokens,omitempty"` to `ChatRequest`.
- Defined constants `DefaultLLMMaxTokens = 4096` and `MaxLLMMaxTokens = 131072`.
- Added `maxTokens int` field to `Client` with `SetMaxTokens(tokens int)` and `MaxTokens() int` accessors.
- Defined `ErrEmptyAssistantContent` stable sentinel and `EmptyAssistantContentError` typed error recording model name, finish reason, reasoning presence, attempted budget, and retry state.
- Implemented `completeWithRecovery`:
  - When content is non-empty, returns content directly. `reasoning_content` is never returned.
  - When content is empty with `finish_reason == "length"` and reasoning is present, retries once with doubled token budget (capped at `MaxLLMMaxTokens = 131072`).
  - If content remains empty after retry or for other finish reasons, returns typed `EmptyAssistantContentError` unwrapping `ErrEmptyAssistantContent`.
- Implemented `maxTokensFromEnv()` parsing `LLM_MAX_TOKENS` clamped to `MaxLLMMaxTokens`.

### 2. `pkg/config/config.go`
- Defined constants `MinLLMMaxTokens = 256` and `MaxLLMMaxTokens = 131072`.
- Added `LLMMaxTokens int json:"llm_max_tokens"` field to `Config`.
- In `Load()`, parsed `LLM_MAX_TOKENS` into `LLMMaxTokens` (defaults to 0 if unset or invalid).
- In `ValidateCapacity()`, verified that when `LLMMaxTokens > 0`, it falls within `[MinLLMMaxTokens, MaxLLMMaxTokens]`, rejecting out-of-bounds values.

### 3. Verification & Tests
- `pkg/llm/reasoning_test.go`:
  - `TestChatCompletion_ReasoningTokenExhaustionRetrySuccess`: verified single retry with doubled `max_tokens` when `finish_reason="length"` with reasoning content.
  - `TestChatCompletion_ReasoningTokenExhaustionRetryFailure`: verified retry failure returns `EmptyAssistantContentError` with actionable remediation guidance.
  - `TestChatCompletion_EmptyContentStopWithoutReasoning`: verified stop without reasoning returns typed error with `HasReasoning=false`.
  - `TestChatCompletion_EmptyContentStopWithReasoning`: verified stop with reasoning returns typed error with `HasReasoning=true`.
  - `TestChatCompletion_ReasoningContentNotLeaked`: verified reasoning content is strictly excluded from return output.
  - `TestChatCompletion_MaxTokensRequestSerialization`: verified `max_tokens` presence when configured and omission when 0.
  - `TestMaxTokensFromEnv`: verified env parsing, clamping, and invalid input handling.
- `pkg/config/config_test.go`:
  - `TestLLMMaxTokens`: table-driven tests for direct validation bounds and `Load()` env parsing (unset, invalid, negative, valid, min/max bounds, out-of-bounds).
- All packages passed `go test -race -count=1 ./...` and `go vet ./...`.
