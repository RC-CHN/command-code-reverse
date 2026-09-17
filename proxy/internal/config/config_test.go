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
		"FINGERPRINT_ENABLED", "FINGERPRINT_SEED", "FINGERPRINT_STATE_FILE",
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
