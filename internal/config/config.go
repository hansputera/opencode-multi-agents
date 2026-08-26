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
	OpenCodeModel      string `yaml:"opencode_model" env:"OPENCODE_MODEL"`

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
	ProxyPoolSize int `yaml:"proxy_pool_size" env:"PROXY_POOL_SIZE"`
	ProxyBasePort int `yaml:"proxy_base_port" env:"PROXY_BASE_PORT"`

	// How many times a freshly created proxy with a duplicate egress IP is
	// rotated (container replaced) before giving up and keeping one duplicate.
	// Values <= 1 disable extra rotation.
	ProxyIPRotateAttempts int `yaml:"proxy_ip_rotate_attempts" env:"PROXY_IP_ROTATE_ATTEMPTS"`

	// ProxySOCKS5Addrs is a comma-separated list of external SOCKS5 proxy
	// addresses that coexist with ProtonVPN containers in the same pool.
	// Format: "socks5://host:port,socks5://user:pass@host:port"
	ProxySOCKS5Addrs string `yaml:"proxy_socks5_addrs" env:"PROXY_SOCKS5_ADDRS"`

	// --- PoW-gated API keys ---
	//
	// When PowEnabled, /v1/* requires a valid API key: either one from
	// GATEWAY_API_KEYS or a PoW-issued key (clients solve a hashcash-style
	// challenge at /api/pow/challenge and redeem it at /api/pow/redeem).
	PowEnabled          bool          `yaml:"pow_enabled" env:"POW_ENABLED"`
	PowStorePath        string        `yaml:"pow_store_path" env:"POW_STORE_PATH"`
	PowChallengeTTL     time.Duration `yaml:"pow_challenge_ttl" env:"POW_CHALLENGE_TTL"`
	PowKeyTTL           time.Duration `yaml:"pow_key_ttl" env:"POW_KEY_TTL"`
	PowBaseDifficulty   int           `yaml:"pow_base_difficulty" env:"POW_BASE_DIFFICULTY"`
	PowMinDifficulty    int           `yaml:"pow_min_difficulty" env:"POW_MIN_DIFFICULTY"`
	PowMaxDifficulty    int           `yaml:"pow_max_difficulty" env:"POW_MAX_DIFFICULTY"`
	PowPlan1Difficulty  int           `yaml:"pow_plan1_difficulty" env:"POW_PLAN1_DIFFICULTY"`
	PowPlan2Difficulty  int           `yaml:"pow_plan2_difficulty" env:"POW_PLAN2_DIFFICULTY"`
	PowPlan3Difficulty  int           `yaml:"pow_plan3_difficulty" env:"POW_PLAN3_DIFFICULTY"`
	PowPlan1RPM         int           `yaml:"pow_plan1_rpm" env:"POW_PLAN1_RPM"`
	PowPlan2RPM         int           `yaml:"pow_plan2_rpm" env:"POW_PLAN2_RPM"`
	PowPlan3RPM         int           `yaml:"pow_plan3_rpm" env:"POW_PLAN3_RPM"`
	PowBurstRPS         int           `yaml:"pow_burst_rps" env:"POW_BURST_RPS"`
	PowBurstCooldown    time.Duration `yaml:"pow_burst_cooldown" env:"POW_BURST_COOLDOWN"`
	PowChallengePerMin  int           `yaml:"pow_challenge_per_min" env:"POW_CHALLENGE_RATE_PER_MIN"`
	PowChallengePerDay  int           `yaml:"pow_challenge_per_day" env:"POW_CHALLENGE_RATE_PER_DAY"`
	PowAdjustInterval   time.Duration `yaml:"pow_adjust_interval" env:"POW_ADJUST_INTERVAL"`
	VPNImage            string        `yaml:"vpn_image" env:"VPN_IMAGE"`
	CooldownDuration    time.Duration `yaml:"cooldown_duration" env:"RATE_LIMIT_COOLDOWN"`
	HealthCheckPeriod   time.Duration `yaml:"health_check_period" env:"HEALTH_CHECK_PERIOD"`
	ResourceCPULimit    string        `yaml:"resource_cpu_limit" env:"RESOURCE_CPU_LIMIT"`
	ResourceMemoryLimit string        `yaml:"resource_memory_limit" env:"RESOURCE_MEMORY_LIMIT"`

	// ProtonVPN configuration
	ProtonVPNUsername       string `yaml:"protonvpn_username" env:"PROTONVPN_USERNAME"`
	ProtonVPNPassword       string `yaml:"protonvpn_password" env:"PROTONVPN_PASSWORD"`
	ProtonVPNAPIBase        string `yaml:"protonvpn_api_base" env:"PROTONVPN_API_BASE"`
	ProtonVPNVpnAPIBase     string `yaml:"protonvpn_vpn_api_base" env:"PROTONVPN_VPN_API_BASE"`
	ProtonVPNStorePath      string `yaml:"protonvpn_store_path" env:"PROTONVPN_STORE_PATH"`
	ProtonVPNRegions        string `yaml:"protonvpn_regions" env:"PROTONVPN_REGIONS"`
	ProtonVPNIPCheckURL     string `yaml:"protonvpn_ip_check_url" env:"PROTONVPN_IP_CHECK_URL"`
	ProtonVPNSessionCookies string `yaml:"protonvpn_session_cookies" env:"PROTONVPN_SESSION_COOKIES"`
	// ProtonVPNAccounts is a comma-separated list of username:password pairs
	// for multi-account round-robin. When set, PROTONVPN_USERNAME/PASSWORD are
	// ignored. Format: "user1:pass1,user2:pass2"
	ProtonVPNAccounts string `yaml:"protonvpn_accounts" env:"PROTONVPN_ACCOUNTS"`

	// Retry configuration
	MaxRetries     int           `yaml:"max_retries" env:"MAX_RETRIES"`
	RetryBaseDelay time.Duration `yaml:"retry_base_delay" env:"RETRY_BASE_DELAY"`
	RetryMaxDelay  time.Duration `yaml:"retry_max_delay" env:"RETRY_MAX_DELAY"`

	// Retry-After (seconds) sent to clients when the upstream keeps rate
	// limiting us across all retries
	RateLimitRetryAfter string `yaml:"rate_limit_retry_after" env:"RATE_LIMIT_RETRY_AFTER"`

	// How long an egress IP stays banned after an upstream 429. During this
	// window no request is routed through it and no new container may be
	// assigned it. The upstream's Retry-After header (if present) extends it.
	IPBanDuration time.Duration `yaml:"ip_ban_duration" env:"IP_BAN_DURATION"`

	// How long the gateway waits for a fresh (unbanned) proxy on an upstream
	// 429 before giving up with 429 + Retry-After. Covers VPN container boot.
	RateLimitFreshIPWait time.Duration `yaml:"rate_limit_fresh_ip_wait" env:"RATE_LIMIT_FRESH_IP_WAIT"`

	// Concurrency
	MaxConcurrent int `yaml:"max_concurrent" env:"MAX_CONCURRENT"`

	// Sticky session
	StickySessionTTL time.Duration `yaml:"sticky_session_ttl" env:"STICKY_SESSION_TTL"`

	// CORS
	CORSOrigin string `yaml:"cors_origin" env:"CORS_ORIGIN"`

	// Logging
	LogLevel  string `yaml:"log_level" env:"LOG_LEVEL"`
	LogFormat string `yaml:"log_format" env:"LOG_FORMAT"`

	// Request timeout
	RequestTimeout time.Duration `yaml:"request_timeout" env:"REQUEST_TIMEOUT"`

	// Metrics
	MetricsDBPath string `yaml:"metrics_db_path" env:"METRICS_DB_PATH"`

	// Model pricing: map from model name to per-1M-token rates
	// Format: "model_name:input_rate,output_rate,cached_rate;model_name:input_rate,output_rate,cached_rate"
	ModelPricing string `yaml:"model_pricing" env:"MODEL_PRICING"`

	// Upstream API keys (comma-separated). When the client sends no
	// Authorization header, the gateway picks a random key per request to
	// spread quota/rate limits across multiple accounts. Entries may be:
	//   - "public" / "public..."        → sent as "x-api-key: <value>"
	//   - "zent-..."                    → sent as "Authorization: Bearer <value>"
	//   - "header:value"                → sent as-is on that header
	UpstreamAPIKeys []string `yaml:"upstream_api_keys"`

	// Gateway API keys (comma-separated). When set, all /v1/* endpoints
	// require the client to send "Authorization: Bearer <key>" with one of
	// these keys; requests without a valid key get 401 authentication_error.
	// Empty (default) keeps /v1/* open to everyone.
	GatewayAPIKeys []string `yaml:"gateway_api_keys"`

	// Built-in web_search tool. When enabled, chat completion requests get a
	// web_search function injected; when the model calls it the gateway runs
	// the search server-side (through the VPN proxy egress) and feeds results
	// back for another round.
	WebSearchEnabled     bool   `yaml:"web_search_enabled" env:"WEB_SEARCH_ENABLED"`
	WebSearchMaxResults  int    `yaml:"web_search_max_results" env:"WEB_SEARCH_MAX_RESULTS"`
	WebSearchMaxPages    int    `yaml:"web_search_max_pages" env:"WEB_SEARCH_MAX_PAGES"`
	WebSearchMaxPageChar int    `yaml:"web_search_max_page_chars" env:"WEB_SEARCH_MAX_PAGE_CHARS"`
	WebSearchMaxRounds   int    `yaml:"web_search_max_rounds" env:"WEB_SEARCH_MAX_ROUNDS"`
	SearxngURL           string `yaml:"searxng_url" env:"SEARXNG_URL"`
	BraveAPIKey          string `yaml:"brave_api_key" env:"BRAVE_API_KEY"`

	// Model list filter: only models whose name contains this substring are
	// returned by /v1/models (case-insensitive). Empty string disables.
	ModelFilter string `yaml:"model_filter" env:"MODEL_FILTER"`
}

