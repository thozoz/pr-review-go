package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// EffortLevel determines the speed/rigor of the review process.
	// Accepted values are "lite" (fast, no sandbox) or "balanced" (default, with sandbox verification).
	EffortLevel string `json:"effort_level"` // lite or balanced

	GitHubToken   string
	WebhookSecret string
	Port          int
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	EnableSandbox bool
	// AutoActions run when a PR opens. Nil uses the backward-compatible default:
	// review,labels. An empty slice disables all automatic actions.
	AutoActions             []string
	GitHubAppSetupToken     string
	PublicURL               string
	GitHubAppID             int64
	GitHubAppPrivateKeyPath string

	// Sandbox resource and isolation controls
	SandboxCPUs             float64       `json:"sandbox_cpus"`
	SandboxMemoryBytes      int64         `json:"sandbox_memory_bytes"`
	SandboxPidsLimit        int64         `json:"sandbox_pids_limit"`
	SandboxDiskBytes        int64         `json:"sandbox_disk_bytes"`
	SandboxTimeoutSource    time.Duration `json:"sandbox_timeout_source"`
	SandboxTimeoutPrep      time.Duration `json:"sandbox_timeout_prep"`
	SandboxTimeoutExecution time.Duration `json:"sandbox_timeout_execution"`
	SandboxTimeoutCleanup   time.Duration `json:"sandbox_timeout_cleanup"`
	SandboxImage            string        `json:"sandbox_image"`
	SandboxSlotDir          string        `json:"sandbox_slot_dir"`
	SandboxControlDir       string        `json:"sandbox_control_dir"`

	// Webhook admission and durable state controls
	WebhookStateDir        string        `json:"webhook_state_dir"`
	WebhookWorkers         int           `json:"webhook_workers"`
	WebhookBacklog         int           `json:"webhook_backlog"`
	WebhookDeliveryTTL     time.Duration `json:"webhook_delivery_ttl"`
	WebhookDeliveryLimit   int           `json:"webhook_delivery_limit"`
	WebhookStateMaxBytes   int64         `json:"webhook_state_max_bytes"`
	WebhookBodyMaxBytes    int64         `json:"webhook_body_max_bytes"`
	WebhookOldQueueWarning time.Duration `json:"webhook_old_queue_warning"`
	WebhookShutdownTimeout time.Duration `json:"webhook_shutdown_timeout"`

	// Capacity controls for LLM and sandbox execution
	LLMConcurrency      int           `json:"llm_concurrency"`
	LLMMinInterval      time.Duration `json:"llm_min_interval"`
	LLMResponseMaxBytes int64         `json:"llm_response_max_bytes"`
	SandboxConcurrency  int           `json:"sandbox_concurrency"`

	// Diff limits
	DiffMaxBytes int64 `json:"diff_max_bytes"`
	DiffMaxFiles int   `json:"diff_max_files"`
	DiffMaxHunks int   `json:"diff_max_hunks"`

	// Agent loop limits
	AgentMaxTurns            int   `json:"agent_max_turns"`
	AgentMaxToolBytes        int64 `json:"agent_max_tool_bytes"`
	AgentFileReadBytes       int64 `json:"agent_file_read_bytes"`
	AgentSearchMaxMatches    int   `json:"agent_search_max_matches"`
	AgentModelViolationLimit int   `json:"agent_model_violation_limit"`

	// Retry limits
	RetryMaxAttempts int           `json:"retry_max_attempts"`
	RetryBaseDelay   time.Duration `json:"retry_base_delay"`
	RetryMaxDelay    time.Duration `json:"retry_max_delay"`
	RetryMaxWait     time.Duration `json:"retry_max_wait"`
	RetryJobBudget   time.Duration `json:"retry_job_budget"`

	// Deferral controls for durable parking of long retry waits
	DeferMaxWait      time.Duration `json:"defer_max_wait"`
	DeferPollInterval time.Duration `json:"defer_poll_interval"`
}

