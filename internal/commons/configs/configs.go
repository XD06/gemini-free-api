package configs

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Gemini    GeminiConfig
	Claude    ClaudeConfig
	OpenAI    OpenAIConfig
	Admin     AdminConfig
	Server    ServerConfig
	RateLimit RateLimitConfig
	LogLevel  string
}

type RateLimitConfig struct {
	Enabled     bool
	WindowMs    int
	MaxRequests int
}

type GeminiConfig struct {
	Secure1PSID                string
	Secure1PSIDTS              string
	RefreshInterval            int
	MaxRetries                 int
	Cookies                    string
	Accounts                   []GeminiAccountConfig
	CookieCachePath            string
	AccountStatePath           string
	CookieCache                bool
	StartupCookieRotate        bool
	CookieWorkerEnabled        bool
	CookieWorkerCommand        string
	CookieWorkerDir            string
	CookieWorkerTimeoutSeconds int
}

type GeminiAccountConfig struct {
	ID              string
	Secure1PSID     string
	Secure1PSIDTS   string
	CookieSource    string
	ProxyURL        string
	Priority        int
	StayMinutes     int
	RefreshInterval int
	MaxRetries      int
}

type ClaudeConfig struct {
	APIKey  string
	Model   string
	Cookies string
}

type OpenAIConfig struct {
	APIKey  string
	Model   string
	Cookies string
}

type AdminConfig struct {
	CookieSyncToken string
}

type ServerConfig struct {
	Port     string
	ProxyURL string
}

const (
	defaultServerPort            = "8787"
	defaultGeminiRefreshInterval = 2
	defaultGeminiMaxRetries      = 3
	defaultLogLevel              = "info"
)

func New() (*Config, error) {
	runtimeOverrides := preserveRuntimeEnv("OPENAI_DEBUG_REQUEST_LOG", "GEMINI_DEBUG_STREAM_DIR", "GEMINI_TRACE_STREAM")
	// Load .env file if it exists. Use Overload so .env values take precedence
	// over any stale system/shell environment variables (e.g. an old
	// GEMINI_1PSIDTS exported in the user's shell that would otherwise shadow
	// the freshly rotated value written to .env).
	_ = godotenv.Overload()
	restoreRuntimeEnv(runtimeOverrides)

	var cfg Config

	// Server
	cfg.Server.Port = getEnv("PORT", defaultServerPort)
	cfg.Server.ProxyURL = os.Getenv("PROXY_URL")

	// General
	cfg.LogLevel = getEnv("LOG_LEVEL", defaultLogLevel)
	cfg.Admin.CookieSyncToken = os.Getenv("COOKIE_SYNC_TOKEN")

	// Rate Limit
	cfg.RateLimit.Enabled = getEnvBool("RATE_LIMIT_ENABLED", true)
	cfg.RateLimit.WindowMs = getEnvInt("RATE_LIMIT_WINDOW_MS", 60000)
	cfg.RateLimit.MaxRequests = getEnvInt("RATE_LIMIT_MAX_REQUESTS", 10)

	// Gemini
	cfg.Gemini.Secure1PSID = os.Getenv("GEMINI_1PSID")
	cfg.Gemini.Secure1PSIDTS = os.Getenv("GEMINI_1PSIDTS")
	cfg.Gemini.Cookies = os.Getenv("GEMINI_COOKIES")
	cfg.Gemini.RefreshInterval = getEnvInt("GEMINI_REFRESH_INTERVAL", defaultGeminiRefreshInterval)
	cfg.Gemini.MaxRetries = getEnvInt("GEMINI_MAX_RETRIES", defaultGeminiMaxRetries)
	cfg.Gemini.CookieCache = getEnvBool("GEMINI_COOKIE_CACHE_ENABLED", true)
	cfg.Gemini.CookieCachePath = getEnv("GEMINI_COOKIE_CACHE_PATH", "data/cookies/accounts.json")
	cfg.Gemini.AccountStatePath = getEnv("GEMINI_ACCOUNT_STATE_PATH", "data/state/accounts.json")
	cfg.Gemini.StartupCookieRotate = getEnvBool("GEMINI_STARTUP_COOKIE_ROTATE", true)
	cfg.Gemini.CookieWorkerEnabled = getEnvBool("GEMINI_COOKIE_WORKER_ENABLED", true)
	cfg.Gemini.CookieWorkerCommand = getEnv("GEMINI_COOKIE_WORKER_COMMAND", "npm run sync --silent")
	cfg.Gemini.CookieWorkerDir = getEnv("GEMINI_COOKIE_WORKER_DIR", "tools/cookie-worker")
	cfg.Gemini.CookieWorkerTimeoutSeconds = getEnvInt("GEMINI_COOKIE_WORKER_TIMEOUT_SECONDS", 120)
	cfg.Gemini.Accounts = loadGeminiAccounts(cfg)

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func preserveRuntimeEnv(keys ...string) map[string]string {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			values[key] = value
		}
	}
	return values
}

