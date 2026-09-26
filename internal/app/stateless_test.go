package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultConfigIsMemoryOnly pins the stateless contract: the server builds
// its configuration in memory and never touches a file, so a container with no
// writable volume still comes up.
func TestDefaultConfigIsMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	oldConfig, oldUsage := configFile, usageFile
	configFile = filepath.Join(dir, "config.yaml")
	usageFile = filepath.Join(dir, "usage.json")
	t.Cleanup(func() { configFile, usageFile = oldConfig, oldUsage })

	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if cfg.adminPassword() == "" {
		t.Fatal("admin password is empty")
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key == "" {
		t.Fatalf("expected one generated client key, got %+v", cfg.APIKeys)
	}
	if cfg.UpstreamBaseURL() == "" {
		t.Fatal("upstream base URL is empty")
	}

	// Saving is an in-memory no-op and must not create a file.
	if err := saveConfig(configFile, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if _, err := os.Stat(configFile); err == nil {
		t.Fatal("saveConfig wrote a file; the build must be stateless")
	}

	// Usage starts empty and never writes.
	usage := loadUsage()
	usage.Record(10, 20, 0, 0)
	if err := usage.save(); err != nil {
		t.Fatalf("usage save: %v", err)
	}
	if _, err := os.Stat(usageFile); err == nil {
		t.Fatal("usage.save wrote a file; the build must be stateless")
	}
	if got := usage.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("in-memory usage = %d, want 1", got)
	}
}

// TestDefaultConfigUsesEnvironment verifies the env knobs that replace the
// removed config file.
func TestDefaultConfigUsesEnvironment(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "env-admin-password")
	t.Setenv("CLIENT_API_KEY", "ccgw-from-env")
	t.Setenv("COMMANDCODE_API_KEY", "cc-upstream-from-env")

	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if cfg.adminPassword() != "env-admin-password" {
		t.Fatalf("admin password = %q", cfg.adminPassword())
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != "ccgw-from-env" {
		t.Fatalf("client keys = %+v", cfg.APIKeys)
	}
	if len(cfg.CommandCode.Accounts) != 1 || cfg.CommandCode.Accounts[0].APIKey != "cc-upstream-from-env" {
		t.Fatalf("accounts = %+v", cfg.CommandCode.Accounts)
	}
}

// TestDefaultConfigWithoutEnvStillServes documents that a bare container start
// yields a usable gateway: a generated client key and no accounts.
func TestDefaultConfigWithoutEnvStillServes(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("CLIENT_API_KEY", "")
	t.Setenv("COMMANDCODE_API_KEY", "")

	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key == "" {
		t.Fatal("a client key must still be generated")
	}
	if len(cfg.CommandCode.Accounts) != 0 {
		t.Fatalf("accounts should be empty without env, got %+v", cfg.CommandCode.Accounts)
	}
}
