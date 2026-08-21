package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the gateway
type Config struct {
	// Server configuration
	ListenAddr string `yaml:"listen_addr" env:"LISTEN_ADDR"`

	// Upstream configuration
	UpstreamBaseURL string `yaml:"upstream_base_url" env:"UPSTREAM_BASE_URL"`
	UpstreamAPIKey  string `yaml:"upstream_api_key" env:"UPSTREAM_API_KEY"`
	// UpstreamProvider selects the upstream driver: "zen" (default, OpenAI
	// compatible), "opencode" (OpenCode Server HTTP API) or "opencode-cli"
	// (the opencode CLI baked into each VPN container, exec'd per request).
	// In "opencode" and "opencode-cli" mode each request runs inside a fresh
	// VPN container so the agent's egress IP is unique per request.
	UpstreamProvider string `yaml:"upstream_provider" env:"UPSTREAM_PROVIDER"`
	// OpenCodeServerURL is the base URL of a running `opencode serve` instance
	// (default http://127.0.0.1:4096). Tunnelled through each proxy container.
	OpenCodeServerURL string `yaml:"opencode_server_url" env:"OPENCODE_SERVER_URL"`
	// OpenCodeServerPassword, if set, authenticates to the OpenCode Server via
	// HTTP basic auth (username "opencode" or OPENCODE_SERVER_USERNAME).
	OpenCodeServerPassword string `yaml:"opencode_server_password" env:"OPENCODE_SERVER_PASSWORD"`
	// OpenCodeProviderID / OpenCodeModel select the provider+model handed to
	// OpenCode's /session/:id/message. If OpenCodeProviderID is empty, the
	// gateway PUTs the client's bearer key to /auth/{providerID} derived from
	// the Authorization header (e.g. "anthropic", "openai").
	OpenCodeProviderID string `yaml:"opencode_provider_id" env:"OPENCODE_PROVIDER_ID"`
	OpenCodeModel    string `yaml:"opencode_model" env:"OPENCODE_MODEL"`

	// opencode-cli mode: model override for `opencode run --model` (default
	// empty = opencode's own default), the name of the provider env var that
	// receives the client's Authorization header credential per exec (default
	// ANTHROPIC_API_KEY), extra `opencode run` args, and a comma-separated
	// model list override for /v1/models (default empty = live `opencode
	// models` output).
	OpenCodeCLIModel       string   `yaml:"opencode_cli_model" env:"OPENCODE_CLI_MODEL"`
	OpenCodeCLIProviderEnv string   `yaml:"opencode_cli_provider_env" env:"OPENCODE_CLI_PROVIDER_ENV"`
	OpenCodeCLIArgs        []string `yaml:"opencode_cli_args" env:"OPENCODE_CLI_ARGS"`
	OpenCodeCLIModels      []string `yaml:"opencode_cli_models" env:"OPENCODE_CLI_MODELS"`

	// Proxy pool configuration
	ProxyPoolSize       int           `yaml:"proxy_pool_size" env:"PROXY_POOL_SIZE"`
	ProxyBasePort       int           `yaml:"proxy_base_port" env:"PROXY_BASE_PORT"`
	VPNImage            string        `yaml:"vpn_image" env:"VPN_IMAGE"`
	CooldownDuration    time.Duration `yaml:"cooldown_duration" env:"RATE_LIMIT_COOLDOWN"`
	HealthCheckPeriod   time.Duration `yaml:"health_check_period" env:"HEALTH_CHECK_PERIOD"`
	ResourceCPULimit    string        `yaml:"resource_cpu_limit" env:"RESOURCE_CPU_LIMIT"`
	ResourceMemoryLimit string        `yaml:"resource_memory_limit" env:"RESOURCE_MEMORY_LIMIT"`

	// ProtonVPN configuration
	ProtonVPNPrivateKeyDir string `yaml:"protonvpn_private_key_dir" env:"PROTONVPN_PRIVATE_KEY_DIR"`
	ProtonVPNServer        string `yaml:"protonvpn_server" env:"PROTONVPN_SERVER"`
	ProtonVPNIPCheckURL    string `yaml:"protonvpn_ip_check_url" env:"PROTONVPN_IP_CHECK_URL"`

	// Retry configuration
	MaxRetries         int           `yaml:"max_retries" env:"MAX_RETRIES"`
	RetryBaseDelay     time.Duration `yaml:"retry_base_delay" env:"RETRY_BASE_DELAY"`
	RetryMaxDelay      time.Duration `yaml:"retry_max_delay" env:"RETRY_MAX_DELAY"`

	// Retry-After (seconds) sent to clients when the upstream keeps rate
	// limiting us across all retries
	RateLimitRetryAfter string        `yaml:"rate_limit_retry_after" env:"RATE_LIMIT_RETRY_AFTER"`

	// How long an egress IP stays banned after an upstream 429. During this
	// window no request is routed through it and no new container may be
	// assigned it. The upstream's Retry-After header (if present) extends it.
	IPBanDuration time.Duration `yaml:"ip_ban_duration" env:"IP_BAN_DURATION"`

	// How long the gateway waits for a fresh (unbanned) proxy on an upstream
	// 429 before giving up with 429 + Retry-After. Covers VPN container boot.
	RateLimitFreshIPWait time.Duration `yaml:"rate_limit_fresh_ip_wait" env:"RATE_LIMIT_FRESH_IP_WAIT"`

	// Concurrency
	MaxConcurrent      int `yaml:"max_concurrent" env:"MAX_CONCURRENT"`

	// Sticky session
	StickySessionTTL   time.Duration `yaml:"sticky_session_ttl" env:"STICKY_SESSION_TTL"`

	// CORS
	CORSOrigin string `yaml:"cors_origin" env:"CORS_ORIGIN"`

	// Logging
	LogLevel  string `yaml:"log_level" env:"LOG_LEVEL"`
	LogFormat string `yaml:"log_format" env:"LOG_FORMAT"`

	// Request timeout
	RequestTimeout time.Duration `yaml:"request_timeout" env:"REQUEST_TIMEOUT"`

	// Metrics
	MetricsDBPath string `yaml:"metrics_db_path" env:"METRICS_DB_PATH"`

	// Upstream API keys (comma-separated). When the client sends no
	// Authorization header, the gateway picks a random key per request to
	// spread quota/rate limits across multiple accounts. Entries may be:
	//   - "public" / "public..."        → sent as "x-api-key: <value>"
	//   - "zent-..."                    → sent as "Authorization: Bearer <value>"
	//   - "header:value"                → sent as-is on that header
	UpstreamAPIKeys []string `yaml:"upstream_api_keys"`

	// Model list filter: only models whose name contains this substring are
	// returned by /v1/models (case-insensitive). Empty string disables.
	ModelFilter string `yaml:"model_filter" env:"MODEL_FILTER"`
}

