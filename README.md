# pr-review-go

Multi-language AI PR Reviewer and Autonomous Assistant written in Go. Designed to inspect, test, and edit pull requests across any programming language (Go, Python, Node.js/TypeScript, Rust, Java/Maven/Gradle, Make, and more). Supports confined direct execution (`SANDBOX_MODE=direct`, fail-closed `bwrap-ro` → `landlock` confinement on Linux) for pre-isolated environments (Proxmox LXC, Docker, Kubernetes, CI runners) as well as rootless Podman container isolation on Linux.

Inspired by GitHub Copilot's agentic architecture and PR-Agent:
1. **Multi-Language Verification**: Detects project ecosystem (`go.mod`, `package.json`, `pyproject.toml`/`requirements.txt`, `Cargo.toml`, `pom.xml`, `build.gradle`, `Makefile`) and runs appropriate build and test verification suites.
2. **Flexible Sandbox Modes**: Choose between disposable rootless Podman container isolation (`SANDBOX_MODE=podman`) or containerless direct execution (`SANDBOX_MODE=direct`) in pre-isolated environments like Proxmox LXC.
3. **Review Discussion & Thread Awareness**: Gathers existing PR comments and inline review discussions. Tracks whether previous review feedback was addressed in new commits and prevents repeating resolved debates.
4. **Discussion Summarizer (`/summarize`)**: Instantly compiles all PR comments, debates, and reviewer feedback into an executive summary table (zero sandbox overhead).
5. **Interactive PR Assistant & Gated Editing**: Responds to questions about PR code and securely applies requested modifications with automated verification before non-force commits.
6. **SHA-256 Yorum Deduplication (`pkg/dedup`)**: Prevents duplicate spam findings across multiple `/review` runs on the same PR using deterministic SHA-256 fingerprinting.
7. **Documentation Report (`/add_docs`, `pkg/docgen`)**: Scans PR code for undocumented functions/types and reports missing comments without modifying files.
8. **Effort Levels (`lite` vs `balanced`)**: Choose between fast diff-only inspection (`-effort lite`) and workspace-assisted review (`-effort balanced`).
9. **Auto-Labeler (`/labels`)**: Context-aware categorization using PR title, description, and diff.
10. **Atomic Changelog Updates (`/update_changelog`)**: Compares immutable commit SHAs and commits via GitHub GraphQL CAS (`createCommitOnBranch` with `expectedHeadOid`). Aborts cleanly if branch moves.
11. **Daemonless & Lightweight**: Consumes only ~10-15 MB RAM as a daemon.

## Architecture

```
cmd/
  pr-review-go/        # Dual-mode CLI & Server daemon
  pr-review-server/    # Dedicated Webhook server binary
  pr-review-sandbox-helper/ # Trusted direct-mode confinement helper (landlock-run applier)
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
  server/              # GitHub Webhook server with HMAC validation, durable bbolt queue & fixed workers
deploy/
  sandbox/             # Trusted Containerfile and helper build recipe
  pr-review.service   # Systemd unit template for Proxmox CT / Linux server
```

## Execution mode (default: no Podman)

If `SANDBOX_MODE` is unset or blank, verification uses **`direct`**. Podman
is optional and must be explicitly selected:

```bash
SANDBOX_MODE=direct  # default: run build/test commands on the execution host, confined
SANDBOX_MODE=podman  # opt in to Podman isolation; provision it separately
```

A Podman failure never falls back to direct mode: the selected backend either
works or reports unavailable.

### The fail-closed confinement chain

Direct mode on Linux wraps every stage — stage-0 dependency preparation, build,
test and in-loop commands — in **exactly one** filesystem confinement rank:

```
bwrap-ro  ──►  landlock  ──►  typed ErrConfinementUnavailable
```

- The rank policy is resolved **once per process**; enforcement is proved **per execution**.
- When no rank is available the run executes **zero** commands and reports
  `StatusUnavailable` with the confinement-unavailable label
  (`CONFINEMENT-UNAVAILABLE: no filesystem confinement rank attested; zero commands executed`),
  wrapping the typed `ErrConfinementUnavailable`. It never runs unconfined and is never retried unwrapped.
- A denial — the helper's negative control, or an attributable `EROFS`/`EACCES`/`EXDEV`
  under the active rank — is reported as `StatusConfinementDenied`, not as a build or
  test failure, and is not retried.

Per-rank guarantee summary:

- **`bwrap-ro`** — host root read-only (`--ro-bind / /`), private `/tmp`, only the job
  workspace/tmp/home/cache writable, network removed (`--unshare-net`).
- **`landlock`** — kernel filesystem ruleset applied by the trusted helper; no user
  namespaces, cgroups or daemon; **no network removal**.

**Security:** direct mode still runs repository code and build/test scripts on the
execution host, inside the host's own filesystem, PID and credential world. The
confinement is a filesystem **write** boundary: it does not hide host files (reads are
permitted by design), and the `landlock` rank does not remove the network. Use a
dedicated, pre-isolated worker without production secrets; a shared LXC alone does not
protect the bot's credentials from untrusted code. Do not run untrusted PRs on an
ordinary shared host. Set `SANDBOX_MODE=podman` explicitly where container isolation is
required. Existing explicit Podman configuration remains effective.