const (
	DefaultLLMConcurrency       = 2
	MinLLMConcurrency           = 1
	MaxLLMConcurrency           = 16
	DefaultLLMMinInterval       = 1 * time.Second
	MinLLMMinInterval           = 1 * time.Millisecond
	MaxLLMMinInterval           = 1 * time.Minute
	DefaultLLMResponseMaxBytes  = 1048576 // 1 MiB
	MinLLMResponseMaxBytes      = 65536   // 64 KiB
	MaxLLMResponseMaxBytes      = 16777216 // 16 MiB
	DefaultSandboxConcurrency   = 1
	MinSandboxConcurrency       = 1
	MaxSandboxConcurrency       = 16

	DefaultDiffMaxBytes = 2097152 // 2 MiB
	MinDiffMaxBytes     = 1024    // 1 KiB
	MaxDiffMaxBytes     = 52428800 // 50 MiB
	DefaultDiffMaxFiles = 2000
	MinDiffMaxFiles     = 1
	MaxDiffMaxFiles     = 20000
	DefaultDiffMaxHunks = 20000
	MinDiffMaxHunks     = 1
	MaxDiffMaxHunks     = 200000

	DefaultAgentMaxTurns            = 12
	MinAgentMaxTurns                = 1
	MaxAgentMaxTurns                = 32
	DefaultAgentMaxToolBytes        = 204800 // 200 KiB
	MinAgentMaxToolBytes            = 16384  // 16 KiB
	MaxAgentMaxToolBytes            = 1048576 // 1 MiB
	DefaultAgentFileReadBytes       = 30000  // 30 KB
	MinAgentFileReadBytes           = 1024   // 1 KB
	MaxAgentFileReadBytes           = 1048576 // 1 MiB
	DefaultAgentSearchMaxMatches    = 50
	MinAgentSearchMaxMatches        = 1
	MaxAgentSearchMaxMatches        = 500
	DefaultAgentModelViolationLimit = 3
	MinAgentModelViolationLimit     = 1
	MaxAgentModelViolationLimit     = 10

	DefaultRetryMaxAttempts = 5
	MinRetryMaxAttempts     = 1
	MaxRetryMaxAttempts     = 10
	DefaultRetryBaseDelay   = 1 * time.Second
	MinRetryBaseDelay       = 10 * time.Millisecond
	MaxRetryBaseDelay       = 10 * time.Second
	DefaultRetryMaxDelay    = 30 * time.Second
	MinRetryMaxDelay        = 1 * time.Second
	MaxRetryMaxDelay        = 5 * time.Minute
	DefaultRetryMaxWait     = 60 * time.Second
	MinRetryMaxWait         = 1 * time.Second
	MaxRetryMaxWait         = 10 * time.Minute
	DefaultRetryJobBudget   = 5 * time.Minute
	MinRetryJobBudget       = 10 * time.Second
	MaxRetryJobBudget       = 30 * time.Minute

	DefaultDeferMaxWait     = 15 * time.Minute
	MinDeferMaxWait         = 1 * time.Minute
	MaxDeferMaxWait         = 2 * time.Hour
	DefaultDeferPollInterval = 30 * time.Second
	MinDeferPollInterval     = 1 * time.Second
	MaxDeferPollInterval     = 5 * time.Minute

	DefaultSandboxCPUs             = 2.0
	DefaultSandboxMemoryBytes      = 2147483648 // 2 GiB
	DefaultSandboxPidsLimit        = 256
	DefaultSandboxDiskBytes        = 3221225472 // 3 GiB fixed fallback
	MaxSandboxDiskBytes            = 3221225472 // 3 GiB maximum enforced
	DefaultSandboxTimeoutSource    = 2 * time.Minute
	DefaultSandboxTimeoutPrep      = 3 * time.Minute
	DefaultSandboxTimeoutExecution = 5 * time.Minute
	DefaultSandboxTimeoutCleanup   = 15 * time.Second

	DefaultWebhookWorkers         = 2
	MinWebhookWorkers             = 1
	MaxWebhookWorkers             = 16
	DefaultWebhookBacklog         = 100
	MinWebhookBacklog             = 2
	MaxWebhookBacklog             = 10000
	DefaultWebhookDeliveryTTL     = 168 * time.Hour
	MinWebhookDeliveryTTL         = 1 * time.Hour
	MaxWebhookDeliveryTTL         = 720 * time.Hour
	DefaultWebhookDeliveryLimit   = 20000
	MinWebhookDeliveryLimit       = 100
	MaxWebhookDeliveryLimit       = 1000000
	DefaultWebhookStateMaxBytes   = 67108864 // 64 MiB
	MinWebhookStateMaxBytes       = 16777216 // 16 MiB
	MaxWebhookStateMaxBytes       = 1073741824 // 1024 MiB
	DefaultWebhookBodyMaxBytes    = 1048576 // 1 MiB
	MinWebhookBodyMaxBytes        = 65536 // 64 KiB
	MaxWebhookBodyMaxBytes        = 26214400 // 25 MiB
	DefaultWebhookOldQueueWarning = 24 * time.Hour
	MinWebhookOldQueueWarning     = 1 * time.Hour
	MaxWebhookOldQueueWarning     = 720 * time.Hour
	DefaultWebhookShutdownTimeout = 30 * time.Second
	MinWebhookShutdownTimeout     = 1 * time.Second
	MaxWebhookShutdownTimeout     = 300 * time.Second
)

// Validate checks if required configuration is present and validates EffortLevel
func (c *Config) Validate() error {
	// Set default EffortLevel if empty
	if c.EffortLevel == "" {
		c.EffortLevel = "balanced"
	}
	if c.EffortLevel != "lite" && c.EffortLevel != "balanced" {
		return fmt.Errorf("invalid EffortLevel: %s (must be 'lite' or 'balanced')", c.EffortLevel)
	}
	if c.AutoActions == nil {
		c.AutoActions = []string{"review", "labels"}
	}
	if c.GitHubAppSetupToken != "" {
		if c.PublicURL == "" {
			return fmt.Errorf("PUBLIC_URL is required when GITHUB_APP_SETUP_TOKEN is set")
		}
		return nil
	}
	for _, action := range c.AutoActions {
		switch action {
		case "review", "labels", "describe", "improve":
		default:
			return fmt.Errorf("invalid AUTO_ACTIONS value: %s", action)
		}
	}

	if c.GitHubAppID != 0 || c.GitHubAppPrivateKeyPath != "" {
		if c.GitHubAppID == 0 || c.GitHubAppPrivateKeyPath == "" {
			return fmt.Errorf("GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_PATH must be set together")
		}
	} else if c.GitHubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN or GH_TOKEN is required")
	}
	if c.LLMAPIKey == "" {
		return fmt.Errorf("LLM_API_KEY or OPENAI_API_KEY is required")
	}
	if c.LLMModel == "" {
		return fmt.Errorf("LLM_MODEL is required")
	}
	if c.LLMBaseURL == "" {
		return fmt.Errorf("LLM_BASE_URL is required")
	}

	if c.EnableSandbox {
		if err := c.ValidateSandboxResources(); err != nil {
			return err
		}
	}

	if err := c.ValidateWebhook(); err != nil {
		return err
	}

	if err := c.ValidateCapacity(); err != nil {
		return err
	}

	if err := c.ValidateDiff(); err != nil {
		return err
	}

	if err := c.ValidateAgent(); err != nil {
		return err
	}

	if err := c.ValidateRetry(); err != nil {
		return err
	}

	if err := c.ValidateDefer(); err != nil {
		return err
	}

	return nil
}

