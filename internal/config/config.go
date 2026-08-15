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

	// Proxy pool configuration
	ProxyPoolSize      int           `yaml:"proxy_pool_size" env:"PROXY_POOL_SIZE"`
	ProxyBasePort      int           `yaml:"proxy_base_port" env:"PROXY_BASE_PORT"`
	WARPImage          string        `yaml:"warp_image" env:"WARP_IMAGE"`
	CooldownDuration   time.Duration `yaml:"cooldown_duration" env:"RATE_LIMIT_COOLDOWN"`
	HealthCheckPeriod  time.Duration `yaml:"health_check_period" env:"HEALTH_CHECK_PERIOD"`
	ResourceCPULimit   string        `yaml:"resource_cpu_limit" env:"RESOURCE_CPU_LIMIT"`
	ResourceMemoryLimit string       `yaml:"resource_memory_limit" env:"RESOURCE_MEMORY_LIMIT"`

	// Retry configuration
	MaxRetries         int           `yaml:"max_retries" env:"MAX_RETRIES"`
	RetryBaseDelay     time.Duration `yaml:"retry_base_delay" env:"RETRY_BASE_DELAY"`
	RetryMaxDelay      time.Duration `yaml:"retry_max_delay" env:"RETRY_MAX_DELAY"`

	// Concurrency
	MaxConcurrent      int `yaml:"max_concurrent" env:"MAX_CONCURRENT"`

	// Sticky session
	StickySessionTTL   time.Duration `yaml:"sticky_session_ttl" env:"STICKY_SESSION_TTL"`

	// Logging
	LogLevel  string `yaml:"log_level" env:"LOG_LEVEL"`
	LogFormat string `yaml:"log_format" env:"LOG_FORMAT"`

	// Request timeout
	RequestTimeout time.Duration `yaml:"request_timeout" env:"REQUEST_TIMEOUT"`
}

// DefaultConfig returns configuration with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		ListenAddr:          ":8080",
		UpstreamBaseURL:     "https://openrouter.ai/api",
		ProxyPoolSize:       3,
		ProxyBasePort:       10801,
		WARPImage:           "caomingjun/warp:latest",
		CooldownDuration:    5 * time.Minute,
		HealthCheckPeriod:   30 * time.Second,
		ResourceCPULimit:    "0.25",
		ResourceMemoryLimit: "64M",
		MaxRetries:          3,
		RetryBaseDelay:      1 * time.Second,
		RetryMaxDelay:       30 * time.Second,
		MaxConcurrent:       100,
		StickySessionTTL:    10 * time.Minute,
		LogLevel:            "info",
		LogFormat:           "json",
		RequestTimeout:      60 * time.Second,
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
		c.WARPImage = v
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
	if c.MaxRetries < 0 {
		return fmt.Errorf("MAX_RETRIES cannot be negative")
	}
	if c.MaxRetries > 10 {
		return fmt.Errorf("MAX_RETRIES cannot exceed 10")
	}
	if c.RequestTimeout < 1*time.Second {
		return fmt.Errorf("REQUEST_TIMEOUT must be at least 1s")
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		c.LogLevel = "info" // Default to info if invalid
	}
	c.LogFormat = strings.ToLower(c.LogFormat)
	if c.LogFormat != "json" && c.LogFormat != "console" {
		c.LogFormat = "json" // Default to json if invalid
	}

	return nil
}
