package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ListenAddr != ":8082" {
		t.Errorf("Expected ListenAddr :8082, got %s", cfg.ListenAddr)
	}

	if cfg.ProxyPoolSize != 3 {
		t.Errorf("Expected ProxyPoolSize 3, got %d", cfg.ProxyPoolSize)
	}

	if cfg.MaxRetries != 3 {
		t.Errorf("Expected MaxRetries 3, got %d", cfg.MaxRetries)
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("LISTEN_ADDR", ":9090")
	os.Setenv("PROXY_POOL_SIZE", "5")
	os.Setenv("MAX_RETRIES", "5")
	os.Setenv("LOG_LEVEL", "debug")
	os.Setenv("PROTONVPN_PRIVATE_KEY_DIR", "/tmp/test-keys")
	defer func() {
		os.Unsetenv("LISTEN_ADDR")
		os.Unsetenv("PROXY_POOL_SIZE")
		os.Unsetenv("MAX_RETRIES")
		os.Unsetenv("LOG_LEVEL")
		os.Unsetenv("PROTONVPN_PRIVATE_KEY_DIR")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("Expected ListenAddr :9090, got %s", cfg.ListenAddr)
	}

	if cfg.ProxyPoolSize != 5 {
		t.Errorf("Expected ProxyPoolSize 5, got %d", cfg.ProxyPoolSize)
	}

	if cfg.MaxRetries != 5 {
		t.Errorf("Expected MaxRetries 5, got %d", cfg.MaxRetries)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("Expected LogLevel debug, got %s", cfg.LogLevel)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *Config
		expectError bool
	}{
		{
			name: "Valid config",
			cfg: &Config{
				UpstreamBaseURL:         "https://api.example.com",
				ProxyPoolSize:           3,
				ProxyBasePort:           10801,
				MaxRetries:              3,
				MaxConcurrent:           100,
				RequestTimeout:          60 * time.Second,
				HealthCheckPeriod:       30 * time.Second,
				CooldownDuration:        5 * time.Minute,
				IPBanDuration:           10 * time.Minute,
				RateLimitFreshIPWait:    90 * time.Second,
				RetryBaseDelay:          1 * time.Second,
				RetryMaxDelay:           30 * time.Second,
				ProtonVPNPrivateKeyDir:  "/tmp/test-keys",
				LogLevel:                "info",
				LogFormat:               "json",
			},
			expectError: false,
		},
		{
			name: "Missing upstream URL",
			cfg: &Config{
				UpstreamBaseURL:        "",
				ProxyPoolSize:          3,
				ProtonVPNPrivateKeyDir: "/tmp/test-keys",
			},
			expectError: true,
		},
		{
			name: "Missing ProtonVPN key dir",
			cfg: &Config{
				UpstreamBaseURL: "https://api.example.com",
				ProxyPoolSize:   3,
			},
			expectError: true,
		},
		{
			name: "Invalid pool size (too low)",
			cfg: &Config{
				UpstreamBaseURL: "https://api.example.com",
				ProxyPoolSize:   0,
			},
			expectError: true,
		},
		{
			name: "Invalid pool size (too high)",
			cfg: &Config{
				UpstreamBaseURL: "https://api.example.com",
				ProxyPoolSize:   25,
			},
			expectError: true,
		},
		{
			name: "Invalid max retries",
			cfg: &Config{
				UpstreamBaseURL: "https://api.example.com",
				ProxyPoolSize:   3,
				MaxRetries:      15,
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}