// DefaultConfig returns configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:          ":8082",
		UpstreamBaseURL:     "https://opencode.ai/zen/v1",
		UpstreamProvider:    "zen",
		OpenCodeServerURL:   "http://127.0.0.1:4096",
		OpenCodeCLIProviderEnv: "ANTHROPIC_API_KEY",
		ProxyPoolSize:       3,
		ProxyBasePort:       10801,
		VPNImage:            "ghcr.io/tprasadtp/protonwire:latest",
		ProtonVPNServer:     "node-nl-01.protonvpn.net",
		ProtonVPNIPCheckURL: "https://icanhazip.com/",
		CooldownDuration:    5 * time.Minute,
		HealthCheckPeriod:   30 * time.Second,
		ResourceCPULimit:    "0.25",
		ResourceMemoryLimit: "512M",
		MaxRetries:          3,
		RetryBaseDelay:      1 * time.Second,
		RetryMaxDelay:       30 * time.Second,
		RateLimitRetryAfter: "60",
		IPBanDuration:       10 * time.Minute,
		RateLimitFreshIPWait: 90 * time.Second,
		MaxConcurrent:       100,
		StickySessionTTL:    10 * time.Minute,
		CORSOrigin:          "*",
		LogLevel:            "info",
		LogFormat:           "json",
		RequestTimeout:      60 * time.Second,
		MetricsDBPath:       "data/metrics.db",
		ModelFilter:         "-free",
	}
}