// DefaultConfig returns configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:             ":8082",
		UpstreamBaseURL:        "https://opencode.ai/zen/v1",
		UpstreamProvider:       "zen",
		OpenCodeServerURL:      "http://127.0.0.1:4096",
		OpenCodeCLIProviderEnv: "ANTHROPIC_API_KEY",
		ProxyPoolSize:          3,
		ProxyBasePort:          10801,
		ProxyIPRotateAttempts:  3,
		PowEnabled:             false, // opt-in: set POW_ENABLED=true to gate /v1/*
		PowStorePath:           "data/pow.db",
		PowChallengeTTL:        10 * time.Minute,
		PowKeyTTL:              7 * 24 * time.Hour,
		PowBaseDifficulty:      24,
		PowMinDifficulty:       20,
		PowMaxDifficulty:       40,
		PowPlan1Difficulty:     0,
		PowPlan2Difficulty:     4,
		PowPlan3Difficulty:     8,
		PowPlan1RPM:            100,
		PowPlan2RPM:            250,
		PowPlan3RPM:            500,
		PowBurstRPS:            5,
		PowBurstCooldown:       5 * time.Minute,
		PowChallengePerMin:     6,
		PowChallengePerDay:     60,
		PowAdjustInterval:      30 * time.Second,
		VPNImage:               "ghcr.io/tprasadtp/protonwire:latest",
		ProtonVPNAPIBase:       "https://account.protonvpn.com",
		ProtonVPNVpnAPIBase:    "https://vpn-api.proton.me",
		ProtonVPNStorePath:     "data/protonvpn.db",
		WebSearchEnabled:       true,
		WebSearchMaxResults:    5,
		WebSearchMaxPages:      2,
		WebSearchMaxPageChar:   6000,
		WebSearchMaxRounds:     3,
		ProtonVPNRegions:       "NL,US,JP,DE",
		ProtonVPNIPCheckURL:    "https://icanhazip.com/",
		CooldownDuration:       5 * time.Minute,
		HealthCheckPeriod:      30 * time.Second,
		ResourceCPULimit:       "0.25",
		ResourceMemoryLimit:    "512M",
		MaxRetries:             3,
		RetryBaseDelay:         1 * time.Second,
		RetryMaxDelay:          30 * time.Second,
		RateLimitRetryAfter:    "60",
		IPBanDuration:          10 * time.Minute,
		RateLimitFreshIPWait:   90 * time.Second,
		MaxConcurrent:          100,
		StickySessionTTL:       10 * time.Minute,
		CORSOrigin:             "*",
		LogLevel:               "info",
		LogFormat:              "json",
		RequestTimeout:         60 * time.Second,
		MetricsDBPath:          "data/metrics.db",
		ModelFilter:            "-free",
		ModelPricing:           "gpt-4o:2.50,10.00,1.25;gpt-4o-mini:0.15,0.60,0.075;gpt-4-turbo:10.00,30.00,5.00;gpt-3.5-turbo:0.50,1.50,0.25;claude-3-5-sonnet:3.00,15.00,1.50;claude-3-5-haiku:0.25,1.25,0.125",
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
	if v := os.Getenv("GATEWAY_API_KEYS"); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				c.GatewayAPIKeys = append(c.GatewayAPIKeys, k)
			}
		}
	}
	if v := os.Getenv("WEB_SEARCH_ENABLED"); v != "" {
		c.WebSearchEnabled = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("WEB_SEARCH_MAX_RESULTS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.WebSearchMaxResults = i
		}
	}
	if v := os.Getenv("WEB_SEARCH_MAX_PAGES"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.WebSearchMaxPages = i
		}
	}
	if v := os.Getenv("WEB_SEARCH_MAX_PAGE_CHARS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.WebSearchMaxPageChar = i
		}
	}
	if v := os.Getenv("WEB_SEARCH_MAX_ROUNDS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.WebSearchMaxRounds = i
		}
	}
	if v := os.Getenv("SEARXNG_URL"); v != "" {
		c.SearxngURL = v
	}
	if v := os.Getenv("BRAVE_API_KEY"); v != "" {
		c.BraveAPIKey = v
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
	if v := os.Getenv("PROXY_IP_ROTATE_ATTEMPTS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.ProxyIPRotateAttempts = i
		}
	}
	if v := os.Getenv("POW_ENABLED"); v != "" {
		c.PowEnabled = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
	}
	if v := os.Getenv("POW_STORE_PATH"); v != "" {
		c.PowStorePath = v
	}
	if v := os.Getenv("POW_CHALLENGE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PowChallengeTTL = d
		}
	}
	if v := os.Getenv("POW_KEY_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PowKeyTTL = d
		}
	}
	if v := os.Getenv("POW_BASE_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowBaseDifficulty = i
		}
	}
	if v := os.Getenv("POW_MIN_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowMinDifficulty = i
		}
	}
	if v := os.Getenv("POW_MAX_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowMaxDifficulty = i
		}
	}
	if v := os.Getenv("POW_PLAN1_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan1Difficulty = i
		}
	}
	if v := os.Getenv("POW_PLAN2_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan2Difficulty = i
		}
	}
	if v := os.Getenv("POW_PLAN3_DIFFICULTY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan3Difficulty = i
		}
	}
	if v := os.Getenv("POW_PLAN1_RPM"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan1RPM = i
		}
	}
	if v := os.Getenv("POW_PLAN2_RPM"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan2RPM = i
		}
	}
	if v := os.Getenv("POW_PLAN3_RPM"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowPlan3RPM = i
		}
	}
	if v := os.Getenv("POW_BURST_RPS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowBurstRPS = i
		}
	}
	if v := os.Getenv("POW_BURST_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PowBurstCooldown = d
		}
	}
	if v := os.Getenv("POW_CHALLENGE_RATE_PER_MIN"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowChallengePerMin = i
		}
	}
	if v := os.Getenv("POW_CHALLENGE_RATE_PER_DAY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			c.PowChallengePerDay = i
		}
	}
	if v := os.Getenv("POW_ADJUST_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PowAdjustInterval = d
		}
	}
	if v := os.Getenv("WARP_IMAGE"); v != "" {
		c.VPNImage = v
	}
	if v := os.Getenv("VPN_IMAGE"); v != "" {
		c.VPNImage = v
	}
	if v := os.Getenv("PROTONVPN_USERNAME"); v != "" {
		c.ProtonVPNUsername = v
	}
	if v := os.Getenv("PROTONVPN_PASSWORD"); v != "" {
		c.ProtonVPNPassword = v
	}
	if v := os.Getenv("PROTONVPN_API_BASE"); v != "" {
		c.ProtonVPNAPIBase = v
	}
	if v := os.Getenv("PROTONVPN_STORE_PATH"); v != "" {
		c.ProtonVPNStorePath = v
	}
	if v := os.Getenv("PROTONVPN_REGIONS"); v != "" {
		c.ProtonVPNRegions = v
	}
	if v := os.Getenv("PROTONVPN_IP_CHECK_URL"); v != "" {
		c.ProtonVPNIPCheckURL = v
	}
	if v := os.Getenv("PROTONVPN_SESSION_COOKIES"); v != "" {
		c.ProtonVPNSessionCookies = v
	}
	if v := os.Getenv("PROTONVPN_ACCOUNTS"); v != "" {
		c.ProtonVPNAccounts = v
	}
	if v := os.Getenv("PROXY_SOCKS5_ADDRS"); v != "" {
		c.ProxySOCKS5Addrs = v
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
	if v := os.Getenv("MODEL_PRICING"); v != "" {
		c.ModelPricing = v
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
	if c.ProtonVPNSessionCookies == "" && c.ProtonVPNAccounts == "" {
		if c.ProtonVPNUsername == "" {
			return fmt.Errorf("PROTONVPN_USERNAME is required (or set PROTONVPN_SESSION_COOKIES / PROTONVPN_ACCOUNTS)")
		}
		if c.ProtonVPNPassword == "" {
			return fmt.Errorf("PROTONVPN_PASSWORD is required (or set PROTONVPN_SESSION_COOKIES / PROTONVPN_ACCOUNTS)")
		}
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

// ProtonVPNAccount represents a single ProtonVPN account credential.
type ProtonVPNAccount struct {
	Username string
	Password string
}

// ParseProtonVPNAccounts parses the PROTONVPN_ACCOUNTS env var into a list of
// credentials. Falls back to PROTONVPN_USERNAME/PASSWORD if accounts is empty.
func (c *Config) ParseProtonVPNAccounts() []ProtonVPNAccount {
	if c.ProtonVPNAccounts != "" {
		var accounts []ProtonVPNAccount
		for _, pair := range strings.Split(c.ProtonVPNAccounts, ",") {
			pair = strings.TrimSpace(pair)
			idx := strings.IndexByte(pair, ':')
			if idx < 0 {
				continue
			}
			accounts = append(accounts, ProtonVPNAccount{
				Username: strings.TrimSpace(pair[:idx]),
				Password: strings.TrimSpace(pair[idx+1:]),
			})
		}
		if len(accounts) > 0 {
			return accounts
		}
	}
	// Fallback to single account
	if c.ProtonVPNUsername != "" {
		return []ProtonVPNAccount{{Username: c.ProtonVPNUsername, Password: c.ProtonVPNPassword}}
	}
	return nil
}

// ParseSOCKS5Addrs parses the PROXY_SOCKS5_ADDRS env var into a list of addresses.
func (c *Config) ParseSOCKS5Addrs() []string {
	if c.ProxySOCKS5Addrs == "" {
		return nil
	}
	var addrs []string
	for _, addr := range strings.Split(c.ProxySOCKS5Addrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}
