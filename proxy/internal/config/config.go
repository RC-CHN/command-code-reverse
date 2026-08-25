// Package config loads and validates commandcode-proxy configuration
// from environment variables (optionally seeded from a .env file).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// AuthMode selects how downstream clients authenticate.
type AuthMode string

const (
	// AuthManaged: downstream uses PROXY_API_KEY; upstream CC keys stay server-side.
	AuthManaged AuthMode = "managed"
	// AuthPassthrough: downstream sends its own CC key (legacy behavior).
	AuthPassthrough AuthMode = "passthrough"
)

// Config holds all runtime configuration.
type Config struct {
	// Upstream
	APIKeys    []string // key pool, fill-first order
	APIBase    string
	Version    string // reported x-command-code-version; empty = auto-refresh
	VersionPin string // forced version, overrides auto-refresh

	// Downstream auth
	AuthMode    AuthMode
	ProxyAPIKey string

	// Fingerprint
	FingerprintEnabled   bool
	FingerprintSeed      string
	FingerprintStateFile string

	// Server
	Host                 string
	Port                 int
	ShutdownDrain        time.Duration
	MaxBodyBytes         int64
	MaxTokensClamp       int
	StreamIdleTimeout    time.Duration
	NonStreamIdleTimeout time.Duration

	// Observability
	LogFormat         string // "json" or "text"
	LogLevel          string
	MetricsEnabled    bool
	CostHeaderEnabled bool
}

// Load reads configuration from the environment. If a .env file exists in the
// working directory, its entries are loaded first (real env wins).
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, err
	}

	c := &Config{
		APIBase:              getEnv("COMMAND_CODE_API_BASE", "https://api.commandcode.ai"),
		Version:              os.Getenv("COMMAND_CODE_VERSION"),
		VersionPin:           os.Getenv("COMMAND_CODE_VERSION_PIN"),
		AuthMode:             AuthMode(getEnv("AUTH_MODE", string(AuthManaged))),
		ProxyAPIKey:          os.Getenv("PROXY_API_KEY"),
		FingerprintEnabled:   getEnvBool("FINGERPRINT_ENABLED", false),
		FingerprintSeed:      os.Getenv("FINGERPRINT_SEED"),
		FingerprintStateFile: getEnv("FINGERPRINT_STATE_FILE", "./data/fingerprint.json"),
		Host:                 getEnv("HOST", "0.0.0.0"),
		ShutdownDrain:        time.Duration(getEnvInt("SHUTDOWN_DRAIN_SECONDS", 30)) * time.Second,
		MaxBodyBytes:         int64(getEnvInt("MAX_BODY_BYTES", 10*1024*1024)),
		MaxTokensClamp:       getEnvInt("MAX_TOKENS_CLAMP", 200000),
		StreamIdleTimeout:    time.Duration(getEnvInt("STREAM_IDLE_TIMEOUT_SECONDS", 30)) * time.Second,
		NonStreamIdleTimeout: time.Duration(getEnvInt("NONSTREAM_IDLE_TIMEOUT_SECONDS", 90)) * time.Second,
		LogFormat:            getEnv("LOG_FORMAT", "json"),
		LogLevel:             getEnv("LOG_LEVEL", "info"),
		MetricsEnabled:       getEnvBool("METRICS_ENABLED", true),
		CostHeaderEnabled:    getEnvBool("COST_HEADER_ENABLED", false),
	}
	c.Port = getEnvInt("PORT", 3050)

	for _, k := range strings.Split(os.Getenv("COMMAND_CODE_API_KEY"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.APIKeys = append(c.APIKeys, k)
		}
	}

	return c, c.Validate()
}

// Validate enforces fail-fast invariants at startup.
func (c *Config) Validate() error {
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("config: COMMAND_CODE_API_KEY is required (comma-separated for key pool)")
	}
	switch c.AuthMode {
	case AuthManaged:
		if c.ProxyAPIKey == "" {
			return fmt.Errorf("config: PROXY_API_KEY is required when AUTH_MODE=managed")
		}
	case AuthPassthrough:
	default:
		return fmt.Errorf("config: AUTH_MODE must be %q or %q, got %q", AuthManaged, AuthPassthrough, c.AuthMode)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: PORT out of range: %d", c.Port)
	}
	if c.MaxBodyBytes <= 0 {
		return fmt.Errorf("config: MAX_BODY_BYTES must be positive")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf("config: LOG_FORMAT must be \"json\" or \"text\", got %q", c.LogFormat)
	}
	return nil
}

// Addr returns the listen address.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// loadDotEnv seeds environment variables from a .env file. Existing
// environment variables take precedence; malformed lines are skipped.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