func restoreRuntimeEnv(values map[string]string) {
	for key, value := range values {
		_ = os.Setenv(key, value)
	}
}

// Validate checks if the configuration has required values
func (c *Config) Validate() error {
	var missingVars []string

	// Check Gemini configuration - legacy single account or named accounts that can
	// be populated later by the cookie worker.
	hasGeminiAccount := c.Gemini.Secure1PSID != ""
	if len(c.Gemini.Accounts) > 0 && os.Getenv("GEMINI_ACCOUNTS") != "" {
		hasGeminiAccount = true
	}
	if !hasGeminiAccount {
		missingVars = append(missingVars, "GEMINI_1PSID or GEMINI_ACCOUNTS")
	}

	// Check Server port is valid
	if c.Server.Port == "" {
		c.Server.Port = defaultServerPort
	}

	if _, err := strconv.Atoi(c.Server.Port); err != nil {
		return fmt.Errorf("invalid PORT value: %q (must be a number)", c.Server.Port)
	}

	if len(missingVars) > 0 {
		return fmt.Errorf("missing required environment variables: %v. Please set them before running the application", missingVars)
	}

	return nil
}

func loadGeminiAccounts(cfg Config) []GeminiAccountConfig {
	accountIDs := splitCSV(os.Getenv("GEMINI_ACCOUNTS"))
	if len(accountIDs) == 0 {
		return []GeminiAccountConfig{{
			ID:              "default",
			Secure1PSID:     cfg.Gemini.Secure1PSID,
			Secure1PSIDTS:   cfg.Gemini.Secure1PSIDTS,
			CookieSource:    cookieSourceFor(cfg.Gemini.Secure1PSID, "env"),
			ProxyURL:        cfg.Server.ProxyURL,
			Priority:        getEnvInt("GEMINI_ACCOUNT_DEFAULT_PRIORITY", 0),
			StayMinutes:     getEnvInt("GEMINI_ACCOUNT_DEFAULT_STAY_MINUTES", 180),
			RefreshInterval: cfg.Gemini.RefreshInterval,
			MaxRetries:      cfg.Gemini.MaxRetries,
		}}
	}

	accounts := make([]GeminiAccountConfig, 0, len(accountIDs))
	for _, id := range accountIDs {
		key := envAccountKey(id)
		accounts = append(accounts, GeminiAccountConfig{
			ID:              id,
			Secure1PSID:     os.Getenv("GEMINI_ACCOUNT_" + key + "_1PSID"),
			Secure1PSIDTS:   os.Getenv("GEMINI_ACCOUNT_" + key + "_1PSIDTS"),
			CookieSource:    cookieSourceFor(os.Getenv("GEMINI_ACCOUNT_"+key+"_1PSID"), "env"),
			ProxyURL:        os.Getenv("GEMINI_ACCOUNT_" + key + "_PROXY"),
			Priority:        getEnvInt("GEMINI_ACCOUNT_"+key+"_PRIORITY", 0),
			StayMinutes:     getEnvInt("GEMINI_ACCOUNT_"+key+"_STAY_MINUTES", 180),
			RefreshInterval: getEnvInt("GEMINI_ACCOUNT_"+key+"_REFRESH_INTERVAL", cfg.Gemini.RefreshInterval),
			MaxRetries:      getEnvInt("GEMINI_ACCOUNT_"+key+"_MAX_RETRIES", cfg.Gemini.MaxRetries),
		})
	}
	return accounts
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func envAccountKey(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(id)) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func cookieSourceFor(value, source string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return source
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseBool(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}