// Load reads configuration from environment variables and optional config file
func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Load .env file if exists (ignore error if not found)
	_ = godotenv.Load()

	// Load from config file if specified
	if configPath := os.Getenv("CONFIG_FILE"); configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}
	}

	// Override with environment variables
	cfg.applyEnvOverrides()

	// Validate
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyEnvOverrides reads configuration from environment variables
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("UPSTREAM_BASE_URL"); v != "" {
		c.UpstreamBaseURL = v
	}
	if v := os.Getenv("UPSTREAM_API_KEY"); v != "" {
		c.UpstreamAPIKey = v
	}
	if v := os.Getenv("UPSTREAM_PROVIDER"); v != "" {
		c.UpstreamProvider = v
	}
	if v := os.Getenv("OPENCODE_SERVER_URL"); v != "" {
		c.OpenCodeServerURL = v
	}
	if v := os.Getenv("OPENCODE_SERVER_PASSWORD"); v != "" {
		c.OpenCodeServerPassword = v
	}
	if v := os.Getenv("OPENCODE_PROVIDER_ID"); v != "" {
		c.OpenCodeProviderID = v
	}
	if v := os.Getenv("OPENCODE_MODEL"); v != "" {
		c.OpenCodeModel = v
	}
	if v := os.Getenv("OPENCODE_CLI_MODEL"); v != "" {
		c.OpenCodeCLIModel = v
	}
	if v := os.Getenv("OPENCODE_CLI_PROVIDER_ENV"); v != "" {
		c.OpenCodeCLIProviderEnv = v
	}
	if v := os.Getenv("OPENCODE_CLI_ARGS"); v != "" {
		for _, a := range strings.Split(v, " ") {
			if a = strings.TrimSpace(a); a != "" {
				c.OpenCodeCLIArgs = append(c.OpenCodeCLIArgs, a)
			}
		}
	}
	if v := os.Getenv("OPENCODE_CLI_MODELS"); v != "" {
		for _, m := range strings.Split(v, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.OpenCodeCLIModels = append(c.OpenCodeCLIModels, m)
			}
		}
	}
	if v := os.Getenv("UPSTREAM_API_KEYS"); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.UpstreamAPIKeys = append(c.UpstreamAPIKeys, k)
			}
		}
	}
	if v := os.Getenv("PROXY_POOL_SIZE"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.ProxyPoolSize = i
		}
	}
	if v := os.Getenv("PROXY_BASE_PORT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.ProxyBasePort = i
		}
	}
	if v := os.Getenv("WARP_IMAGE"); v != "" {
		c.VPNImage = v
	}
	if v := os.Getenv("VPN_IMAGE"); v != "" {
		c.VPNImage = v
	}
	if v := os.Getenv("PROTONVPN_PRIVATE_KEY_DIR"); v != "" {
		c.ProtonVPNPrivateKeyDir = v
	}
	if v := os.Getenv("PROTONVPN_SERVER"); v != "" {
		c.ProtonVPNServer = v
	}
	if v := os.Getenv("PROTONVPN_IP_CHECK_URL"); v != "" {
		c.ProtonVPNIPCheckURL = v
	}
	if v := os.Getenv("RATE_LIMIT_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.CooldownDuration = d
		}
	}
	if v := os.Getenv("HEALTH_CHECK_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.HealthCheckPeriod = d
		}
	}
	if v := os.Getenv("RESOURCE_CPU_LIMIT"); v != "" {
		c.ResourceCPULimit = v
	}
	if v := os.Getenv("RESOURCE_MEMORY_LIMIT"); v != "" {
		c.ResourceMemoryLimit = v
	}
	// opencode-cli mode hosts the opencode (Node/Bun) runtime per container:
	// the 512M default OOM-kills it mid-run (~600MB+ RSS observed). Use a
	// larger default unless the operator set RESOURCE_MEMORY_LIMIT explicitly.
	if c.UpstreamProvider == "opencode-cli" && os.Getenv("RESOURCE_MEMORY_LIMIT") == "" {
		c.ResourceMemoryLimit = "2G"
		c.ResourceCPULimit = "1.0"
	}
	if v := os.Getenv("MAX_RETRIES"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.MaxRetries = i
		}
	}
	if v := os.Getenv("RETRY_BASE_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RetryBaseDelay = d
		}
	}
	if v := os.Getenv("RETRY_MAX_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RetryMaxDelay = d
		}
	}
	if v := os.Getenv("MAX_CONCURRENT"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.MaxConcurrent = i
		}
	}
	if v := os.Getenv("STICKY_SESSION_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.StickySessionTTL = d
		}
	}
	if v := os.Getenv("CORS_ORIGIN"); v != "" {
		c.CORSOrigin = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}
	if v := os.Getenv("REQUEST_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.RequestTimeout = d
		}
	}
	if v := os.Getenv("METRICS_DB_PATH"); v != "" {
		c.MetricsDBPath = v
	}
}