Podman has **not worked in our tested nested Proxmox LXC environment**: cgroup
controller delegation and UID/GID mapping errors blocked execution. This is an
environment-specific open limitation, not a claim that Podman fails everywhere.
See [issue #20](https://github.com/thozoz/pr-review-go/issues/20) for tracking.
`ALLOW_ROOTFUL_SANDBOX` remains disabled by default; it is not needed in direct mode.

### Direct-mode confinement prerequisites

#### `bwrap-ro` — install Bubblewrap

```bash
apt-get install -y bubblewrap   # Debian 12/13; tested bubblewrap 0.12.0-1~deb13u1
```

That is the entire installation story for the working rank. It additionally needs
unprivileged user namespaces, which `unshare -Urn true` verifies. Nothing else requires root.

#### Go toolchain: set `GOROOT` explicitly on the execution host

The `bwrap-ro` rank pins the *running process's* Go toolchain into the sandbox, because a
build or `go test` inside the sandbox must find a compiler that the host trusts. That path
comes from `runtime.GOROOT()`, which falls back to the **build machine's** absolute Go path
whenever the environment does not override it. A binary compiled on a developer machine and
shipped to a host with a different filesystem layout therefore points at a directory that
does not exist, and every direct-mode verification degrades to `StatusUnavailable` with zero
commands executed — the review still succeeds, but nothing is compiled or tested.

Deployments must therefore name the toolchain explicitly. Omitting this is the single most
common cause of "verification silently did nothing" in direct mode:

```bash
# /etc/pr-review-go/env (or any env file read before start)
GOROOT=/opt/go          # the Go root on THIS host; must contain bin/go and src/
```

Verify it points at a real toolchain before starting the service:

```bash
test -x "$GOROOT/bin/go" && test -d "$GOROOT/src" && "$GOROOT/bin/go" env GOROOT
```

`-trimpath` is **not** a substitute: it makes `runtime.GOROOT()` resolve to an empty string,
which fails the same check from the other direction. Do not build release binaries with
`-trimpath` while relying on direct-mode Go verification.

#### `landlock` — install nothing

No user namespaces, subuid/subgid, cgroups or daemon are required; the ruleset is applied
by the shipped trusted helper `pr-review-sandbox-helper`. It needs a kernel ≥ 5.13 with
Landlock present in the boot LSM list.

#### Proxmox LXC host prerequisite for Landlock

On the **PVE host** — **this cannot be performed from inside the LXC container** — do the following:

1. Append `lsm=landlock,lockdown,yama,integrity,apparmor` to the existing kernel command
   line in `/etc/kernel/cmdline` (keep the existing entries; do not replace them):
   ```bash
   # On the PVE host, not in the container:
   sed -i 's/$/ lsm=landlock,lockdown,yama,integrity,apparmor/' /etc/kernel/cmdline
   ```
2. Refresh the boot configuration:
   ```bash
   proxmox-boot-tool refresh
   ```
3. Reboot the PVE host.
4. Verify:
   ```bash
   dmesg | grep landlock   # expect: landlock: Up and running.
   ```

#### Landlock unavailability diagnostics

The code maps each kernel observation to one specific typed reason:

| Observation | Meaning | Action |
|---|---|---|
| `EOPNOTSUPP` (`disabled_at_boot`) | Landlock is compiled in but disabled at boot | The host `lsm=` + `proxmox-boot-tool refresh` + reboot step above |
| `ENOSYS` (`kernel_unsupported`) | Kernel predates 5.13, or `CONFIG_SECURITY_LANDLOCK` is unset | Upgrade the kernel or use `bwrap-ro` |
| `EPERM` (`blocked`) | seccomp / capability misconfiguration | Review the container's seccomp and privilege policy |
| ABI < 2 (`abi_too_old`) | Landlock is present but `REFER` cannot be granted | Unavailable for this workload; use `bwrap-ro` |

Runnable checks:

```bash
bwrap --version
go test ./pkg/sandbox/ -run 'TestOnHostLandlockRankUnavailableReason' -count=1 -v
```

The second command **records** the typed on-host reason rather than failing, so it is safe
to run on a host where Landlock is disabled.

#### Configuration knobs

- `SANDBOX_DIRECT_WORK_ROOT` — job-private work root (default `<WebhookStateDir>/direct`).
- `SANDBOX_DIRECT_CACHE_DIR` — job-private cache root (default `<WebhookStateDir>/direct/cache`).
- `SANDBOX_HELPER_PATH` — operator override for the trusted helper. Resolution order is
  `SANDBOX_HELPER_PATH` → sibling of the server binary → `PATH` → identity-guarded
  self-exec. The helper ships in the release archives for linux/amd64 and linux/arm64.

Following these steps on any host must end in either a confined run or one specific typed
unavailable reason per rank — never in an accepted unconfined execution.

## Optional model fallback

```bash
export LLM_MODEL="primary-model"
export LLM_FALLBACK_MODELS="backup-model-1,backup-model-2"
```

The comma-separated fallback chain is opt-in (empty by default), ordered, and
uses the same configured API endpoint and credentials. After retries for a model
fail with HTTP 429 or 5xx, the client tries the next model. Authentication errors,
invalid requests, network failures, malformed/empty responses and cancellation
are not fallback triggers. Duplicate/blank names are ignored. Each model retains
the existing retry policy; the caller's context/deadline bounds the whole chain.
Every new call starts with the primary model (no permanent model switch).

Only list models/providers you approve: fallback may incur additional charges
and send the same PR content to a different provider behind your proxy.

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
Do not install with `--omit=optional`. npm publication is initially disabled and requires one-time npm Trusted Publisher configuration.

### 1. Build
```bash
go build -o bin/pr-review-go ./cmd/pr-review-go
go build -o bin/pr-review-server ./cmd/pr-review-server
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

# Optional durable queue and capacity controls (defaults shown; WEBHOOK_STATE_DIR defaults to platform os.UserConfigDir()/pr-review-go/state):
export WEBHOOK_STATE_DIR="$HOME/.config/pr-review-go/state"
export WEBHOOK_WORKERS=2
export WEBHOOK_BACKLOG=100
export LLM_CONCURRENCY=2
export LLM_MIN_INTERVAL=1s
export SANDBOX_CONCURRENCY=1

./bin/pr-review-server
```

When running in server mode:
- **Webhook secret must match the GitHub App's own secret**: deliveries are validated against `WEBHOOK_SECRET`, so a mismatch rejects every event with `401 invalid signature` while the service otherwise looks healthy. A repo-level webhook is not involved when the bot is installed as a GitHub App — the secret on the App configuration page and the value in the environment file must be aligned (or rotated together) and the service restarted. A failing mismatch is visible in the log as `[webhook] HMAC validation failed`.
- **Durable bbolt Ledger & Bounded Retention**: Webhooks commit delivery receipts and job bundles atomically to `jobs.db` before returning HTTP 200. Bounded maintenance reclaims expired terminal records without evicting active work. Saturated queues return HTTP 503 (`Retry-After: 30`).
- **Per-PR FIFO Serialization**: Work is serialized per pull request; distinct PRs process in parallel across fixed workers.
- **Automatic Review Coalescing**: Subsequent commits during an active review coalesce into a single follow-up review of the latest commit; outdated reports are discarded before publication.
- **Status Comment Recycling & Outbox**: A dedicated background StatusOutbox publishes durable queued status comments immediately; transitions recycle the same status comment per PR.
- **Rerun Protection**: Repeated comment deliveries are deduplicated within the retention window; fresh `/review` comments on already-reviewed commits explicitly rerun and display `"This commit was already reviewed. Reviewing again."`.
- **Offline Disaster Recovery**: Blocked or crashed jobs can be inspected and resolved using `WEBHOOK_STATE_DIR=/path ./bin/pr-review-server --queue-inspect` and `--queue-resolve` (`confirmed`, `rerun`, `cancel`). Full configuration tables and recovery runbooks ship with the local operator docs.

Incoming webhook triggers:
- `pull_request`: `opened` -> auto-labels + code review
- `pull_request`: `synchronize` -> coalesced incremental review
- Comment `/review` -> triggers code review, including project build/test verification when
  the sandbox is available (see the `GOROOT` requirement above; an unavailable sandbox is
  reported as such in the review body rather than silently skipped)
- Comment `/approve` -> publishes an APPROVE review from the bot identity, bound to the exact head SHA at execution time. Command-triggered only; automatic reviews stay COMMENT. One decision per head commit: a duplicate command for the same head is rejected with a warning comment, and a new commit requires a fresh command (the old record stays on the old commit in the GitHub UI). Whether the approval satisfies required reviews depends on repo branch protection; stale-approval dismissal on new commits ("Dismiss stale pull request approvals when new commits are pushed") is a repo setting the bot cannot control.
- Comment `/request_changes` -> publishes a REQUEST_CHANGES review under the same exact-head, one-decision-per-head, and bot-identity rules as `/approve`.
- Comment `/improve` -> posts an unavailable notice; no suggestions are generated
- Comment `/describe` -> appends a purpose and file walkthrough to PR body. Mermaid architecture diagrams are opt-in (default OFF): set `ENABLE_MERMAID=1` or run an explicit `/describe` command to include a diagram; author text is never modified and re-runs replace only the diagram block.
- Comment `/add_docs` or `/docs` -> posts documentation suggestions as a PR comment. Permanently report-only: never modifies code, commits, or pushes, including after any future push work.
- Comment `/update_changelog` -> commits one changelog entry when author has not edited it
- Comment `/summarize` or `/summary` -> triggers discussion summary
- Comment `/labels` or `/generate_labels` -> triggers label generation
- Comment `@bot <task>`, `@pr-review <task>`, or `/ask <task>` -> launches interactive sandbox assistant

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