// ValidateSandboxResources validates and defaults sandbox CPU, memory, pids, disk, and timeout settings.
func (c *Config) ValidateSandboxResources() error {
	if c.SandboxCPUs == 0 {
		c.SandboxCPUs = DefaultSandboxCPUs
	}
	if c.SandboxCPUs <= 0 {
		return fmt.Errorf("invalid SandboxCPUs: %v (must be positive)", c.SandboxCPUs)
	}

	if c.SandboxMemoryBytes == 0 {
		c.SandboxMemoryBytes = DefaultSandboxMemoryBytes
	}
	if c.SandboxMemoryBytes <= 0 {
		return fmt.Errorf("invalid SandboxMemoryBytes: %d (must be positive)", c.SandboxMemoryBytes)
	}

	if c.SandboxPidsLimit == 0 {
		c.SandboxPidsLimit = DefaultSandboxPidsLimit
	}
	if c.SandboxPidsLimit <= 0 {
		return fmt.Errorf("invalid SandboxPidsLimit: %d (must be positive)", c.SandboxPidsLimit)
	}

	if c.SandboxDiskBytes == 0 {
		c.SandboxDiskBytes = DefaultSandboxDiskBytes
	}
	if c.SandboxDiskBytes <= 0 {
		return fmt.Errorf("invalid SandboxDiskBytes: %d (must be positive)", c.SandboxDiskBytes)
	}
	if c.SandboxDiskBytes > MaxSandboxDiskBytes {
		return fmt.Errorf("invalid SandboxDiskBytes: %d exceeds maximum 3 GiB limit (%d bytes)", c.SandboxDiskBytes, MaxSandboxDiskBytes)
	}

	if c.SandboxTimeoutSource == 0 {
		c.SandboxTimeoutSource = DefaultSandboxTimeoutSource
	}
	if c.SandboxTimeoutSource <= 0 {
		return fmt.Errorf("invalid SandboxTimeoutSource: %v (must be positive)", c.SandboxTimeoutSource)
	}

	if c.SandboxTimeoutPrep == 0 {
		c.SandboxTimeoutPrep = DefaultSandboxTimeoutPrep
	}
	if c.SandboxTimeoutPrep <= 0 {
		return fmt.Errorf("invalid SandboxTimeoutPrep: %v (must be positive)", c.SandboxTimeoutPrep)
	}

	if c.SandboxTimeoutExecution == 0 {
		c.SandboxTimeoutExecution = DefaultSandboxTimeoutExecution
	}
	if c.SandboxTimeoutExecution <= 0 {
		return fmt.Errorf("invalid SandboxTimeoutExecution: %v (must be positive)", c.SandboxTimeoutExecution)
	}

	if c.SandboxTimeoutCleanup == 0 {
		c.SandboxTimeoutCleanup = DefaultSandboxTimeoutCleanup
	}
	if c.SandboxTimeoutCleanup <= 0 {
		return fmt.Errorf("invalid SandboxTimeoutCleanup: %v (must be positive)", c.SandboxTimeoutCleanup)
	}

	return nil
}