// Validate checks if configuration is valid
func (c *Config) Validate() error {
	if c.UpstreamBaseURL == "" {
		return fmt.Errorf("UPSTREAM_BASE_URL is required")
	}
	if c.ProxyPoolSize < 1 {
		return fmt.Errorf("PROXY_POOL_SIZE must be at least 1")
	}
	if c.ProxyPoolSize > 20 {
		return fmt.Errorf("PROXY_POOL_SIZE cannot exceed 20")
	}
	if c.ProxyBasePort < 1024 || c.ProxyBasePort > 65535 {
		return fmt.Errorf("PROXY_BASE_PORT must be between 1024 and 65535")
	}
	if c.ProtonVPNPrivateKeyDir == "" {
		return fmt.Errorf("PROTONVPN_PRIVATE_KEY_DIR is required")
	}
	if c.MaxRetries < 0 {
		return fmt.Errorf("MAX_RETRIES cannot be negative")
	}
	if c.MaxRetries > 10 {
		return fmt.Errorf("MAX_RETRIES cannot exceed 10")
	}
	if c.MaxConcurrent < 1 {
		return fmt.Errorf("MAX_CONCURRENT must be at least 1")
	}
	if c.MaxConcurrent > 10000 {
		return fmt.Errorf("MAX_CONCURRENT cannot exceed 10000")
	}
	if c.RequestTimeout < 1*time.Second {
		return fmt.Errorf("REQUEST_TIMEOUT must be at least 1s")
	}
	if c.HealthCheckPeriod < 5*time.Second {
		return fmt.Errorf("HEALTH_CHECK_PERIOD must be at least 5s")
	}
	if c.CooldownDuration < 1*time.Minute {
		return fmt.Errorf("RATE_LIMIT_COOLDOWN must be at least 1m")
	}
	if c.IPBanDuration < 1*time.Minute {
		return fmt.Errorf("IP_BAN_DURATION must be at least 1m")
	}
	if c.RateLimitFreshIPWait < 10*time.Second {
		return fmt.Errorf("RATE_LIMIT_FRESH_IP_WAIT must be at least 10s")
	}
	if c.RetryBaseDelay > c.RetryMaxDelay {
		return fmt.Errorf("RETRY_BASE_DELAY must be less than RETRY_MAX_DELAY")
	}
	// Validate upstream provider
	switch c.UpstreamProvider {
	case "zen", "opencode", "opencode-cli":
		// valid
	case "":
		c.UpstreamProvider = "zen" // default
	default:
		return fmt.Errorf("UPSTREAM_PROVIDER must be one of: zen, opencode, opencode-cli")
	}
	// Validate log level
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	case "":
		c.LogLevel = "info" // default
	default:
		return fmt.Errorf("LOG_LEVEL must be one of: debug, info, warn, error")
	}
	// Validate log format
	switch c.LogFormat {
	case "json", "console":
		// valid
	case "":
		c.LogFormat = "json" // default
	default:
		return fmt.Errorf("LOG_FORMAT must be one of: json, console")
	}

	return nil
}
