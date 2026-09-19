# pr-review-go

High-performance, sandbox-verified AI PR Reviewer written in Go.

Inspired by GitHub Copilot's agentic code review architecture and PR-Agent:
1. **Live Sandbox Verification**: Clones PR branch into an isolated temporary workspace and executes real compilers (`go build`, `cargo check`) and unit tests (`go test`, `npm test`, `pytest`) to eliminate hallucinations and verify claims empirically.
2. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
3. **Structured Findings**: Outputs deterministic JSON findings with severity ratings, code locations, and actionable suggestions.
4. **Daemonless & Lightweight**: Consumes only ~10-15 MB RAM as a daemon (vs 400-800 MB Python/Uvicorn setups).

## Architecture

```
cmd/
  pr-review-go/        # Dual-mode CLI & Server daemon
  pr-review-server/    # Dedicated Webhook server binary
pkg/
  config/              # Environment & LLM configuration
  github/              # GitHub API client (PRs, diffs, comments, discussions)
  sandbox/             # Ephemeral workspace runner for compilation & test verification
  llm/                 # OpenAI-compatible / LiteLLM API client
  reviewer/            # Review orchestrator, prompt synthesis, and report formatting
  server/              # GitHub Webhook server with HMAC validation & background runner
deploy/
  pr-review.service   # Systemd unit template for Proxmox CT / Linux server
```

## Quick Start

### 1. Build
```bash
go build -o bin/pr-review-go ./cmd/pr-review-go
```

### 2. Manual CLI Mode
```bash
export GITHUB_TOKEN="ghp_..."
export LLM_BASE_URL="http://192.168.1.129:4000/v1"
export LLM_MODEL="openai/agy/claude-sonnet-4-6"

# Review a pull request directly
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42

# Review and post results directly as a comment on the PR
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -post
```

### 3. 7/24 Webhook Daemon Mode (GitHub App / Webhook)
```bash
export WEBHOOK_SECRET="your-hmac-secret"
export PORT=3000

# Start server
./bin/pr-review-go -server
```

When running in server mode:
- `GET /` -> `{"status":"ok"}` healthcheck
- `POST /api/v1/github_webhooks` -> Validates HMAC SHA256 and handles:
  - `pull_request`: `opened`, `synchronize`, `reopened` (auto-reviews)
  - `issue_comment`: comments starting with `/review` (on-demand review)