// ValidateWebhook validates and defaults webhook admission and durable state settings.
func (c *Config) ValidateWebhook() error {
	if c.WebhookStateDir == "" {
		configDir, err := os.UserConfigDir()
		if err != nil || configDir == "" {
			configDir = os.TempDir()
		}
		c.WebhookStateDir = filepath.Join(configDir, "pr-review-go", "state")
	}

	if c.WebhookWorkers == 0 {
		c.WebhookWorkers = DefaultWebhookWorkers
	}
	if c.WebhookWorkers < MinWebhookWorkers || c.WebhookWorkers > MaxWebhookWorkers {
		return fmt.Errorf("invalid WebhookWorkers: %d (must be between %d and %d)", c.WebhookWorkers, MinWebhookWorkers, MaxWebhookWorkers)
	}

	if c.WebhookBacklog == 0 {
		c.WebhookBacklog = DefaultWebhookBacklog
	}
	if c.WebhookBacklog < MinWebhookBacklog || c.WebhookBacklog > MaxWebhookBacklog {
		return fmt.Errorf("invalid WebhookBacklog: %d (must be between %d and %d)", c.WebhookBacklog, MinWebhookBacklog, MaxWebhookBacklog)
	}

	if c.WebhookDeliveryTTL == 0 {
		c.WebhookDeliveryTTL = DefaultWebhookDeliveryTTL
	}
	if c.WebhookDeliveryTTL < MinWebhookDeliveryTTL || c.WebhookDeliveryTTL > MaxWebhookDeliveryTTL {
		return fmt.Errorf("invalid WebhookDeliveryTTL: %v (must be between %v and %v)", c.WebhookDeliveryTTL, MinWebhookDeliveryTTL, MaxWebhookDeliveryTTL)
	}

	if c.WebhookDeliveryLimit == 0 {
		c.WebhookDeliveryLimit = DefaultWebhookDeliveryLimit
	}
	if c.WebhookDeliveryLimit < MinWebhookDeliveryLimit || c.WebhookDeliveryLimit > MaxWebhookDeliveryLimit {
		return fmt.Errorf("invalid WebhookDeliveryLimit: %d (must be between %d and %d)", c.WebhookDeliveryLimit, MinWebhookDeliveryLimit, MaxWebhookDeliveryLimit)
	}

	if c.WebhookStateMaxBytes == 0 {
		c.WebhookStateMaxBytes = DefaultWebhookStateMaxBytes
	}
	if c.WebhookStateMaxBytes < MinWebhookStateMaxBytes || c.WebhookStateMaxBytes > MaxWebhookStateMaxBytes {
		return fmt.Errorf("invalid WebhookStateMaxBytes: %d (must be between %d and %d)", c.WebhookStateMaxBytes, MinWebhookStateMaxBytes, MaxWebhookStateMaxBytes)
	}

	if c.WebhookBodyMaxBytes == 0 {
		c.WebhookBodyMaxBytes = DefaultWebhookBodyMaxBytes
	}
	if c.WebhookBodyMaxBytes < MinWebhookBodyMaxBytes || c.WebhookBodyMaxBytes > MaxWebhookBodyMaxBytes {
		return fmt.Errorf("invalid WebhookBodyMaxBytes: %d (must be between %d and %d)", c.WebhookBodyMaxBytes, MinWebhookBodyMaxBytes, MaxWebhookBodyMaxBytes)
	}

	if c.WebhookOldQueueWarning == 0 {
		c.WebhookOldQueueWarning = DefaultWebhookOldQueueWarning
	}
	if c.WebhookOldQueueWarning < MinWebhookOldQueueWarning || c.WebhookOldQueueWarning > MaxWebhookOldQueueWarning {
		return fmt.Errorf("invalid WebhookOldQueueWarning: %v (must be between %v and %v)", c.WebhookOldQueueWarning, MinWebhookOldQueueWarning, MaxWebhookOldQueueWarning)
	}

	if c.WebhookShutdownTimeout == 0 {
		c.WebhookShutdownTimeout = DefaultWebhookShutdownTimeout
	}
	if c.WebhookShutdownTimeout < MinWebhookShutdownTimeout || c.WebhookShutdownTimeout > MaxWebhookShutdownTimeout {
		return fmt.Errorf("invalid WebhookShutdownTimeout: %v (must be between %v and %v)", c.WebhookShutdownTimeout, MinWebhookShutdownTimeout, MaxWebhookShutdownTimeout)
	}

	return nil
}

// ValidateCapacity validates and defaults LLM and sandbox concurrency and rate settings.
func (c *Config) ValidateCapacity() error {
	if c.LLMConcurrency == 0 {
		c.LLMConcurrency = DefaultLLMConcurrency
	}
	if c.LLMConcurrency < MinLLMConcurrency || c.LLMConcurrency > MaxLLMConcurrency {
		return fmt.Errorf("invalid LLMConcurrency: %d (must be between %d and %d)", c.LLMConcurrency, MinLLMConcurrency, MaxLLMConcurrency)
	}

	if c.LLMMinInterval == 0 {
		c.LLMMinInterval = DefaultLLMMinInterval
	}
	if c.LLMMinInterval < MinLLMMinInterval || c.LLMMinInterval > MaxLLMMinInterval {
		return fmt.Errorf("invalid LLMMinInterval: %v (must be between %v and %v)", c.LLMMinInterval, MinLLMMinInterval, MaxLLMMinInterval)
	}

	if c.LLMResponseMaxBytes == 0 {
		c.LLMResponseMaxBytes = DefaultLLMResponseMaxBytes
	}
	if c.LLMResponseMaxBytes < MinLLMResponseMaxBytes || c.LLMResponseMaxBytes > MaxLLMResponseMaxBytes {
		return fmt.Errorf("invalid LLMResponseMaxBytes: %d (must be between %d and %d)", c.LLMResponseMaxBytes, MinLLMResponseMaxBytes, MaxLLMResponseMaxBytes)
	}

	if c.SandboxConcurrency == 0 {
		c.SandboxConcurrency = DefaultSandboxConcurrency
	}
	if c.SandboxConcurrency < MinSandboxConcurrency || c.SandboxConcurrency > MaxSandboxConcurrency {
		return fmt.Errorf("invalid SandboxConcurrency: %d (must be between %d and %d)", c.SandboxConcurrency, MinSandboxConcurrency, MaxSandboxConcurrency)
	}

	return nil
}

