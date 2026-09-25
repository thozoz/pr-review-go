# pr-review-go

High-performance, sandbox-verified AI PR Reviewer and Interactive Assistant written in Go.

Inspired by GitHub Copilot's agentic architecture and PR-Agent:
1. **Live Sandbox Verification**: Clones PR branch into an isolated temporary workspace and executes real compilers (`go build`, `cargo check`) and unit tests (`go test`, `npm test`, `pytest`) to eliminate hallucinations and verify claims empirically.
2. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
3. **Discussion Summarizer (`/summarize`)**: Instantly compiles all PR comments, debates, and reviewer feedback into an executive summary table (zero sandbox overhead).
4. **Interactive PR Assistant (`@bot` / `/ask`)**: Allows developers to mention `@bot` in comments with instructions like *"run tests in pkg/auth"* or *"read server.go line 40"*. The agent iteratively uses tools (`read_file`, `write_file`, `list_files`, `run_command`, `commit_and_push`) in the sandbox to answer.
5. **SHA-256 Yorum Deduplication (`pkg/dedup`)**: Prevents duplicate spam findings across multiple `/review` runs on the same PR using deterministic SHA-256 fingerprinting.
6. **Automatic Documentation Generator (`/add_docs`, `pkg/docgen`)**: Scans PR code for undocumented functions/types and reports or generates godoc comments.
7. **Effort Levels (`lite` vs `balanced`)**: Choose between fast diff-only inspection (`-effort lite`) and full sandbox-verified deep review (`-effort balanced`).
8. **Auto-Labeler (`/labels`)**: Context-aware categorization using PR title, description, and diff.
9. **Daemonless & Lightweight**: Consumes only ~10-15 MB RAM as a daemon.

## Architecture

```
cmd/
  pr-review-go/        # Dual-mode CLI & Server daemon
  pr-review-server/    # Dedicated Webhook server binary
pkg/
  config/              # Environment & LLM configuration
  github/              # GitHub API client (PRs, diffs, comments, discussions, labels)
  sandbox/             # Ephemeral workspace runner for compilation & test verification
  llm/                 # OpenAI-compatible / LiteLLM API client
  reviewer/            # Review orchestrator, prompt synthesis, and report formatting
  summarizer/          # Fast PR discussion & consensus summarizer
  assistant/           # Tool-enabled interactive PR assistant (read_file, run_command)
  labeler/             # Auto-labeler based on PR contents
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
export GITHUB_TOKEN="ghp_your_github_token_here"
export LLM_BASE_URL="https://api.openai.com/v1"
export LLM_API_KEY="your_api_key_here"
export LLM_MODEL="gpt-4o"

# Review a pull request directly
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42

# Summarize existing discussions on a PR (no sandbox needed, fast)
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -summary

# Ask interactive assistant to inspect or run code
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -ask "Run unit tests and check if there are race conditions"

# Only generate and apply labels
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -labels

# Post safe, inline one-click GitHub suggestions
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -improve

# Append an AI-generated purpose and file walkthrough to the PR body
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -describe
```

### 3. 7/24 Webhook Daemon Mode (GitHub App / Webhook)
```bash
export WEBHOOK_SECRET="your-hmac-secret"
export PORT=3000

./bin/pr-review-go -server
```

When running in server mode, incoming webhook triggers:
- `pull_request`: `opened` -> auto-labels + code review
- `pull_request`: `synchronize` -> incremental review
- Comment `/review` -> triggers full sandbox review
- Comment `/improve` -> posts safe inline GitHub suggestion blocks
- Comment `/describe` -> appends a purpose and file walkthrough to PR body
- Comment `/summarize` or `/summary` -> triggers discussion summary
- Comment `/labels` or `/generate_labels` -> triggers label generation
- Comment `@bot <task>`, `@pr-review <task>`, or `/ask <task>` -> launches interactive sandbox assistant
