package app

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// AccountConfig is one upstream Command Code credential in config.yaml.
type AccountConfig struct {
	Name    string `yaml:"name"`
	APIKey  string `yaml:"api_key"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil means enabled
}

func (a AccountConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// ClientKeyConfig is one local bearer key that clients use to call this
// gateway.
type ClientKeyConfig struct {
	Name    string `yaml:"name"`
	Key     string `yaml:"key"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil means enabled
}

func (k ClientKeyConfig) IsEnabled() bool {
	return k.Enabled == nil || *k.Enabled
}

type Config struct {
	APIKey string `yaml:"api_key,omitempty"`
	// APIKeys is the list of local client keys. The legacy single api_key
	// field above is migrated into it on load and cleared on save once the
	// list is non-empty.
	APIKeys []ClientKeyConfig `yaml:"api_keys,omitempty"`
	// AdminPassword guards the WebUI admin API. Generated on first start when
	// empty; changeable at runtime via the admin API.
	AdminPassword string `yaml:"admin_password,omitempty"`
	// WebUI controls whether the binary serves the embedded admin interface.
	// nil means enabled.
	WebUI *bool  `yaml:"webui,omitempty"`
	Host  string `yaml:"host"`
	Port  int    `yaml:"port"`

	CommandCode struct {
		// APIKey is the legacy single-account field. It is migrated into
		// Accounts on load and cleared on save once accounts exist.
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	} `yaml:"commandcode"`

	ExcludeModels []string `yaml:"exclude_models"`
	// AccountStrategy selects how requests are spread across accounts:
	// "round_robin" (default) or "priority" (drain the earliest account in the
	// list, then move to the next). Empty means round_robin.
	AccountStrategy string `yaml:"account_strategy,omitempty"`
	Debug           bool   `yaml:"-"` // runtime flag, not persisted

	// mu guards the fields that the admin API mutates while request handlers
	// read them (ExcludeModels, CommandCode.BaseURL, AdminPassword).
	mu sync.RWMutex
}

// SelectionStrategy maps the configured account_strategy to a pool strategy.
// Unknown values fall back to round-robin rather than failing startup.
func (c *Config) SelectionStrategy() SelectionStrategy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return parseSelectionStrategy(c.AccountStrategy)
}

func parseSelectionStrategy(value string) SelectionStrategy {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "priority", "sequential", "failover":
		return StrategyPriority
	default:
		return StrategyRoundRobin
	}
}

// SetAccountStrategy records the strategy in canonical form so a subsequent
// read reports exactly what the pool is doing.
func (c *Config) SetAccountStrategy(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AccountStrategy = parseSelectionStrategy(value).String()
}

func (c *Config) WebUIEnabled() bool {
	return c.WebUI == nil || *c.WebUI
}

func (c *Config) Excludes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ExcludeModels
}

func (c *Config) SetExcludes(list []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ExcludeModels = list
}

func (c *Config) UpstreamBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CommandCode.BaseURL
}

func (c *Config) SetUpstreamBaseURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CommandCode.BaseURL = url
}

func (c *Config) adminPassword() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AdminPassword
}

func (c *Config) setAdminPassword(password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AdminPassword = password
}