// ValidateDiff validates and defaults diff budget limits.
func (c *Config) ValidateDiff() error {
	if c.DiffMaxBytes == 0 {
		c.DiffMaxBytes = DefaultDiffMaxBytes
	}
	if c.DiffMaxBytes < MinDiffMaxBytes || c.DiffMaxBytes > MaxDiffMaxBytes {
		return fmt.Errorf("invalid DiffMaxBytes: %d (must be between %d and %d)", c.DiffMaxBytes, MinDiffMaxBytes, MaxDiffMaxBytes)
	}

	if c.DiffMaxFiles == 0 {
		c.DiffMaxFiles = DefaultDiffMaxFiles
	}
	if c.DiffMaxFiles < MinDiffMaxFiles || c.DiffMaxFiles > MaxDiffMaxFiles {
		return fmt.Errorf("invalid DiffMaxFiles: %d (must be between %d and %d)", c.DiffMaxFiles, MinDiffMaxFiles, MaxDiffMaxFiles)
	}

	if c.DiffMaxHunks == 0 {
		c.DiffMaxHunks = DefaultDiffMaxHunks
	}
	if c.DiffMaxHunks < MinDiffMaxHunks || c.DiffMaxHunks > MaxDiffMaxHunks {
		return fmt.Errorf("invalid DiffMaxHunks: %d (must be between %d and %d)", c.DiffMaxHunks, MinDiffMaxHunks, MaxDiffMaxHunks)
	}

	return nil
}

// ValidateAgent validates and defaults agent loop limits.
func (c *Config) ValidateAgent() error {
	if c.AgentMaxTurns == 0 {
		c.AgentMaxTurns = DefaultAgentMaxTurns
	}
	if c.AgentMaxTurns < MinAgentMaxTurns || c.AgentMaxTurns > MaxAgentMaxTurns {
		return fmt.Errorf("invalid AgentMaxTurns: %d (must be between %d and %d)", c.AgentMaxTurns, MinAgentMaxTurns, MaxAgentMaxTurns)
	}

	if c.AgentMaxToolBytes == 0 {
		c.AgentMaxToolBytes = DefaultAgentMaxToolBytes
	}
	if c.AgentMaxToolBytes < MinAgentMaxToolBytes || c.AgentMaxToolBytes > MaxAgentMaxToolBytes {
		return fmt.Errorf("invalid AgentMaxToolBytes: %d (must be between %d and %d)", c.AgentMaxToolBytes, MinAgentMaxToolBytes, MaxAgentMaxToolBytes)
	}

	if c.AgentFileReadBytes == 0 {
		c.AgentFileReadBytes = DefaultAgentFileReadBytes
	}
	if c.AgentFileReadBytes < MinAgentFileReadBytes || c.AgentFileReadBytes > MaxAgentFileReadBytes {
		return fmt.Errorf("invalid AgentFileReadBytes: %d (must be between %d and %d)", c.AgentFileReadBytes, MinAgentFileReadBytes, MaxAgentFileReadBytes)
	}

	if c.AgentSearchMaxMatches == 0 {
		c.AgentSearchMaxMatches = DefaultAgentSearchMaxMatches
	}
	if c.AgentSearchMaxMatches < MinAgentSearchMaxMatches || c.AgentSearchMaxMatches > MaxAgentSearchMaxMatches {
		return fmt.Errorf("invalid AgentSearchMaxMatches: %d (must be between %d and %d)", c.AgentSearchMaxMatches, MinAgentSearchMaxMatches, MaxAgentSearchMaxMatches)
	}

	if c.AgentModelViolationLimit == 0 {
		c.AgentModelViolationLimit = DefaultAgentModelViolationLimit
	}
	if c.AgentModelViolationLimit < MinAgentModelViolationLimit || c.AgentModelViolationLimit > MaxAgentModelViolationLimit {
		return fmt.Errorf("invalid AgentModelViolationLimit: %d (must be between %d and %d)", c.AgentModelViolationLimit, MinAgentModelViolationLimit, MaxAgentModelViolationLimit)
	}

	return nil
}

// ValidateRetry validates and defaults retry attempts, backoff delays, max wait, and job budget.
func (c *Config) ValidateRetry() error {
	if c.RetryMaxAttempts == 0 {
		c.RetryMaxAttempts = DefaultRetryMaxAttempts
	}
	if c.RetryMaxAttempts < MinRetryMaxAttempts || c.RetryMaxAttempts > MaxRetryMaxAttempts {
		return fmt.Errorf("invalid RetryMaxAttempts: %d (must be between %d and %d)", c.RetryMaxAttempts, MinRetryMaxAttempts, MaxRetryMaxAttempts)
	}

	if c.RetryBaseDelay == 0 {
		c.RetryBaseDelay = DefaultRetryBaseDelay
	}
	if c.RetryBaseDelay < MinRetryBaseDelay || c.RetryBaseDelay > MaxRetryBaseDelay {
		return fmt.Errorf("invalid RetryBaseDelay: %v (must be between %v and %v)", c.RetryBaseDelay, MinRetryBaseDelay, MaxRetryBaseDelay)
	}

	if c.RetryMaxDelay == 0 {
		c.RetryMaxDelay = DefaultRetryMaxDelay
	}
	if c.RetryMaxDelay < MinRetryMaxDelay || c.RetryMaxDelay > MaxRetryMaxDelay {
		return fmt.Errorf("invalid RetryMaxDelay: %v (must be between %v and %v)", c.RetryMaxDelay, MinRetryMaxDelay, MaxRetryMaxDelay)
	}

	if c.RetryMaxWait == 0 {
		c.RetryMaxWait = DefaultRetryMaxWait
	}
	if c.RetryMaxWait < MinRetryMaxWait || c.RetryMaxWait > MaxRetryMaxWait {
		return fmt.Errorf("invalid RetryMaxWait: %v (must be between %v and %v)", c.RetryMaxWait, MinRetryMaxWait, MaxRetryMaxWait)
	}

	if c.RetryJobBudget == 0 {
		c.RetryJobBudget = DefaultRetryJobBudget
	}
	if c.RetryJobBudget < MinRetryJobBudget || c.RetryJobBudget > MaxRetryJobBudget {
		return fmt.Errorf("invalid RetryJobBudget: %v (must be between %v and %v)", c.RetryJobBudget, MinRetryJobBudget, MaxRetryJobBudget)
	}

	return nil
}

