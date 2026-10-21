package configs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
	// CookieSyncTokenSource records where the active token came from:
	// "env", "file" (a token persisted by an earlier run), "generated" (created
	// during this run), "ephemeral" (generated but not persisted), or "unavailable".
	CookieSyncTokenSource string
	// CookieSyncTokenPath is the file used to persist an auto-generated token.
	CookieSyncTokenPath string
	// CookieSyncTokenGenerated is true when this process created a new token,
	// so startup can print it once.
	CookieSyncTokenGenerated bool
}

type ServerConfig struct {
	Port     string
	ProxyURL string
}

const (
	defaultServerPort            = "8787"
	defaultGeminiRefreshInterval = 1440
	defaultGeminiMaxRetries      = 2
	defaultLogLevel              = "info"
	defaultRateLimitMaxRequests  = 30
	defaultAdminTokenPath        = "data/state/admin-token"
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
	cfg.RateLimit.MaxRequests = getEnvInt("RATE_LIMIT_MAX_REQUESTS", defaultRateLimitMaxRequests)

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

	// An account pool may be empty at startup: the console is the primary way
	// to add the first account now. The admin token must still work, so when
	// COOKIE_SYNC_TOKEN is unset we generate one and persist it for reuse.
	ensureAdminToken(&cfg)

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

// Validate checks that the configuration can start the server. An empty
// account pool is valid: accounts can be added from /console at runtime.
func (c *Config) Validate() error {
	// Check Server port is valid
	if c.Server.Port == "" {
		c.Server.Port = defaultServerPort
	}

	if _, err := strconv.Atoi(c.Server.Port); err != nil {
		return fmt.Errorf("invalid PORT value: %q (must be a number)", c.Server.Port)
	}

	return nil
}

func loadGeminiAccounts(cfg Config) []GeminiAccountConfig {
	accountIDs := splitCSV(os.Getenv("GEMINI_ACCOUNTS"))
	if len(accountIDs) == 0 {
		// No accounts is a supported starting point: the console adds the
		// first one. Only synthesize the legacy single account when a cookie
		// was actually configured.
		if strings.TrimSpace(cfg.Gemini.Secure1PSID) == "" {
			return nil
		}
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

// ensureAdminToken guarantees that /admin/* is usable on a fresh install.
// COOKIE_SYNC_TOKEN still wins; otherwise the token is read from, or created
// at, ADMIN_TOKEN_PATH (default data/state/admin-token).
func ensureAdminToken(cfg *Config) {
	if token := strings.TrimSpace(cfg.Admin.CookieSyncToken); token != "" {
		cfg.Admin.CookieSyncToken = token
		cfg.Admin.CookieSyncTokenSource = "env"
		return
	}

	path := strings.TrimSpace(getEnv("ADMIN_TOKEN_PATH", defaultAdminTokenPath))
	cfg.Admin.CookieSyncTokenPath = path
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				cfg.Admin.CookieSyncToken = token
				cfg.Admin.CookieSyncTokenSource = "file"
				return
			}
		}
	}

	token, err := generateAdminToken()
	if err != nil {
		cfg.Admin.CookieSyncTokenSource = "unavailable"
		return
	}
	cfg.Admin.CookieSyncToken = token
	cfg.Admin.CookieSyncTokenSource = "generated"
	cfg.Admin.CookieSyncTokenGenerated = true

	if path == "" {
		cfg.Admin.CookieSyncTokenSource = "ephemeral"
		return
	}
	if err := writeAdminToken(path, token); err != nil {
		cfg.Admin.CookieSyncTokenSource = "ephemeral"
	}
}

func generateAdminToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeAdminToken(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".admin-token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
