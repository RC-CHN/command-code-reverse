package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// chdir into a temp dir so .env autoload doesn't pick up the repo's real .env.
func isolate(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, k := range []string{
		"COMMAND_CODE_API_KEY", "COMMAND_CODE_API_BASE", "COMMAND_CODE_VERSION",
		"COMMAND_CODE_VERSION_PIN", "AUTH_MODE", "PROXY_API_KEY",
		"CMD_ZDR",
		"JEV_UNLOCK_MAX_OPTIONS", "JEV_TIMEOUT_SECONDS", "JEV_MAX_RESPONSE_BYTES",
		"FINGERPRINT_ENABLED", "FINGERPRINT_SEED", "FINGERPRINT_STATE_FILE", "FINGERPRINT_SESSION_IDLE_SECONDS",
		"HOST", "PORT", "LOG_FORMAT", "LOG_LEVEL", "MAX_BODY_BYTES",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoadDefaults(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", "k1")
	t.Setenv("PROXY_API_KEY", "p")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIBase != "https://api.commandcode.ai" {
		t.Errorf("APIBase = %q", c.APIBase)
	}
	if c.AuthMode != AuthManaged {
		t.Errorf("AuthMode = %q", c.AuthMode)
	}
	if c.Port != 3050 || c.Addr() != "0.0.0.0:3050" {
		t.Errorf("Addr = %q", c.Addr())
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0] != "k1" {
		t.Errorf("APIKeys = %v", c.APIKeys)
	}
	if c.MaxBodyBytes != 64*1024*1024 {
		t.Errorf("MaxBodyBytes = %d, want 64 MiB", c.MaxBodyBytes)
	}
	if c.FingerprintSessionIdle.Seconds() != 1800 {
		t.Fatal("unexpected fingerprint idle default")
	}
	if c.ZDR {
		t.Error("ZDR must be opt-in")
	}
	if c.JevUnlockMaxOptions || c.JevTimeout.Seconds() != 90 || c.JevMaxResponseBytes != 8<<20 {
		t.Fatal("unexpected Jev defaults")
	}
}

func TestJevConfiguration(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		invalid    bool
	}{
		{"JEV_UNLOCK_MAX_OPTIONS", "true", false}, {"JEV_UNLOCK_MAX_OPTIONS", "1", false},
		{"JEV_UNLOCK_MAX_OPTIONS", "false", false}, {"JEV_UNLOCK_MAX_OPTIONS", "0", false},
		{"JEV_UNLOCK_MAX_OPTIONS", "ture", true},
		{"JEV_TIMEOUT_SECONDS", "12", false}, {"JEV_TIMEOUT_SECONDS", "0", true},
		{"JEV_TIMEOUT_SECONDS", "bad", true}, {"JEV_TIMEOUT_SECONDS", "9223372036854775807", true},
		{"JEV_MAX_RESPONSE_BYTES", "1024", false}, {"JEV_MAX_RESPONSE_BYTES", "-1", true},
		{"JEV_MAX_RESPONSE_BYTES", "9223372036854775807", true},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			isolate(t)
			t.Setenv("COMMAND_CODE_API_KEY", "k1")
			t.Setenv("PROXY_API_KEY", "p")
			t.Setenv(tc.key, tc.value)
			cfg, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch tc.key {
			case "JEV_UNLOCK_MAX_OPTIONS":
				if cfg.JevUnlockMaxOptions != (tc.value == "true" || tc.value == "1") {
					t.Fatal("unlock mismatch")
				}
			case "JEV_TIMEOUT_SECONDS":
				if cfg.JevTimeout.Seconds() != 12 {
					t.Fatal("timeout mismatch")
				}
			case "JEV_MAX_RESPONSE_BYTES":
				if cfg.JevMaxResponseBytes != 1024 {
					t.Fatal("response limit mismatch")
				}
			}
		})
	}
}