// defaultConfig builds the initial in-memory configuration.
//
// The gateway is deliberately stateless: there is no config file and no data
// volume. Accounts and client keys are added at runtime through the WebUI and
// live only in memory, so a restart starts clean. The admin password is taken
// from ADMIN_PASSWORD when set so a redeploy keeps a stable credential;
// otherwise a random one is generated and printed once.
func defaultConfig() (*Config, error) {
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		generated, err := genAdminPassword()
		if err != nil {
			return nil, err
		}
		adminPassword = generated
	} else if len(strings.TrimSpace(adminPassword)) < 8 {
		// The admin API refuses passwords this short, so warn rather than
		// silently accepting a credential that cannot be changed back to itself.
		log.Printf("[WARN] ADMIN_PASSWORD is shorter than 8 characters; the admin API requires at least 8 when changing it")
	}

	c := &Config{
		AdminPassword: adminPassword,
		Host:          "localhost",
		Port:          11434,
		ExcludeModels: []string{"gpt-", "claude-", "gemini-"},
		// Default to priority when the deployment opts in via the environment:
		// draining one account before the next is the common request when a
		// plan is prepaid. Round-robin stays the default otherwise.
		AccountStrategy: os.Getenv("ACCOUNT_STRATEGY"),
	}
	c.CommandCode.BaseURL = "https://api.commandcode.ai"

	// An upstream account and a local client key may be seeded from the
	// environment for deployments that want a working gateway without opening
	// the WebUI first. Both are optional; without them the server still starts
	// and chat requests return 503 until an account is added.
	if key := os.Getenv("COMMANDCODE_API_KEY"); key != "" {
		c.CommandCode.Accounts = []AccountConfig{{Name: "default", APIKey: key}}
	}
	if key := os.Getenv("CLIENT_API_KEY"); key != "" {
		c.APIKeys = []ClientKeyConfig{{Name: "default", Key: key}}
	} else {
		// No configured client key: mint one so the API is usable immediately.
		apiKey, err := genAPIKey()
		if err != nil {
			return nil, err
		}
		c.APIKeys = []ClientKeyConfig{{Name: "default", Key: apiKey}}
	}
	return c, nil
}

func genAPIKey() (string, error) {
	key, err := randomHex(24)
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "ccgw-" + key, nil
}

func genAdminPassword() (string, error) {
	// 12 位纯随机字符即可，不加可读前缀
	return randomPassword(12)
}

// loadConfig is retained for the config-file migration path used by tests and
// one-off tooling. The running server does not call it: runtime configuration
// is built in memory by defaultConfig.
//
// It reads a YAML config and migrates the legacy single-key fields into the
// accounts and client-keys lists.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(cfg.CommandCode.Accounts) == 0 && cfg.CommandCode.APIKey != "" {
		cfg.CommandCode.Accounts = []AccountConfig{{Name: "default", APIKey: cfg.CommandCode.APIKey}}
	}
	if len(cfg.APIKeys) == 0 && cfg.APIKey != "" {
		cfg.APIKeys = []ClientKeyConfig{{Name: "default", Key: cfg.APIKey}}
	}
	return &cfg, nil
}

// saveConfig is a no-op: configuration lives only in memory, so admin edits
// take effect immediately and are intentionally lost on restart.
//
// The signature (and error return) is kept so every admin handler that used to
// persist state still reports success without pretending to write a file.
func saveConfig(path string, cfg *Config) error {
	if cfg == nil {
		return nil
	}
	cfg.mu.Lock()
	// Non-empty lists are the source of truth; keeping the legacy fields would
	// resurrect deleted keys if a config is ever exported from this state.
	if len(cfg.CommandCode.Accounts) > 0 {
		cfg.CommandCode.APIKey = ""
	}
	if len(cfg.APIKeys) > 0 {
		cfg.APIKey = ""
	}
	cfg.mu.Unlock()
	return nil
}

// saveConfigFile writes the legacy YAML config. Kept for tests and for tooling
// that exports configuration; the server does not call it.
func saveConfigFile(path string, cfg *Config) error {
	cfg.mu.Lock()
	if len(cfg.CommandCode.Accounts) > 0 {
		cfg.CommandCode.APIKey = ""
	}
	if len(cfg.APIKeys) > 0 {
		cfg.APIKey = ""
	}
	data, err := yaml.Marshal(cfg)
	cfg.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// writeConfigTemplate is retained for tests; the server no longer writes a
// config file on first start.
func writeConfigTemplate(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	template := "# cmdcode2api configuration\n" +
		"# See README.md for all options.\n" +
		"\n" +
		"# exclude_models is enabled by default for premium/non-open-source models\n" +
		"# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
		"# Remove entries below or set exclude_models: [] to make all models available.\n" +
		"\n" +
		"# commandcode.accounts holds one or more upstream API keys; requests are\n" +
		"# rotated across them. api_keys holds the local bearer keys that clients\n" +
		"# use to call this gateway; both are editable in the WebUI at /webui.\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
