package app

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
)

// Version is the program version, formatted as vX.Y.Z.
const Version = "v0.2.0"

// configFile is only consulted for the optional legacy config-file import path
// (used by `--oauth` and tests). The running server builds its configuration in
// memory and never writes this path.
var configFile = "config.yaml"

func Run() {
	oauthMode := flag.Bool("oauth", false, "通过浏览器 OAuth 获取 Command Code API Key")
	oauthCallback := flag.String("oauth-callback", "", "OAuth callback URL，例如 http://server.example.com:5959/callback")
	host := flag.String("host", "", "HTTP listen host，例如 localhost 或 0.0.0.0")
	port := flag.Int("port", 0, "HTTP listen port")
	debug := flag.Bool("debug", false, "print request body and all CC SSE events to stderr")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *version {
		fmt.Printf("cmdcode2api %s (go %s)\n", Version, runtime.Version())
		os.Exit(0)
	}

	cfgPath := findConfig()

	// --oauth 模式：浏览器登录获取 API Key
	if *oauthMode {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config failed: %v", err)
		}
		if cfg == nil {
			// 没有配置，先生成一份
			cfg2, err := defaultConfig()
			if err != nil {
				log.Fatalf("create config failed: %v", err)
			}
			cfg = cfg2
		}

		cb, err := runOAuth(OAuthOptions{CallbackURL: *oauthCallback})
		if err != nil {
			log.Fatalf("OAuth failed: %v", err)
		}

		// OAuth 追加账号而不是覆盖：重复执行即可接入多个账号。
		pool := NewAccountPool(cfg.CommandCode.Accounts)
		if acct := pool.Get(accountID(cb.APIKey)); acct != nil {
			fmt.Printf("\nℹ️  API key already configured as account %q\n", acct.Name)
			return
		}
		name := cb.displayName()
		if name == "" {
			name = oauthAccountName(pool)
		}
		if _, err := pool.Add(name, cb.APIKey, true); err != nil {
			log.Fatalf("add oauth account failed: %v", err)
		}
		pool.SyncToConfig(cfg)

		// Without a data volume there is nowhere durable to put this key, so
		// print it for use as COMMANDCODE_API_KEY instead of silently losing it.
		fmt.Printf("\n✅ API key ready as account %q (%d account(s) total)\n", name, pool.Len())
		fmt.Printf("\nThis build keeps state in memory only, so the key cannot be saved.\n")
		fmt.Printf("Set it in the environment to keep using it after a restart:\n\n")
		fmt.Printf("  COMMANDCODE_API_KEY=%s\n\n", cb.APIKey)
		return
	}

	// 正常模式。配置只存在于内存中：没有 config.yaml，也没有数据卷。
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	if cfg == nil {
		cfg, err = defaultConfig()
		if err != nil {
			log.Fatalf("create config failed: %v", err)
		}
		fmt.Printf(`cmdcode2api is starting with a fresh in-memory configuration.

There is no config file and no data volume: accounts and client keys added
in the WebUI live in memory only and are lost when the process restarts.

Local client key: %s
WebUI admin password: %s

To keep these stable across restarts, set CLIENT_API_KEY and ADMIN_PASSWORD
in the environment (and COMMANDCODE_API_KEY for an upstream account).
`, cfg.APIKeys[0].Key, cfg.adminPassword())
	}

	// 没有上游账号也照常启动：WebUI/客户端密钥/设置均可用，
	// chat 请求会返回 503 no_accounts，直到在 WebUI 添加账号。
	if len(cfg.CommandCode.Accounts) == 0 {
		log.Printf("[WARN] no Command Code accounts configured; chat requests will return 503 until an account is added via the WebUI (/webui) or --oauth")
	}

	if cfg.Port == 0 {
		cfg.Port = 11434
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if cfg.UpstreamBaseURL() == "" {
		cfg.SetUpstreamBaseURL("https://api.commandcode.ai")
	}
	if *debug {
		cfg.Debug = true
		debugMode = true
	}

	// 日志同时写入环形缓冲，供 WebUI 查看
	ring := newLogRing()
	log.SetOutput(io.MultiWriter(os.Stderr, ring))

	pool := NewAccountPool(cfg.CommandCode.Accounts)
	cc := NewCCClientWithPool(pool, cfg.UpstreamBaseURL())
	usage := loadUsage()

	if primary := pool.Primary(); primary != nil {
		FetchProviderModels(cfg.UpstreamBaseURL(), primary.APIKey)
	} else {
		log.Printf("[WARN] no enabled Command Code accounts; starting with an empty model catalog")
	}

	log.Printf("accounts: %d configured, %d enabled", pool.Len(), pool.EnabledCount())

	if err := runServer(cc, cfg, usage, ring); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// oauthAccountName derives a label for an OAuth-added account.
func oauthAccountName(pool *AccountPool) string {
	return fmt.Sprintf("oauth-%d", pool.Len()+1)
}

func findConfig() string {
	return configFile
}