func TestJevDotEnvOverride(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", "k1")
	t.Setenv("PROXY_API_KEY", "p")
	if err := os.WriteFile(".env", []byte("JEV_UNLOCK_MAX_OPTIONS=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || !cfg.JevUnlockMaxOptions {
		t.Fatalf("dotenv: cfg=%v err=%v", cfg != nil, err)
	}
	t.Setenv("JEV_UNLOCK_MAX_OPTIONS", "false")
	cfg, err = Load()
	if err != nil || cfg.JevUnlockMaxOptions {
		t.Fatalf("override: cfg=%v err=%v", cfg != nil, err)
	}
}

func TestZDRConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		enabled bool
		invalid bool
	}{
		{value: "1", enabled: true},
		{value: "true", enabled: true},
		{value: "0"},
		{value: "false"},
		{value: "ture", invalid: true},
		{value: "2", invalid: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			isolate(t)
			t.Setenv("COMMAND_CODE_API_KEY", "k1")
			t.Setenv("PROXY_API_KEY", "p")
			t.Setenv("CMD_ZDR", tc.value)
			cfg, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid ZDR value silently accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ZDR != tc.enabled {
				t.Fatalf("ZDR = %v, want %v", cfg.ZDR, tc.enabled)
			}
		})
	}
}

func TestZDRDotEnvPrecedence(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			isolate(t)
			t.Setenv("COMMAND_CODE_API_KEY", "k1")
			t.Setenv("PROXY_API_KEY", "p")
			if err := os.WriteFile(".env", []byte("CMD_ZDR=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if override {
				t.Setenv("CMD_ZDR", "0")
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ZDR == override {
				t.Fatalf("ZDR = %v with environment override = %v", cfg.ZDR, override)
			}
		})
	}
}

func TestMaxBodyBytesOverride(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", "k1")
	t.Setenv("PROXY_API_KEY", "p")
	for _, value := range []string{"10485760", "134217728", "0", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("MAX_BODY_BYTES", value)
			c, err := Load()
			if value == "0" || value == "-1" {
				if err == nil {
					t.Fatal("non-positive body limit accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(c.MaxBodyBytes) != value {
				t.Fatalf("MaxBodyBytes = %d, want %s", c.MaxBodyBytes, value)
			}
		})
	}
}

func TestLoadKeyPool(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", " k1 , k2,,k3 ")
	t.Setenv("PROXY_API_KEY", "p")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"k1", "k2", "k3"}
	if len(c.APIKeys) != len(want) {
		t.Fatalf("APIKeys = %v", c.APIKeys)
	}
	for i := range want {
		if c.APIKeys[i] != want[i] {
			t.Errorf("APIKeys[%d] = %q, want %q", i, c.APIKeys[i], want[i])
		}
	}
}

func TestValidateRequiresKeys(t *testing.T) {
	isolate(t)
	if _, err := Load(); err == nil {
		t.Fatal("expected error when COMMAND_CODE_API_KEY is empty")
	}
}

func TestValidateManagedRequiresProxyKey(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", "k1")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when PROXY_API_KEY missing in managed mode")
	}
}

func TestValidatePassthrough(t *testing.T) {
	isolate(t)
	t.Setenv("COMMAND_CODE_API_KEY", "k1")
	t.Setenv("AUTH_MODE", "passthrough")
	if _, err := Load(); err != nil {
		t.Fatalf("passthrough should not require PROXY_API_KEY: %v", err)
	}
}

func TestDotEnvPrecedence(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("COMMAND_CODE_API_KEY=fromfile\nPROXY_API_KEY=p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// Real env wins over .env file.
	t.Setenv("COMMAND_CODE_API_KEY", "fromenv")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKeys[0] != "fromenv" {
		t.Errorf("APIKeys[0] = %q, want fromenv", c.APIKeys[0])
	}
	if c.ProxyAPIKey != "p" {
		t.Errorf("ProxyAPIKey = %q, want p (from .env)", c.ProxyAPIKey)
	}
}

func TestFingerprintIdleConfiguration(t *testing.T) {
	for _, value := range []string{"2400", "0", "-1", "bad", "9223372036854775807"} {
		t.Run(value, func(t *testing.T) {
			isolate(t)
			t.Setenv("COMMAND_CODE_API_KEY", "k1")
			t.Setenv("PROXY_API_KEY", "p")
			t.Setenv("FINGERPRINT_SESSION_IDLE_SECONDS", value)
			cfg, err := Load()
			if value == "2400" {
				if err != nil || cfg.FingerprintSessionIdle.Seconds() != 2400 {
					t.Fatalf("idle config: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid idle duration accepted")
			}
		})
	}
}