// ValidateDefer validates and defaults durable-deferral bounds. DeferMaxWait
// caps how far in the future a deferred attempt may be parked; required waits
// beyond it are clamped, never dropped. DeferPollInterval bounds the scheduler
// wake for due deferred attempts.
func (c *Config) ValidateDefer() error {
	if c.DeferMaxWait == 0 {
		c.DeferMaxWait = DefaultDeferMaxWait
	}
	if c.DeferMaxWait < MinDeferMaxWait || c.DeferMaxWait > MaxDeferMaxWait {
		return fmt.Errorf("invalid DeferMaxWait: %v (must be between %v and %v)", c.DeferMaxWait, MinDeferMaxWait, MaxDeferMaxWait)
	}

	if c.DeferPollInterval == 0 {
		c.DeferPollInterval = DefaultDeferPollInterval
	}
	if c.DeferPollInterval < MinDeferPollInterval || c.DeferPollInterval > MaxDeferPollInterval {
		return fmt.Errorf("invalid DeferPollInterval: %v (must be between %v and %v)", c.DeferPollInterval, MinDeferPollInterval, MaxDeferPollInterval)
	}

	return nil
}

func (c *Config) IsGitHubAppSetupMode() bool { return c.GitHubAppSetupToken != "" }

func (c *Config) AutoActionEnabled(action string) bool {
	for _, configured := range c.AutoActions {
		if configured == action {
			return true
		}
	}
	return false
}

