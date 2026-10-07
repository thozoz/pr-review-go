# pr-review-go

AI PR Reviewer and read-only Interactive Assistant written in Go. Project build/test verification runs inside rootless Podman containers on Linux with dedicated 3 GiB storage slots and cgroups v2 boundaries; on non-Linux platforms (macOS/Windows) or unprovisioned systems, verification is safely reported as unavailable. Assistant mutations and one-click code suggestions remain permanently disabled.

Inspired by GitHub Copilot's agentic architecture and PR-Agent:
1. **Isolated Verification & Truthful Results**: On Linux with operator-configured Podman, PR builds and tests execute inside disposable rootless containers (`network=none`, 2 GiB RAM, 3 GiB fixed ext4 slot leases). On macOS, Windows, or systems without operator sandboxing, verification truthfully reports `unavailable` with 0 commands executed on the host. See [docs/SANDBOX.md](docs/SANDBOX.md).
2. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
3. **Discussion Summarizer (`/summarize`)**: Instantly compiles all PR comments, debates, and reviewer feedback into an executive summary table (zero sandbox overhead).
4. **Read-only PR Assistant (`@bot` / `/ask`)**: Allows repository writers to ask about PR files, using `read_file` and `list_files` within an `os.OpenRoot` confined snapshot. File writes, command execution, and pushes are permanently disabled.
5. **SHA-256 Yorum Deduplication (`pkg/dedup`)**: Prevents duplicate spam findings across multiple `/review` runs on the same PR using deterministic SHA-256 fingerprinting.
6. **Documentation Report (`/add_docs`, `pkg/docgen`)**: Scans PR code for undocumented functions/types and reports missing comments without modifying files.
7. **Effort Levels (`lite` vs `balanced`)**: Choose between fast diff-only inspection (`-effort lite`) and workspace-assisted review (`-effort balanced`).
8. **Auto-Labeler (`/labels`)**: Context-aware categorization using PR title, description, and diff.
9. **Atomic Changelog Updates (`/update-changelog`)**: Compares immutable commit SHAs and commits via GitHub GraphQL CAS (`createCommitOnBranch` with `expectedHeadOid`). Aborts cleanly if branch moves.
10. **Daemonless & Lightweight**: Consumes only ~10-15 MB RAM as a daemon.

## Architecture

```
cmd/
  pr-review-go/        # Dual-mode CLI & Server daemon
  pr-review-server/    # Dedicated Webhook server binary
  pr-review-sandbox-helper/ # Trusted helper binary inside container
pkg/
  config/              # Environment & LLM configuration
  github/              # GitHub API client (PRs, diffs, comments, discussions, GraphQL CAS)
  sandbox/             # Rootless Podman container runner (Linux) & API snapshot provider (macOS/Win)
  llm/                 # OpenAI-compatible / LiteLLM API client
  reviewer/            # Review orchestrator, prompt synthesis, and report formatting
  summarizer/          # Fast PR discussion & consensus summarizer
  assistant/           # Read-only interactive PR assistant (read_file, list_files)
  labeler/             # Auto-labeler based on PR contents
  changelog/           # Atomic changelog generation and expected-head CAS publication
  server/              # GitHub Webhook server with HMAC validation & background runner
deploy/
  sandbox/             # Trusted Containerfile and helper build recipe
  pr-review.service   # Systemd unit template for Proxmox CT / Linux server
```

## Quick Start

### Install a release

Version tags such as `v1.0.0` trigger tests, then publish a GitHub Release with
Linux, macOS, and Windows archives for amd64 and arm64. Each archive contains
`pr-review-go` and `pr-review-server`; `checksums.txt` contains SHA-256 checksums.
Both binaries accept `-version` without requiring credentials.

The same tag publishes a multi-platform server image:

```bash
docker run --rm --env-file .env -p 3000:3000 ghcr.io/thozoz/pr-review-go:1.0.0
```

Images use explicit version tags; stable releases also update `latest`.
Pre-releases do not replace `latest`.
The Docker image includes Git for workspace cloning. Packaging does not change
the documented host-clone and filesystem isolation limits of PR review.

After npm publishing is enabled, install both commands with:

```bash
npm install -g @thozoz/pr-review-go
pr-review-go -version
pr-review-server -version
```

The npm wrapper selects one of six platform packages through optional dependencies.
Do not install with `--omit=optional`. npm publication is initially disabled;
see [release setup](docs/RELEASING.md) for the one-time publisher configuration.

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

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
