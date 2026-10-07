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
	}
}