func Load() *Config {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}

	webhookSecret := os.Getenv("WEBHOOK_SECRET")
	if webhookSecret == "" {
		webhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	}
	setupToken := os.Getenv("GITHUB_APP_SETUP_TOKEN")
	publicURL := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	appID, _ := strconv.ParseInt(os.Getenv("GITHUB_APP_ID"), 10, 64)

	port := 3000
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			port = p
		}
	}

	llmBaseURL := os.Getenv("LLM_BASE_URL")
	if llmBaseURL == "" {
		llmBaseURL = "https://api.openai.com/v1"
	}

	llmAPIKey := os.Getenv("LLM_API_KEY")
	if llmAPIKey == "" {
		llmAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	llmModel := os.Getenv("LLM_MODEL")
	if llmModel == "" {
		llmModel = "gpt-4o"
	}

	// Determine effort level and sandbox enablement
	effort := os.Getenv("EFFORT_LEVEL")
	if effort == "" {
		effort = "balanced"
	}
	enableSandbox := true
	if effort == "lite" {
		enableSandbox = false
	}

	var autoActions []string
	if raw, set := os.LookupEnv("AUTO_ACTIONS"); set {
		autoActions = []string{}
		for _, action := range strings.Split(raw, ",") {
			action = strings.TrimSpace(strings.ToLower(action))
			if action != "" {
				autoActions = append(autoActions, action)
			}
		}
	}

	sandboxCPUs := DefaultSandboxCPUs
	if raw := os.Getenv("SANDBOX_CPUS"); raw != "" {
		if val, err := strconv.ParseFloat(raw, 64); err == nil {
			sandboxCPUs = val
		} else {
			sandboxCPUs = -1
		}
	}

	sandboxMemoryBytes := int64(DefaultSandboxMemoryBytes)
	if raw := os.Getenv("SANDBOX_MEMORY_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			sandboxMemoryBytes = val
		} else {
			sandboxMemoryBytes = -1
		}
	}

	sandboxPidsLimit := int64(DefaultSandboxPidsLimit)
	if raw := os.Getenv("SANDBOX_PIDS_LIMIT"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			sandboxPidsLimit = val
		} else {
			sandboxPidsLimit = -1
		}
	}

	sandboxDiskBytes := int64(DefaultSandboxDiskBytes)
	if raw := os.Getenv("SANDBOX_DISK_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			sandboxDiskBytes = val
		} else {
			sandboxDiskBytes = -1
		}
	}

	sandboxTimeoutSource := DefaultSandboxTimeoutSource
	if raw := os.Getenv("SANDBOX_TIMEOUT_SOURCE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			sandboxTimeoutSource = d
		} else {
			sandboxTimeoutSource = -1
		}
	}

	sandboxTimeoutPrep := DefaultSandboxTimeoutPrep
	if raw := os.Getenv("SANDBOX_TIMEOUT_PREP"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			sandboxTimeoutPrep = d
		} else {
			sandboxTimeoutPrep = -1
		}
	}

	sandboxTimeoutExecution := DefaultSandboxTimeoutExecution
	if raw := os.Getenv("SANDBOX_TIMEOUT_EXECUTION"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			sandboxTimeoutExecution = d
		} else {
			sandboxTimeoutExecution = -1
		}
	}

	sandboxTimeoutCleanup := DefaultSandboxTimeoutCleanup
	if raw := os.Getenv("SANDBOX_TIMEOUT_CLEANUP"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			sandboxTimeoutCleanup = d
		} else {
			sandboxTimeoutCleanup = -1
		}
	}

	sandboxControlDir := os.Getenv("SANDBOX_CONTROL_DIR")
	if sandboxControlDir == "" {
		sandboxControlDir = "/tmp/pr-review-sandbox-control"
	}

	webhookStateDir := os.Getenv("WEBHOOK_STATE_DIR")
	webhookWorkers := DefaultWebhookWorkers
	if raw := os.Getenv("WEBHOOK_WORKERS"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil {
			webhookWorkers = val
		} else {
			webhookWorkers = -1
		}
	}

	webhookBacklog := DefaultWebhookBacklog
	if raw := os.Getenv("WEBHOOK_BACKLOG"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil {
			webhookBacklog = val
		} else {
			webhookBacklog = -1
		}
	}

	webhookDeliveryTTL := DefaultWebhookDeliveryTTL
	if raw := os.Getenv("WEBHOOK_DELIVERY_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			webhookDeliveryTTL = d
		} else {
			webhookDeliveryTTL = -1
		}
	}

	webhookDeliveryLimit := DefaultWebhookDeliveryLimit
	if raw := os.Getenv("WEBHOOK_DELIVERY_LIMIT"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil {
			webhookDeliveryLimit = val
		} else {
			webhookDeliveryLimit = -1
		}
	}

	webhookStateMaxBytes := int64(DefaultWebhookStateMaxBytes)
	if raw := os.Getenv("WEBHOOK_STATE_MAX_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			webhookStateMaxBytes = val
		} else {
			webhookStateMaxBytes = -1
		}
	}

	webhookBodyMaxBytes := int64(DefaultWebhookBodyMaxBytes)
	if raw := os.Getenv("WEBHOOK_BODY_MAX_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			webhookBodyMaxBytes = val
		} else {
			webhookBodyMaxBytes = -1
		}
	}

	webhookOldQueueWarning := DefaultWebhookOldQueueWarning
	if raw := os.Getenv("WEBHOOK_OLD_QUEUE_WARNING"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			webhookOldQueueWarning = d
		} else {
			webhookOldQueueWarning = -1
		}
	}

	webhookShutdownTimeout := DefaultWebhookShutdownTimeout
	if raw := os.Getenv("WEBHOOK_SHUTDOWN_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			webhookShutdownTimeout = d
		} else {
			webhookShutdownTimeout = -1
		}
	}

	llmConcurrency := DefaultLLMConcurrency
	if raw := os.Getenv("LLM_CONCURRENCY"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil {
			llmConcurrency = val
		} else {
			llmConcurrency = -1
		}
	}

	llmMinInterval := DefaultLLMMinInterval
	if raw := os.Getenv("LLM_MIN_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			llmMinInterval = d
		} else {
			llmMinInterval = -1
		}
	}

	llmResponseMaxBytes := int64(DefaultLLMResponseMaxBytes)
	if raw := os.Getenv("LLM_RESPONSE_MAX_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil {
			llmResponseMaxBytes = val
		} else {
			llmResponseMaxBytes = -1
		}
	}

	sandboxConcurrency := DefaultSandboxConcurrency
	if raw := os.Getenv("SANDBOX_CONCURRENCY"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil {
			sandboxConcurrency = val
		} else {
			sandboxConcurrency = -1
		}
	}

	diffMaxBytes := int64(DefaultDiffMaxBytes)
	if raw := os.Getenv("DIFF_MAX_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil && val > 0 {
			diffMaxBytes = val
		} else {
			diffMaxBytes = -1
		}
	}

	diffMaxFiles := DefaultDiffMaxFiles
	if raw := os.Getenv("DIFF_MAX_FILES"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			diffMaxFiles = val
		} else {
			diffMaxFiles = -1
		}
	}

	diffMaxHunks := DefaultDiffMaxHunks
	if raw := os.Getenv("DIFF_MAX_HUNKS"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			diffMaxHunks = val
		} else {
			diffMaxHunks = -1
		}
	}

	agentMaxTurns := DefaultAgentMaxTurns
	if raw := os.Getenv("AGENT_MAX_TURNS"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			agentMaxTurns = val
		} else {
			agentMaxTurns = -1
		}
	}

	agentMaxToolBytes := int64(DefaultAgentMaxToolBytes)
	if raw := os.Getenv("AGENT_MAX_TOOL_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil && val > 0 {
			agentMaxToolBytes = val
		} else {
			agentMaxToolBytes = -1
		}
	}

	agentFileReadBytes := int64(DefaultAgentFileReadBytes)
	if raw := os.Getenv("AGENT_FILE_READ_BYTES"); raw != "" {
		if val, err := strconv.ParseInt(raw, 10, 64); err == nil && val > 0 {
			agentFileReadBytes = val
		} else {
			agentFileReadBytes = -1
		}
	}

	agentSearchMaxMatches := DefaultAgentSearchMaxMatches
	if raw := os.Getenv("AGENT_SEARCH_MAX_MATCHES"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			agentSearchMaxMatches = val
		} else {
			agentSearchMaxMatches = -1
		}
	}

	agentModelViolationLimit := DefaultAgentModelViolationLimit
	if raw := os.Getenv("AGENT_MODEL_VIOLATION_LIMIT"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			agentModelViolationLimit = val
		} else {
			agentModelViolationLimit = -1
		}
	}

	retryMaxAttempts := DefaultRetryMaxAttempts
	if raw := os.Getenv("RETRY_MAX_ATTEMPTS"); raw != "" {
		if val, err := strconv.Atoi(raw); err == nil && val > 0 {
			retryMaxAttempts = val
		} else {
			retryMaxAttempts = -1
		}
	}

	retryBaseDelay := DefaultRetryBaseDelay
	if raw := os.Getenv("RETRY_BASE_DELAY"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			retryBaseDelay = d
		} else {
			retryBaseDelay = -1
		}
	}

	retryMaxDelay := DefaultRetryMaxDelay
	if raw := os.Getenv("RETRY_MAX_DELAY"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			retryMaxDelay = d
		} else {
			retryMaxDelay = -1
		}
	}

	retryMaxWait := DefaultRetryMaxWait
	if raw := os.Getenv("RETRY_MAX_WAIT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			retryMaxWait = d
		} else {
			retryMaxWait = -1
		}
	}

	retryJobBudget := DefaultRetryJobBudget
	if raw := os.Getenv("RETRY_JOB_BUDGET"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			retryJobBudget = d
		} else {
			retryJobBudget = -1
		}
	}

	deferMaxWait := DefaultDeferMaxWait
	if raw := os.Getenv("DEFER_MAX_WAIT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			deferMaxWait = d
		} else {
			deferMaxWait = -1
		}
	}

	deferPollInterval := DefaultDeferPollInterval
	if raw := os.Getenv("DEFER_POLL_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			deferPollInterval = d
		} else {
			deferPollInterval = -1
		}
	}

	return &Config{
		GitHubToken:             token,
		WebhookSecret:           webhookSecret,
		Port:                    port,
		LLMBaseURL:              llmBaseURL,
		LLMAPIKey:               llmAPIKey,
		LLMModel:                llmModel,
		EffortLevel:             effort,
		EnableSandbox:           enableSandbox,
		AutoActions:             autoActions,
		GitHubAppSetupToken:     setupToken,
		PublicURL:               publicURL,
		GitHubAppID:             appID,
		GitHubAppPrivateKeyPath: os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"),
		SandboxCPUs:             sandboxCPUs,
		SandboxMemoryBytes:      sandboxMemoryBytes,
		SandboxPidsLimit:        sandboxPidsLimit,
		SandboxDiskBytes:        sandboxDiskBytes,
		SandboxTimeoutSource:    sandboxTimeoutSource,
		SandboxTimeoutPrep:      sandboxTimeoutPrep,
		SandboxTimeoutExecution: sandboxTimeoutExecution,
		SandboxTimeoutCleanup:   sandboxTimeoutCleanup,
		SandboxImage:            os.Getenv("SANDBOX_IMAGE"),
		SandboxSlotDir:          os.Getenv("SANDBOX_SLOT_DIR"),
		SandboxControlDir:       sandboxControlDir,
		WebhookStateDir:         webhookStateDir,
		WebhookWorkers:          webhookWorkers,
		WebhookBacklog:          webhookBacklog,
		WebhookDeliveryTTL:      webhookDeliveryTTL,
		WebhookDeliveryLimit:    webhookDeliveryLimit,
		WebhookStateMaxBytes:    webhookStateMaxBytes,
		WebhookBodyMaxBytes:     webhookBodyMaxBytes,
		WebhookOldQueueWarning:  webhookOldQueueWarning,
		WebhookShutdownTimeout:  webhookShutdownTimeout,
		LLMConcurrency:          llmConcurrency,
		LLMMinInterval:          llmMinInterval,
		LLMResponseMaxBytes:     llmResponseMaxBytes,
		SandboxConcurrency:      sandboxConcurrency,
		DiffMaxBytes:            diffMaxBytes,
		DiffMaxFiles:            diffMaxFiles,
		DiffMaxHunks:            diffMaxHunks,
		AgentMaxTurns:            agentMaxTurns,
		AgentMaxToolBytes:        agentMaxToolBytes,
		AgentFileReadBytes:       agentFileReadBytes,
		AgentSearchMaxMatches:    agentSearchMaxMatches,
		AgentModelViolationLimit: agentModelViolationLimit,
		RetryMaxAttempts:         retryMaxAttempts,
		RetryBaseDelay:           retryBaseDelay,
		RetryMaxDelay:            retryMaxDelay,
		RetryMaxWait:             retryMaxWait,
		RetryJobBudget:           retryJobBudget,
		DeferMaxWait:             deferMaxWait,
		DeferPollInterval:        deferPollInterval,
	}
}
