# pr-review-go

High-performance, sandbox-verified AI PR Reviewer written in Go.

Inspired by GitHub Copilot's agentic code review architecture and PR-Agent, but built with:
1. **Live Sandbox Verification**: Clones PR branch into an isolated environment and executes real compilers (`go build`, `cargo check`) and unit tests (`go test`, `npm test`, `pytest`) to eliminate hallucinations and verify claims empirically.
2. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
3. **Structured Findings**: Outputs deterministic JSON findings with severity ratings, code locations, and actionable suggestions.

## Architecture

```
cmd/
  pr-review-go/        # CLI entrypoint
pkg/
  config/              # Environment & LLM configuration
  github/              # GitHub API client (PRs, diffs, comments, discussions)
  sandbox/             # Ephemeral workspace runner for compilation & test verification
  llm/                 # OpenAI-compatible / LiteLLM API client
  reviewer/            # Review orchestrator, prompt synthesis, and report formatting
```

## Quick Start

### 1. Build
```bash
go build -o bin/pr-review-go ./cmd/pr-review-go
```

### 2. Run
```bash
export GITHUB_TOKEN="ghp_..."
export LLM_BASE_URL="http://192.168.1.129:4000/v1"
export LLM_API_KEY="sk-..."
export LLM_MODEL="openai/agy/claude-sonnet-4-6"

# Review a pull request
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42

# Review and post results directly as a comment on the PR
./bin/pr-review-go -pr https://github.com/owner/repo/pull/42 -post
```
