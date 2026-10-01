# pr-review-go

AI PR Reviewer and read-only Interactive Assistant written in Go. Project build/test verification and one-click suggestions are disabled until isolated execution is configured.

Inspired by GitHub Copilot's agentic architecture and PR-Agent:
1. **Project Verification (disabled)**: PR source is cloned into a temporary workspace, but build and test commands are skipped until container isolation is configured. Reviews must not claim live verification.
2. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
3. **Discussion Summarizer (`/summarize`)**: Instantly compiles all PR comments, debates, and reviewer feedback into an executive summary table (zero sandbox overhead).
4. **Read-only PR Assistant (`@bot` / `/ask`)**: Allows repository writers to ask about PR files, using `read_file` and `list_files`. File writes, command execution, and pushes are unavailable.
5. **SHA-256 Yorum Deduplication (`pkg/dedup`)**: Prevents duplicate spam findings across multiple `/review` runs on the same PR using deterministic SHA-256 fingerprinting.
6. **Documentation Report (`/add_docs`, `pkg/docgen`)**: Scans PR code for undocumented functions/types and reports missing comments without modifying files.
7. **Effort Levels (`lite` vs `balanced`)**: Choose between fast diff-only inspection (`-effort lite`) and workspace-assisted review (`-effort balanced`); neither runs PR build/tests.
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
  sandbox/             # Ephemeral workspace runner; build/test verification currently disabled
  llm/                 # OpenAI-compatible / LiteLLM API client
  reviewer/            # Review orchestrator, prompt synthesis, and report formatting
  summarizer/          # Fast PR discussion & consensus summarizer
  assistant/           # Read-only interactive PR assistant (read_file, list_files)
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

# Choose actions that run automatically when a PR opens.
# Omit AUTO_ACTIONS for the default: review,labels.
export AUTO_ACTIONS="review,labels,describe"

# Review a pull request directly
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42

# Summarize existing discussions on a PR (no sandbox needed, fast)
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -summary

# Ask read-only assistant about PR code (no commands are run)
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -ask "Explain the changes in server.go"

# Only generate and apply labels
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -labels

# Append an AI-generated purpose and file walkthrough to the PR body
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -describe

# Add and commit one Keep a Changelog entry (skips author edits)
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -update-changelog
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
- Comment `/review` -> triggers code review; project build/tests remain disabled
- Comment `/improve` -> posts an unavailable notice; no suggestions are generated
- Comment `/describe` -> appends a purpose and file walkthrough to PR body
- Comment `/update_changelog` -> commits one changelog entry when author has not edited it

`AUTO_ACTIONS` accepts `review`, `labels`, `describe`, and `improve`, but `improve` is currently skipped. Set it empty
to disable automatic actions. `review` also runs on later PR updates when selected;
other automatic actions run only when the PR opens. Comment commands require repository write permission.
- Comment `/summarize` or `/summary` -> triggers discussion summary
- Comment `/labels` or `/generate_labels` -> triggers label generation
- Comment `@bot <task>`, `@pr-review <task>`, or `/ask <task>` -> launches interactive sandbox assistant
