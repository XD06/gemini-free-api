package providers

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gemini-free-api/internal/commons/configs"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"go.uber.org/zap"
)

type Client struct {
	accountID       string
	proxyURL        string
	cookieSource    string
	cookieCache     bool
	cookieCachePath string
	startupRotate   bool
	httpClient      *req.Client
	rawHTTPClient   *http.Client
	cookies         *CookieStore
	at              string
	cookieHeader    string // full Cookie header string built by refreshSessionToken, used in GenerateContent
	pushID          string
	buildLabel      string
	sessionID       string
	language        string
	mu              sync.RWMutex // protects: at, healthy, cookieHeader, pushID, buildLabel, sessionID, language
	healthy         bool
	log             *zap.Logger

	autoRefresh           bool
	refreshInterval       time.Duration
	stopRefresh           chan struct{}
	closeOnce             sync.Once
	maxRetries            int
	cachedModels          []ModelInfo
	cachedAliases         map[string]string
	conversations         map[string]*SessionMetadata
	conversationSeen      map[string]time.Time
	conversationUntrusted map[string]bool
	conversationMu        sync.RWMutex
	statusMu              sync.RWMutex
	healthState           string
	lastError             string
	lastValidated         time.Time
	lastCookieSync        time.Time
	requestSeq            uint64
}

type CookieStore struct {
	Secure1PSID   string    `json:"__Secure-1PSID"`
	Secure1PSIDTS string    `json:"__Secure-1PSIDTS"`
	UpdatedAt     time.Time `json:"updated_at"`
	mu            sync.RWMutex
}

const (
	defaultRefreshIntervalMinutes = 2
	maxConversationCacheEntries   = 1000
	conversationCacheTTL          = 12 * time.Hour
)

const (
	AccountStateHealthy          = "healthy"
	AccountStateRefreshing       = "refreshing"
	AccountStateExpired          = "expired"
	AccountStateNeedsManualLogin = "needs_manual_login"
	AccountStateUninitialized    = "uninitialized"
)

var (
	accessTokenRegex         = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	accessTokenFallbackRegex = regexp.MustCompile(`\["SNlM0e","([^"]+)"\]`)
	pushIDRegex              = regexp.MustCompile(`"qKIAYe":"([^"]+)"`)
	buildLabelRegex          = regexp.MustCompile(`"cfb2h":"([^"]+)"`)
	sessionIDRegex           = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
	languageRegex            = regexp.MustCompile(`"TuX5cc":"([^"]+)"`)
	modelIDRegex             = regexp.MustCompile(`gemini-[a-zA-Z0-9.-]+`)
	validModelPrefixRegex    = regexp.MustCompile(`^gemini-(\d|advanced)`)
	imageURLRegex            = regexp.MustCompile(`(?i)(?:https?:)?//[^\s"'<>\\]+|(?:[a-z0-9.-]+\.)?googleusercontent\.com/[^\s"'<>\\]+`)
	bardErrorInfoRegex       = regexp.MustCompile(`BardErrorInfo.*?\[([0-9]+)\]`)
)

func NewClient(cfg *configs.Config, log *zap.Logger) *Client {
	return NewClientForAccount(cfg, defaultGeminiAccountConfig(cfg), log)
}

func defaultGeminiAccountConfig(cfg *configs.Config) configs.GeminiAccountConfig {
	if cfg != nil && len(cfg.Gemini.Accounts) > 0 {
		return cfg.Gemini.Accounts[0]
	}
	return configs.GeminiAccountConfig{
		ID:              "default",
		Secure1PSID:     cfg.Gemini.Secure1PSID,
		Secure1PSIDTS:   cfg.Gemini.Secure1PSIDTS,
		ProxyURL:        cfg.Server.ProxyURL,
		StayMinutes:     180,
		RefreshInterval: cfg.Gemini.RefreshInterval,
		MaxRetries:      cfg.Gemini.MaxRetries,
	}
}

func NewClientForAccount(cfg *configs.Config, account configs.GeminiAccountConfig, log *zap.Logger) *Client {
	if account.ID == "" {
		account.ID = "default"
	}
	cookies := &CookieStore{
		Secure1PSID:   account.Secure1PSID,
		Secure1PSIDTS: account.Secure1PSIDTS,
		UpdatedAt:     time.Now(),
	}

	client := req.NewClient().
		SetTimeout(10 * time.Minute).
		SetCommonHeaders(DefaultHeaders)
	if account.ProxyURL != "" {
		client.SetProxyURL(account.ProxyURL)
	}
	if log != nil {
		log.Info("Gemini account proxy configured",
			zap.String("account", account.ID),
			zap.Bool("proxy_enabled", strings.TrimSpace(account.ProxyURL) != ""),
			zap.String("proxy", redactProxyURL(account.ProxyURL)),
		)
	}

	rawTransport := &http.Transport{
		Proxy:                 proxyFunc(account.ProxyURL),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	rawClient := &http.Client{
		Transport: rawTransport,
		Timeout:   5 * time.Minute,
	}

	refreshIntervalMinutes := cfg.Gemini.RefreshInterval
	if account.RefreshInterval > 0 {
		refreshIntervalMinutes = account.RefreshInterval
	}
	if refreshIntervalMinutes <= 0 {
		refreshIntervalMinutes = defaultRefreshIntervalMinutes
	}
	maxRetries := account.MaxRetries
	if maxRetries <= 0 {
		maxRetries = cfg.Gemini.MaxRetries
	}

	return &Client{
		accountID:             account.ID,
		proxyURL:              account.ProxyURL,
		cookieSource:          account.CookieSource,
		cookieCache:           cfg.Gemini.CookieCache,
		cookieCachePath:       cfg.Gemini.CookieCachePath,
		startupRotate:         cfg.Gemini.StartupCookieRotate,
		httpClient:            client,
		rawHTTPClient:         rawClient,
		cookies:               cookies,
		autoRefresh:           true,
		refreshInterval:       time.Duration(refreshIntervalMinutes) * time.Minute,
		stopRefresh:           make(chan struct{}),
		maxRetries:            maxRetries,
		log:                   log,
		conversations:         make(map[string]*SessionMetadata),
		conversationSeen:      make(map[string]time.Time),
		conversationUntrusted: make(map[string]bool),
		healthState:           AccountStateUninitialized,
		requestSeq:            uint64(time.Now().UnixNano()%900000) + 100000,
	}
}

type AccountStatus struct {
	ID             string    `json:"id"`
	Healthy        bool      `json:"healthy"`
	State          string    `json:"state"`
	ProxyURL       string    `json:"proxy_url,omitempty"`
	CookieSource   string    `json:"cookie_source,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastValidated  time.Time `json:"last_validated,omitempty"`
	LastCookieSync time.Time `json:"last_cookie_sync,omitempty"`
	Active         bool      `json:"active,omitempty"`
	ActiveUntil    time.Time `json:"active_until,omitempty"`
}

func (c *Client) AccountStatus() AccountStatus {
	c.mu.RLock()
	healthy := c.healthy
	c.mu.RUnlock()

	c.statusMu.RLock()
	state := c.healthState
	lastError := c.lastError
	lastValidated := c.lastValidated
	lastCookieSync := c.lastCookieSync
	c.statusMu.RUnlock()
	if state == "" {
		state = AccountStateUninitialized
	}

	return AccountStatus{
		ID:             c.accountID,
		Healthy:        healthy,
		State:          state,
		ProxyURL:       c.proxyURL,
		CookieSource:   c.cookieSource,
		LastError:      lastError,
		LastValidated:  lastValidated,
		LastCookieSync: lastCookieSync,
	}
}

func (c *Client) setAccountState(state string, err error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.healthState = state
	if err != nil {
		c.lastError = err.Error()
	} else {
		c.lastError = ""
	}
	if state == AccountStateHealthy {
		c.lastValidated = time.Now()
	}
}

func proxyFunc(proxyURL string) func(*http.Request) (*url.URL, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return func(*http.Request) (*url.URL, error) {
			return nil, err
		}
	}
	return http.ProxyURL(parsed)
}

func redactProxyURL(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return "[invalid]"
	}
	if parsed.User != nil {
		prefix := ""
		if parsed.Scheme != "" {
			prefix = parsed.Scheme + "://"
		}
		return prefix + "***@" + parsed.Host
	}
	return parsed.String()
}

func (c *Client) nextRequestID() string {
	return strconv.FormatUint(atomic.AddUint64(&c.requestSeq, 100000), 10)
}

func (c *Client) newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 proxyFunc(c.proxyURL),
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		Timeout: timeout,
	}
}

func (c *Client) Init(ctx context.Context) error {
	c.setAccountState(AccountStateRefreshing, nil)
	c.log.Info("Gemini account initialization started",
		zap.String("account", c.accountID),
		zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
		zap.String("proxy", redactProxyURL(c.proxyURL)),
	)
	// Clean cookies
	c.cookies.Secure1PSID = cleanCookie(c.cookies.Secure1PSID)
	configPSIDTS := cleanCookie(c.cookies.Secure1PSIDTS) // Save original config value
	c.cookies.Secure1PSIDTS = configPSIDTS

	// Check if we should use cached cookies or clear cache
	if c.cookies.Secure1PSID != "" {
		cachedTS, err := c.LoadCachedCookies()

		selectedTS, source, clearCache := selectStartupPSIDTS(configPSIDTS, c.cookieSource, cachedTS, err)
		if clearCache {
			_ = c.ClearCookieCache()
		}
		if selectedTS != "" && selectedTS != c.cookies.Secure1PSIDTS {
			c.cookies.Secure1PSIDTS = selectedTS
		}
		if source == "cache" {
			c.log.Info("Loaded __Secure-1PSIDTS from cache")
		}
	}

	// Populate cookies first
	c.httpClient.SetCommonCookies(c.cookies.ToHTTPCookies()...)

	// Get SNlM0e token - first try with provided cookies directly
	// This is the real "can this cookie talk to Gemini" test
	err := c.refreshSessionToken()

	// If direct SNlM0e fetch fails, try rotation as recovery
	if err != nil {
		c.log.Info("Direct session token fetch failed, attempting cookie rotation", zap.Error(err))
		if rotErr := c.RotateCookies(); rotErr == nil {
			c.log.Info("Cookie rotation succeeded, retrying session token fetch")
			err = c.refreshSessionToken()
		} else {
			c.log.Warn("Cookie rotation also failed", zap.Error(rotErr))
		}
	}

	if err != nil {
		c.setAccountState(AccountStateNeedsManualLogin, err)
		return err
	}

	// Optional: proactive rotation for fresh PSIDTS (only if SNlM0e already works)
	// This runs in background so it doesn't block init
	if c.startupRotate && c.cookies.Secure1PSID != "" {
		go func() {
			c.log.Info("Proactively rotating cookies in background for fresh __Secure-1PSIDTS...")
			if rotErr := c.RotateCookies(); rotErr != nil {
				c.log.Debug("Background cookie rotation failed (non-critical)", zap.Error(rotErr))
			} else {
				c.log.Info("Background cookie rotation succeeded")
			}
		}()
	}

	// Save the valid cookies to cache immediately after successful init
	_ = c.SaveCachedCookies()

	c.log.Info("✅ Gemini client initialized successfully",
		zap.String("account", c.accountID),
		zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
		zap.String("proxy", redactProxyURL(c.proxyURL)),
	)
	c.setAccountState(AccountStateHealthy, nil)

	// 5. Start auto-refresh in background
	if c.autoRefresh {
		go c.startAutoRefresh()
	}

	return nil
}

func (c *Client) refreshSessionToken() error {
	c.log.Info("Gemini session token refresh started",
		zap.String("account", c.accountID),
		zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
		zap.String("proxy", redactProxyURL(c.proxyURL)),
	)
	// 1. Initial hit to google.com to get extra cookies (NID, etc)
	// Reuse the existing httpClient to avoid creating a new transport
	// (and new TLS connections) on every refresh.
	resp1, err := c.httpClient.R().Get("https://www.google.com/")
	extraCookies := ""
	if err == nil {
		parts := []string{}
		for _, ck := range resp1.Cookies() {
			parts = append(parts, fmt.Sprintf("%s=%s", ck.Name, ck.Value))
			// Also sync to main client
			c.httpClient.SetCommonCookies(ck)
		}
		if len(parts) > 0 {
			extraCookies = strings.Join(parts, "; ") + "; "
		}
	}

	// 2. Prepare full cookie string
	cookieStr := fmt.Sprintf("%s__Secure-1PSID=%s; __Secure-1PSIDTS=%s",
		extraCookies, c.cookies.Secure1PSID, c.cookies.Secure1PSIDTS)

	commonHeaders := map[string]string{
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
		"Accept-Language":           "en-US,en;q=0.9",
		"Cache-Control":             "max-age=0",
		"Origin":                    "https://gemini.google.com",
		"Sec-Ch-Ua":                 `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Windows"`,
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"X-Same-Domain":             "1",
		"User-Agent":                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}

	// Reuse the existing rawHTTPClient (already configured with proxy, TLS,
	// and connection pooling) instead of creating a new one each time.
	hClient := c.rawHTTPClient

	// Helper to merge cookies into a map to avoid duplicates
	mergeCookies := func(baseStr string, newCks []*http.Cookie) string {
		m := make(map[string]string)
		for _, part := range strings.Split(baseStr, ";") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			kv := strings.SplitN(p, "=", 2)
			if len(kv) == 2 {
				m[kv[0]] = kv[1]
			}
		}
		for _, ck := range newCks {
			m[ck.Name] = ck.Value
		}
		res := []string{}
		for k, v := range m {
			res = append(res, fmt.Sprintf("%s=%s", k, v))
		}
		return strings.Join(res, "; ")
	}

	req1, _ := http.NewRequest("GET", "https://gemini.google.com/?hl=en", nil)
	for k, v := range commonHeaders {
		req1.Header.Set(k, v)
	}
	req1.Header.Set("Cookie", cookieStr)
	resp1_direct, _ := hClient.Do(req1)
	if resp1_direct != nil {
		cookieStr = mergeCookies(cookieStr, resp1_direct.Cookies())
		for _, ck := range resp1_direct.Cookies() {
			c.httpClient.SetCommonCookies(ck)
		}
		resp1_direct.Body.Close()
	}

	// 2. The main INIT hit
	req2, _ := http.NewRequest("GET", EndpointInit+"?hl=en", nil)
	for k, v := range commonHeaders {
		req2.Header.Set(k, v)
	}
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	req2.Header.Set("Cookie", cookieStr)
	req2.Header.Set("Referer", "https://gemini.google.com/")
	// NOTE: do NOT set Accept-Encoding manually. Go's default Transport only
	// auto-decompresses gzip when it set the header itself; declaring br/zstd
	// here would make the server respond with brotli, which we cannot decode,
	// turning the body into garbage and breaking SNlM0e extraction.

	resp, err := hClient.Do(req2)
	if err != nil {
		return fmt.Errorf("failed to reach gemini app: %w", err)
	}
	defer resp.Body.Close()

	var bodyReader io.ReadCloser = resp.Body
	// Transport-driven gzip auto-decompression normally clears the
	// Content-Encoding header; keep this as a defensive fallback.
	if strings.Contains(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err == nil {
			bodyReader = gz
			defer gz.Close()
		}
	}

	bodyBytes, _ := io.ReadAll(bodyReader)
	body := string(bodyBytes)

	// Merge cookies from the init response into cookieStr
	cookieStr = mergeCookies(cookieStr, resp.Cookies())

	matches := accessTokenRegex.FindStringSubmatch(body)
	if len(matches) < 2 {
		matches = accessTokenFallbackRegex.FindStringSubmatch(body)
		if len(matches) < 2 {
			errMsg := "authentication failed: SNlM0e not found"
			if strings.Contains(body, "Sign in") || strings.Contains(body, "login") {
				errMsg = "authentication failed: cookies invalid. Please provide __Secure-1PSIDTS in addition to __Secure-1PSID"
			}
			c.log.Info(errMsg)
			return fmt.Errorf("%s", errMsg)
		}
	}

	pushID := "feeds/mcudyrk2a4khkz"
	if pushMatches := pushIDRegex.FindStringSubmatch(body); len(pushMatches) >= 2 {
		pushID = pushMatches[1]
	}
	buildLabel := ""
	if buildMatches := buildLabelRegex.FindStringSubmatch(body); len(buildMatches) >= 2 {
		buildLabel = buildMatches[1]
	}
	sessionID := ""
	if sessionMatches := sessionIDRegex.FindStringSubmatch(body); len(sessionMatches) >= 2 {
		sessionID = sessionMatches[1]
	}
	language := "en"
	if langMatches := languageRegex.FindStringSubmatch(body); len(langMatches) >= 2 {
		language = langMatches[1]
	}

	c.mu.Lock()
	c.at = matches[1]
	c.cookieHeader = cookieStr // save full cookie string for use in GenerateContent
	c.pushID = pushID
	c.buildLabel = buildLabel
	c.sessionID = sessionID
	c.language = language
	c.healthy = true
	c.mu.Unlock()

	// Update dynamic models from the same initialization body
	c.refreshModels(body)

	return nil
}

func (c *Client) refreshModels(body string) {
	now := time.Now().Unix()

	allModels := []ModelInfo{
		{ID: "cf41b0e0dd7d53e5", Created: now, OwnedBy: "google", Provider: "gemini"},
		{ID: "fbb127bbb056c959", Created: now, OwnedBy: "google", Provider: "gemini"},
		{ID: "9d8ca3786ebdfbea", Created: now, OwnedBy: "google", Provider: "gemini"},
	}

	// Also pick up gemini-advanced if available in page body
	matches := modelIDRegex.FindAllString(body, -1)
	seen := map[string]bool{
		"cf41b0e0dd7d53e5": true,
		"fbb127bbb056c959": true,
		"9d8ca3786ebdfbea": true,
	}
	for _, id := range matches {
		id = strings.Trim(id, `\"' `)
		if !seen[id] && validModelPrefixRegex.MatchString(id) && id == "gemini-advanced" {
			seen[id] = true
			allModels = append(allModels, ModelInfo{
				ID:       id,
				Created:  now,
				OwnedBy:  "google",
				Provider: "gemini",
			})
		}
	}

	c.mu.Lock()
	c.cachedModels, c.cachedAliases = resolveModels(allModels)
	c.mu.Unlock()

	if len(c.cachedModels) == 0 {
		c.log.Warn("No models found. Please check your cookies or connection.")
	} else {
		ids := make([]string, 0, len(c.cachedModels))
		for _, m := range c.cachedModels {
			ids = append(ids, m.ID)
		}
		c.log.Info("Refreshed models", zap.Int("count", len(c.cachedModels)), zap.Strings("models", ids))
	}
}

func resolveModels(all []ModelInfo) ([]ModelInfo, map[string]string) {
	seen := make(map[string]bool)
	var models []ModelInfo
	aliases := map[string]string{}
	for _, m := range all {
		id := m.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, m)

		switch id {
		case "cf41b0e0dd7d53e5":
			aliases["gemini-3.1-flash-lite"] = id
		case "fbb127bbb056c959":
			aliases["gemini-3.5-flash"] = id
		case "9d8ca3786ebdfbea":
			aliases["gemini-3.1-pro"] = id
		}
	}
	return models, aliases
}

// startAutoRefresh periodically refreshes the PSIDTS cookie
func (c *Client) startAutoRefresh() {
	for {
		timer := time.NewTimer(jitterDuration(c.refreshInterval))
		select {
		case <-timer.C:
			c.log.Debug("Starting scheduled cookie refresh")
			rotateErr := c.RotateCookies()
			if rotateErr != nil {
				// Check if it's a 401/403 (cookies fully expired) — no point retrying session token
				isCookieExpired := strings.Contains(rotateErr.Error(), "status 401") ||
					strings.Contains(rotateErr.Error(), "status 403")

				if isCookieExpired {
					c.log.Error("Cookies have expired — please update GEMINI_1PSID and GEMINI_1PSIDTS in .env",
						zap.Error(rotateErr),
						zap.String("action", "Visit https://gemini.google.com → F12 → Application → Cookies"),
					)
					c.mu.Lock()
					c.healthy = false
					c.mu.Unlock()
					c.setAccountState(AccountStateNeedsManualLogin, rotateErr)
					continue
				}

				// RotateCookies failed but NOT due to expired cookies (Google may not return new cookie every time)
				// Fallback: try to refresh the session token (SNlM0e/at) to keep client alive
				c.log.Warn("Cookie rotation failed, falling back to session token refresh", zap.Error(rotateErr))
				if sessionErr := c.refreshSessionToken(); sessionErr != nil {
					// Both methods failed — mark client as unhealthy so callers know
					c.log.Error("Session token refresh also failed, marking client unhealthy",
						zap.NamedError("rotation_error", rotateErr),
						zap.NamedError("session_error", sessionErr),
					)
					c.mu.Lock()
					c.healthy = false
					c.mu.Unlock()
					c.setAccountState(AccountStateExpired, sessionErr)
				} else {
					c.log.Info("Session token refreshed successfully after rotation failure")
					// Ensure client is marked healthy since session token is valid
					c.mu.Lock()
					c.healthy = true
					c.mu.Unlock()
					c.setAccountState(AccountStateHealthy, nil)
				}
			} else {
				// Rotation succeeded — also refresh session token to keep SNlM0e/at up to date
				if sessionErr := c.refreshSessionToken(); sessionErr != nil {
					c.log.Warn("Cookie rotated but session token refresh failed", zap.Error(sessionErr))
					c.setAccountState(AccountStateExpired, sessionErr)
				} else {
					c.log.Info("Cookie and session token refreshed successfully")
					c.setAccountState(AccountStateHealthy, nil)
				}
			}
		case <-c.stopRefresh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		base = time.Duration(defaultRefreshIntervalMinutes) * time.Minute
	}
	half := base / 2
	if half <= 0 {
		return base
	}
	return half + time.Duration(rand.Int63n(int64(base)))
}

func selectStartupPSIDTS(configPSIDTS, configSource, cachedPSIDTS string, cacheErr error) (selected string, source string, clearCache bool) {
	configPSIDTS = cleanCookie(configPSIDTS)
	configSource = strings.TrimSpace(configSource)
	cachedPSIDTS = cleanCookie(cachedPSIDTS)
	if configPSIDTS != "" && configSource != "" && configSource != "env" {
		return configPSIDTS, configSource, false
	}
	if cacheErr == nil && cachedPSIDTS != "" {
		return cachedPSIDTS, "cache", false
	}
	if configPSIDTS != "" {
		return configPSIDTS, "config", false
	}
	return "", "", false
}

func (c *Client) RotateCookies() error {
	c.log.Info("Gemini cookie rotation started",
		zap.String("account", c.accountID),
		zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
		zap.String("proxy", redactProxyURL(c.proxyURL)),
	)
	var lastErr error
	backoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

	for i := 0; i <= 3; i++ {
		if i > 0 {
			c.log.Warn(fmt.Sprintf("Retrying cookie rotation (attempt %d/3) after %v due to error: %v", i, backoffs[i-1], lastErr))
			time.Sleep(backoffs[i-1])
		}
		err := c.rotateCookiesOnce()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("cookie rotation failed after 3 retries: %w", lastErr)
}

func (c *Client) rotateCookiesOnce() error {
	c.cookies.mu.Lock()
	defer c.cookies.mu.Unlock()

	// Prepare cookies for rotation request
	// NOTE: We access fields directly instead of using ToHTTPCookies() to avoid recursive locking (deadlock)
	parts := []string{}
	if c.cookies.Secure1PSID != "" {
		parts = append(parts, fmt.Sprintf("__Secure-1PSID=%s", c.cookies.Secure1PSID))
	}
	if c.cookies.Secure1PSIDTS != "" {
		parts = append(parts, fmt.Sprintf("__Secure-1PSIDTS=%s", c.cookies.Secure1PSIDTS))
	}
	cookieStr := strings.Join(parts, "; ")

	// Payload must be exactly this string
	strBody := `[000,"-0000000000000000000"]`
	req, _ := http.NewRequest("POST", EndpointRotateCookies, strings.NewReader(strBody))

	req.Header.Set("Content-Type", "application/json")
	// Google often blocks requests with default Go-http-client User-Agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", cookieStr)

	c.log.Debug("Sending rotation request", zap.String("url", EndpointRotateCookies))
	hClient := c.newHTTPClient(5 * time.Second)
	resp, err := hClient.Do(req)
	if err != nil {
		// Log as Info to avoid scary stacktraces in development mode for expected auth failures
		c.log.Info("Rotation request failed (network/auth issue)", zap.String("error", err.Error()))
		return fmt.Errorf("failed to call rotation endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.log.Info("Rotation failed (likely invalid __Secure-1PSID)", zap.Int("status", resp.StatusCode))
		return fmt.Errorf("rotation failed with status %d", resp.StatusCode)
	}

	// Extract new PSIDTS from Set-Cookie headers
	found := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__Secure-1PSIDTS" {
			c.cookies.Secure1PSIDTS = cookie.Value
			c.cookies.UpdatedAt = time.Now()
			found = true
			// Save the new cookie to cache immediately
			_ = c.SaveCachedCookies()
		}
		// Sync to req/v3 client for future calls
		c.httpClient.SetCommonCookies(cookie)
	}

	if found {
		c.log.Info("Cookie rotated successfully", zap.Time("updated_at", c.cookies.UpdatedAt))
	} else {
		if c.cookies.Secure1PSIDTS == "" {
			return errors.New("failed to obtain __Secure-1PSIDTS from Google rotation endpoint. Your __Secure-1PSID cookie might be invalid or expired")
		}
		// Google returns 200 but omits a new cookie when the existing one is still valid — not an error
		c.log.Debug("No new __Secure-1PSIDTS issued; existing cookie is still valid")
	}
	return nil
}

func (c *Client) GetCookies() *CookieStore {
	c.cookies.mu.RLock()
	defer c.cookies.mu.RUnlock()

	return &CookieStore{
		Secure1PSID:   c.cookies.Secure1PSID,
		Secure1PSIDTS: c.cookies.Secure1PSIDTS,
		UpdatedAt:     c.cookies.UpdatedAt,
	}
}

func (c *Client) UpdateCookies(ctx context.Context, secure1PSID, secure1PSIDTS string) error {
	_ = ctx
	secure1PSID = cleanCookie(secure1PSID)
	secure1PSIDTS = cleanCookie(secure1PSIDTS)
	if secure1PSID == "" {
		return errors.New("secure_1psid is required")
	}

	c.statusMu.RLock()
	oldHealthState := c.healthState
	oldLastError := c.lastError
	oldLastValidated := c.lastValidated
	oldLastCookieSync := c.lastCookieSync
	c.statusMu.RUnlock()

	c.setAccountState(AccountStateRefreshing, nil)

	c.cookies.mu.Lock()
	oldPSID := c.cookies.Secure1PSID
	oldPSIDTS := c.cookies.Secure1PSIDTS
	oldUpdatedAt := c.cookies.UpdatedAt
	c.cookies.Secure1PSID = secure1PSID
	c.cookies.Secure1PSIDTS = secure1PSIDTS
	c.cookies.UpdatedAt = time.Now()
	c.cookies.mu.Unlock()

	if err := c.refreshSessionToken(); err != nil {
		c.cookies.mu.Lock()
		c.cookies.Secure1PSID = oldPSID
		c.cookies.Secure1PSIDTS = oldPSIDTS
		c.cookies.UpdatedAt = oldUpdatedAt
		c.cookies.mu.Unlock()
		c.statusMu.Lock()
		c.healthState = oldHealthState
		c.lastError = oldLastError
		c.lastValidated = oldLastValidated
		c.lastCookieSync = oldLastCookieSync
		c.statusMu.Unlock()
		return fmt.Errorf("validate updated cookies: %w", err)
	}

	c.httpClient.SetCommonCookies(c.cookies.ToHTTPCookies()...)
	if err := c.SaveCachedCookies(); err != nil && c.log != nil {
		c.log.Warn("failed to save updated cookies", zap.String("account", c.accountID), zap.Error(err))
	}
	if c.cookieCache {
		if err := saveAccountCookieCache(c.cookieCachePath, c.accountID, secure1PSID, secure1PSIDTS, c.proxyURL, "worker"); err != nil && c.log != nil {
			c.log.Warn("failed to save account cookie cache", zap.String("account", c.accountID), zap.Error(err))
		}
	}
	c.cookieSource = "worker"

	c.statusMu.Lock()
	c.healthState = AccountStateHealthy
	c.lastError = ""
	c.lastValidated = time.Now()
	c.lastCookieSync = time.Now()
	c.statusMu.Unlock()

	c.mu.Lock()
	c.healthy = true
	c.mu.Unlock()

	return nil
}

func (c *Client) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {
	config := &GenerateConfig{}
	for _, opt := range options {
		opt(config)
	}

	// Default to first available model if not set or "gemini-pro"
	c.mu.RLock()
	aliases := c.cachedAliases
	if config.Model == "" || config.Model == "gemini-pro" {
		if len(c.cachedModels) > 0 {
			config.Model = c.cachedModels[0].ID
		}
	}

	requestedModel := config.Model
	if id, ok := aliases[requestedModel]; ok {
		config.Model = id
	}
	resolvedModel, _ := resolveAvailableModel(config.Model, c.cachedModels)
	config.Model = resolvedModel
	at := c.at
	cookieHdr := c.cookieHeader
	buildLabel := c.buildLabel
	sessionID := c.sessionID
	language := c.language
	c.mu.RUnlock()
	if language == "" {
		language = "en"
	}

	// Temporarily bypass model check for testing name-based models
	// if !found && requestedModel != "" {
	// 	return nil, fmt.Errorf("model '%s' is not supported or not available. Available models: %v", requestedModel, c.ListModelsIDs())
	// }

	if at == "" {
		return nil, errors.New("client not initialized")
	}

	uploadedFiles, err := c.uploadRequestFiles(ctx, config, cookieHdr)
	if err != nil {
		return nil, err
	}

	requestID := strings.ToUpper(uuid.NewString())
	resolvedModelID := config.Model
	expectedConversationCID := c.conversationID(config.ConversationID)
	sourcePath := ""
	if config.SourcePath {
		sourcePath = c.conversationSourcePath(config.ConversationID)
	}
	inner := buildGenerateInner(prompt, uploadedFiles, language, requestID, c.conversationMetadata(config.ConversationID), c.conversationContextToken(config.ConversationID))

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)

	// Encode form body manually to have full control over the request
	formValues := url.Values{}
	formValues.Set("at", at)
	formValues.Set("f.req", string(outerJSON))
	formBody := formValues.Encode()

	queryValues := url.Values{}
	queryValues.Set("at", at)
	queryValues.Set("hl", language)
	queryValues.Set("_reqid", c.nextRequestID())
	queryValues.Set("rt", "c")
	if sourcePath != "" {
		queryValues.Set("source-path", sourcePath)
	}
	if buildLabel != "" {
		queryValues.Set("bl", buildLabel)
	}
	if sessionID != "" {
		queryValues.Set("f.sid", sessionID)
	}
	generateURL := EndpointGenerate + "?" + queryValues.Encode()

	maxAttempts := c.maxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	// Use rawHTTPClient for connection reuse
	plainClient := c.rawHTTPClient

	totalStart := time.Now()

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(1<<uint(attempt-2)) * time.Second
			c.log.Warn("Retrying GenerateContent",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", maxAttempts),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr),
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		httpStart := time.Now()

		httpReq, err := http.NewRequestWithContext(ctx, "POST", generateURL, strings.NewReader(formBody))
		if err != nil {
			lastErr = fmt.Errorf("failed to build generate request: %w", err)
			continue
		}
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
		httpReq.Header.Set("Origin", "https://gemini.google.com")
		httpReq.Header.Set("Referer", "https://gemini.google.com/")
		httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		httpReq.Header.Set("X-Same-Domain", "1")
		// Model ID + tracking headers are mandatory for streaming; see setGenerationHeaders.
		setGenerationHeaders(httpReq, resolvedModelID, requestID, config.ThinkingLevel)
		if cookieHdr != "" {
			httpReq.Header.Set("Cookie", cookieHdr)
		}
		c.log.Info("Gemini generate request prepared",
			zap.String("account", c.accountID),
			zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
			zap.String("proxy", redactProxyURL(c.proxyURL)),
			zap.String("model", resolvedModelID),
			zap.String("request_id", requestID),
			zap.Int("attempt", attempt),
			zap.Bool("has_conversation_id", strings.TrimSpace(config.ConversationID) != ""),
		)

		httpResp, err := plainClient.Do(httpReq)
		httpDuration := time.Since(httpStart)
		if err != nil {
			c.log.Warn("Generate request failed, will retry",
				zap.Error(err),
				zap.Duration("http_duration", httpDuration),
				zap.Int("attempt", attempt),
			)
			lastErr = err
			continue
		}
		defer httpResp.Body.Close()

		if httpResp.StatusCode != http.StatusOK {
			bodySnippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
			lastErr = fmt.Errorf("generate failed with status: %d", httpResp.StatusCode)
			c.log.Warn("Generate returned non-200",
				zap.Int("status", httpResp.StatusCode),
				zap.String("body_snippet", string(bodySnippet)),
				zap.Int("attempt", attempt),
			)
			if httpResp.StatusCode >= 500 {
				continue
			}
			return nil, lastErr
		}

		respBytes, err := io.ReadAll(httpResp.Body)
		if err != nil {
			lastErr = fmt.Errorf("failed to read generate response: %w", err)
			continue
		}
		respBody := string(respBytes)

		parseStart := time.Now()
		result, parseErr := c.parseResponse(respBody)
		parseDuration := time.Since(parseStart)

		if parseErr != nil {
			lastErr = parseErr
			c.log.Warn("Failed to parse response, will retry",
				zap.Error(parseErr),
				zap.Int("attempt", attempt),
			)
			continue
		}
		if err := c.checkConversationContinuity(config.ConversationID, expectedConversationCID, result.Metadata, false); err != nil {
			return nil, err
		}
		c.updateConversation(config.ConversationID, result.Metadata)

		c.log.Debug("GenerateContent timing",
			zap.Duration("gemini_server_rtt", httpDuration),
			zap.Duration("parse_duration", parseDuration),
			zap.Duration("total_duration", time.Since(totalStart)),
			zap.Int("attempt", attempt),
			zap.Int("response_bytes", len(respBody)),
		)

		if attempt > 1 {
			c.log.Info("GenerateContent succeeded after retry", zap.Int("attempt", attempt))
		}
		return result, nil
	}

	c.log.Error("GenerateContent failed after all attempts",
		zap.Int("attempts", maxAttempts),
		zap.Error(lastErr),
	)
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

type StreamCallback func(deltaText string) bool

type StreamEventCallback func(event StreamEvent) bool

type StreamEvent struct {
	Kind  string
	Delta string
}

type StreamState struct {
	ThinkingTexts []string
	HasContent    bool
}

const maxStreamParseBufferBytes = 512 * 1024
const defaultStreamFinishIdleTimeout = 1500 * time.Millisecond
const defaultStreamFirstContentTimeout = 45 * time.Second

// streamParseMinIntervalBytes limits how often the full parse buffer is
// re-scanned for text deltas. Without this, every 16 KB chunk triggers a
// full O(n) scan of up to 512 KB, making long responses O(n²) in CPU.
const streamParseMinIntervalBytes = 8 * 1024

func (c *Client) GenerateContentStreamForOpenAI(ctx context.Context, prompt string, onEvent func(event StreamEvent) bool, options ...GenerateOption) error {
	state := &StreamState{}
	return c.generateContentStreamInternal(ctx, prompt, func(buffer []byte, deltaText string) bool {
		if deltaText != "" {
			state.HasContent = true
			return onEvent(StreamEvent{Kind: "content_delta", Delta: deltaText})
		}
		if state.HasContent {
			return true
		}
		nextState := ExtractStreamState(buffer)
		for _, text := range nextState.ThinkingTexts {
			delta := nextThinkingDelta(state, text)
			if delta == "" {
				continue
			}
			if !onEvent(StreamEvent{Kind: "thinking_text", Delta: delta}) {
				return false
			}
		}
		return true
	}, options...)
}

func nextThinkingDelta(state *StreamState, text string) string {
	text = strings.TrimSpace(text)
	if text == "" || containsString(state.ThinkingTexts, text) {
		return ""
	}

	bestPrefix := ""
	for _, previous := range state.ThinkingTexts {
		if len(previous) > len(bestPrefix) && len(previous) < len(text) && strings.HasPrefix(text, previous) {
			bestPrefix = previous
		}
	}
	state.ThinkingTexts = append(state.ThinkingTexts, text)
	if bestPrefix != "" {
		return strings.TrimPrefix(text, bestPrefix)
	}
	return text
}

func (c *Client) GenerateContentStream(ctx context.Context, prompt string, onChunk StreamCallback, options ...GenerateOption) error {
	return c.generateContentStreamInternal(ctx, prompt, func(_ []byte, deltaText string) bool {
		if deltaText == "" {
			return true
		}
		return onChunk(deltaText)
	}, options...)
}

func (c *Client) generateContentStreamInternal(ctx context.Context, prompt string, onEvent func(buffer []byte, deltaText string) bool, options ...GenerateOption) error {
	config := &GenerateConfig{}
	for _, opt := range options {
		opt(config)
	}

	c.mu.RLock()
	aliases := c.cachedAliases
	if config.Model == "" || config.Model == "gemini-pro" {
		if len(c.cachedModels) > 0 {
			config.Model = c.cachedModels[0].ID
		}
	}
	if id, ok := aliases[config.Model]; ok {
		config.Model = id
	}
	resolvedModel, _ := resolveAvailableModel(config.Model, c.cachedModels)
	config.Model = resolvedModel
	at := c.at
	cookieHdr := c.cookieHeader
	buildLabel := c.buildLabel
	sessionID := c.sessionID
	language := c.language
	c.mu.RUnlock()
	if language == "" {
		language = "en"
	}
	if at == "" {
		return errors.New("client not initialized")
	}

	traceEnabled := getEnvBoolStrict("GEMINI_TRACE_STREAM")
	streamStart := time.Now()
	logTrace := func(message string, fields ...zap.Field) {
		if !traceEnabled {
			return
		}
		base := []zap.Field{
			zap.String("model", config.Model),
			zap.Duration("elapsed", time.Since(streamStart)),
		}
		c.log.Info(message, append(base, fields...)...)
	}

	uploadedFiles, err := c.uploadRequestFiles(ctx, config, cookieHdr)
	if err != nil {
		return err
	}

	requestID := strings.ToUpper(uuid.NewString())
	resolvedModelID := config.Model
	expectedConversationCID := c.conversationID(config.ConversationID)
	sourcePath := ""
	if config.SourcePath {
		sourcePath = c.conversationSourcePath(config.ConversationID)
	}
	inner := buildGenerateInner(prompt, uploadedFiles, language, requestID, c.conversationMetadata(config.ConversationID), c.conversationContextToken(config.ConversationID))
	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)

	formValues := url.Values{}
	formValues.Set("at", at)
	formValues.Set("f.req", string(outerJSON))
	formBody := formValues.Encode()

	queryValues := url.Values{}
	queryValues.Set("at", at)
	queryValues.Set("hl", language)
	queryValues.Set("_reqid", c.nextRequestID())
	queryValues.Set("rt", "c")
	if sourcePath != "" {
		queryValues.Set("source-path", sourcePath)
	}
	if buildLabel != "" {
		queryValues.Set("bl", buildLabel)
	}
	if sessionID != "" {
		queryValues.Set("f.sid", sessionID)
	}
	generateURL := EndpointGenerate + "?" + queryValues.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", generateURL, strings.NewReader(formBody))
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	httpReq.Header.Set("Origin", "https://gemini.google.com")
	httpReq.Header.Set("Referer", "https://gemini.google.com/")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	httpReq.Header.Set("X-Same-Domain", "1")
	setGenerationHeaders(httpReq, resolvedModelID, requestID, config.ThinkingLevel)
	if cookieHdr != "" {
		httpReq.Header.Set("Cookie", cookieHdr)
	}
	debugCapture := newStreamDebugCapture(requestID, resolvedModelID, c.log)
	if debugCapture != nil {
		defer debugCapture.Close()
		debugCapture.DumpRequest(generateURL, formBody, httpReq.Header)
	}
	maybeDumpStreamRequest(generateURL, formBody, httpReq.Header)
	logTrace("gemini stream request prepared",
		zap.String("request_id", requestID),
		zap.String("url", generateURL),
		zap.String("account", c.accountID),
		zap.Bool("proxy_enabled", strings.TrimSpace(c.proxyURL) != ""),
		zap.String("proxy", redactProxyURL(c.proxyURL)),
		zap.Int("prompt_len", len(prompt)),
		zap.Int("uploaded_files", len(uploadedFiles)),
	)

	requestStart := time.Now()
	httpResp, err := c.rawHTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("stream request failed: %w", err)
	}
	defer httpResp.Body.Close()
	logTrace("gemini upstream response headers received",
		zap.Duration("round_trip", time.Since(requestStart)),
		zap.Int("status", httpResp.StatusCode),
	)
	if httpResp.StatusCode != http.StatusOK {
		bodySnippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		snippet := strings.TrimSpace(string(bodySnippet))
		c.log.Warn("Gemini stream returned non-200",
			zap.Int("status", httpResp.StatusCode),
			zap.String("body_snippet", snippet),
			zap.String("request_id", requestID),
			zap.String("url", generateURL),
		)
		if snippet != "" {
			return fmt.Errorf("stream returned status %d: %s", httpResp.StatusCode, snippet)
		}
		return fmt.Errorf("stream returned status %d", httpResp.StatusCode)
	}

	var lastText string
	var lastContentAt time.Time
	var buf bytes.Buffer
	var lastParseLen int // bytes of buf at last text-extraction attempt; used to throttle re-parses
	continuityChecked := expectedConversationCID == ""
	metadataStored := false
	finalizeStreamConversation := func() error {
		metadata := extractConversationMetadataFromBuffer(buf.Bytes())
		if err := c.checkConversationContinuity(config.ConversationID, expectedConversationCID, metadata, lastText != ""); err != nil {
			return err
		}
		c.updateConversation(config.ConversationID, metadata)
		return nil
	}
	finishIdleTimeout := streamFinishIdleTimeout()
	firstContentTimeout := streamFirstContentTimeout()
	firstContentDeadline := time.Now().Add(firstContentTimeout)
	firstByteLogged := false
	firstTextLogged := false
	entryTrace := newStreamEntryTrace(20)
	if debugCapture != nil {
		defer func() {
			entryTrace.Flush(time.Since(requestStart))
			debugCapture.DumpEntryTrace(entryTrace)
		}()
	}
	var debugStreamFile *os.File
	if debugPath := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_STREAM_PATH")); debugPath != "" {
		if err := os.MkdirAll(filepath.Dir(debugPath), 0755); err == nil {
			if f, err := os.Create(debugPath); err == nil {
				debugStreamFile = f
				defer debugStreamFile.Close()
			} else {
				c.log.Warn("failed to create stream debug capture", zap.String("path", debugPath), zap.Error(err))
			}
		}
	}
	readCtx, stopReader := context.WithCancel(ctx)
	defer stopReader()
	readCh := make(chan streamReadResult, 1)
	go readStreamChunks(readCtx, httpResp.Body, readCh)

	// Create timers once and reuse via Reset to avoid per-iteration allocation.
	var idleTimer *time.Timer
	var firstContentTimer *time.Timer
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		if firstContentTimer != nil {
			firstContentTimer.Stop()
		}
	}()

readLoop:
	for {
		var idleCh <-chan time.Time
		var firstContentCh <-chan time.Time
		if timeout, ok := streamFinishIdleRemaining(lastText, lastContentAt, finishIdleTimeout); ok {
			if timeout <= 0 {
				logTrace("gemini stream finish idle timeout reached",
					zap.Duration("idle_timeout", finishIdleTimeout),
					zap.Int("final_text_len", len(lastText)),
				)
				if err := finalizeStreamConversation(); err != nil {
					return err
				}
				stopReader()
				_ = httpResp.Body.Close()
				return nil
			}
			if idleTimer == nil {
				idleTimer = time.NewTimer(timeout)
			} else {
				idleTimer.Reset(timeout)
			}
			idleCh = idleTimer.C
		}
		if lastText == "" && firstContentTimeout > 0 {
			remaining := time.Until(firstContentDeadline)
			if remaining <= 0 {
				logTrace("gemini stream first content timeout reached",
					zap.Duration("first_content_timeout", firstContentTimeout),
					zap.Int("response_bytes", buf.Len()),
				)
				stopReader()
				_ = httpResp.Body.Close()
				return fmt.Errorf("gemini stream first content timeout after %s", firstContentTimeout)
			}
			if firstContentTimer == nil {
				firstContentTimer = time.NewTimer(remaining)
			} else {
				firstContentTimer.Reset(remaining)
			}
			firstContentCh = firstContentTimer.C
		}

		var result streamReadResult
		select {
		case readResult, ok := <-readCh:
			if !ok {
				break readLoop
			}
			if idleTimer != nil {
				idleTimer.Stop()
			}
			if firstContentTimer != nil {
				firstContentTimer.Stop()
			}
			result = readResult
		case <-idleCh:
			if firstContentTimer != nil {
				firstContentTimer.Stop()
			}
			logTrace("gemini stream finish idle timeout reached",
				zap.Duration("idle_timeout", finishIdleTimeout),
				zap.Int("final_text_len", len(lastText)),
			)
			if err := finalizeStreamConversation(); err != nil {
				return err
			}
			stopReader()
			_ = httpResp.Body.Close()
			return nil
		case <-firstContentCh:
			if idleTimer != nil {
				idleTimer.Stop()
			}
			logTrace("gemini stream first content timeout reached",
				zap.Duration("first_content_timeout", firstContentTimeout),
				zap.Int("response_bytes", buf.Len()),
			)
			stopReader()
			_ = httpResp.Body.Close()
			return fmt.Errorf("gemini stream first content timeout after %s", firstContentTimeout)
		case <-ctx.Done():
			if idleTimer != nil {
				idleTimer.Stop()
			}
			if firstContentTimer != nil {
				firstContentTimer.Stop()
			}
			stopReader()
			_ = httpResp.Body.Close()
			return ctx.Err()
		}

		if len(result.data) > 0 {
			if !firstByteLogged {
				firstByteLogged = true
				logTrace("gemini first upstream bytes received",
					zap.Int("chunk_bytes", len(result.data)),
					zap.Duration("ttfb", time.Since(requestStart)),
				)
			}
			buf.Write(result.data)
			if debugStreamFile != nil {
				_, _ = debugStreamFile.Write(result.data)
			}
			if debugCapture != nil {
				debugCapture.WriteRaw(result.data, time.Since(requestStart))
			}
			entryTrace.CaptureChunk(result.data, time.Since(requestStart))
			if !continuityChecked || !metadataStored {
				metadata := extractConversationMetadataFromBuffer(buf.Bytes())
				if cid, _ := metadata["cid"].(string); cid != "" {
					if !continuityChecked {
						if err := c.checkConversationContinuity(config.ConversationID, expectedConversationCID, metadata, lastText != ""); err != nil {
							return err
						}
						continuityChecked = true
					}
					if !metadataStored {
						c.updateConversation(config.ConversationID, metadata)
						metadataStored = true
					}
				}
			}
			parseBuffer := recentBytes(buf.Bytes(), maxStreamParseBufferBytes)
			if !onEvent(parseBuffer, "") {
				return nil
			}
		}
		parseBuffer := recentBytes(buf.Bytes(), maxStreamParseBufferBytes)
		// Throttle re-parsing: only attempt text extraction when enough new
		// bytes have accumulated since the last attempt, or when the stream
		// has ended (result.err != nil). This avoids O(n²) re-scans of the
		// full buffer on every 16 KB chunk for long responses.
		shouldParse := buf.Len()-lastParseLen >= streamParseMinIntervalBytes || result.err != nil
		if !shouldParse {
			if result.err != nil {
				break readLoop
			}
			continue
		}
		lastParseLen = buf.Len()
		text := extractStreamTextFromBuffer(parseBuffer)
		if text == "" {
			text = extractTextFromBuffer(parseBuffer)
		}
		if text != "" && text != lastText {
			delta := streamTextDelta(lastText, text)
			if delta != "" || strings.HasPrefix(text, lastText) {
				lastText = text
			}
			if delta != "" {
				if !firstTextLogged {
					firstTextLogged = true
					logTrace("gemini first parsed text emitted",
						zap.Int("delta_len", len(delta)),
						zap.Duration("parse_ttfb", time.Since(requestStart)),
					)
				}
				if !onEvent(parseBuffer, delta) {
					return nil
				}
				lastContentAt = time.Now()
			}
		}

		if result.err != nil {
			if result.err != io.EOF {
				return fmt.Errorf("stream read: %w", result.err)
			}
			break readLoop
		}
	}
	entryTrace.Flush(time.Since(requestStart))
	if debugCapture != nil {
		debugCapture.DumpOrderedEntries(buf.Bytes())
	}
	maybeDumpStreamEntryTrace(entryTrace)
	logTrace("gemini stream completed",
		zap.Int("response_bytes", buf.Len()),
		zap.Int("final_text_len", len(lastText)),
	)
	if lastText == "" {
		if code := extractBardErrorCode(buf.Bytes()); code != "" {
			c.markConversationUntrusted(config.ConversationID)
			return fmt.Errorf("gemini bard error %s", code)
		}
		return fmt.Errorf("gemini stream completed without parsed content")
	}
	return finalizeStreamConversation()
}

func extractBardErrorCode(data []byte) string {
	match := bardErrorInfoRegex.FindStringSubmatch(string(data))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

type streamReadResult struct {
	data []byte
	err  error
}

func readStreamChunks(ctx context.Context, body io.Reader, out chan<- streamReadResult) {
	defer close(out)
	chunk := make([]byte, 16*1024)
	for {
		n, err := body.Read(chunk)
		var data []byte
		if n > 0 {
			data = append([]byte(nil), chunk[:n]...)
		}
		select {
		case out <- streamReadResult{data: data, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func streamFinishIdleTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("GEMINI_STREAM_FINISH_IDLE_MS"))
	if value == "" {
		return defaultStreamFinishIdleTimeout
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms < 0 {
		return defaultStreamFinishIdleTimeout
	}
	if ms == 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func streamFirstContentTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS"))
	if value == "" {
		return defaultStreamFirstContentTimeout
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms < 0 {
		return defaultStreamFirstContentTimeout
	}
	if ms == 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func streamFinishIdleRemaining(lastText string, lastContentAt time.Time, timeout time.Duration) (time.Duration, bool) {
	if timeout <= 0 || lastText == "" || lastContentAt.IsZero() {
		return 0, false
	}
	return timeout - time.Since(lastContentAt), true
}

var uRegex = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

func decodeUnicodeEscapes(s string) string {
	return uRegex.ReplaceAllStringFunc(s, func(m string) string {
		r, err := strconv.ParseInt(m[2:], 16, 64)
		if err != nil {
			return m
		}
		return string(rune(r))
	})
}

func extractTextFromBuffer(data []byte) string {
	s := string(data)
	idx := strings.LastIndex(s, `\"rc_`)
	if idx < 0 {
		return ""
	}
	after := s[idx:]
	delim := `\",[\"`
	idx = strings.Index(after, delim)
	if idx < 0 {
		return ""
	}
	after = after[idx+len(delim):]
	end := `\"],`
	idx = strings.Index(after, end)
	if idx < 0 {
		idx = strings.Index(after, `\"]`)
		if idx < 0 {
			return ""
		}
	}
	text := after[:idx]
	text = strings.ReplaceAll(text, `\\\"`, `\"`)
	text = strings.ReplaceAll(text, `\\`, `\`)
	text = decodeUnicodeEscapes(text)
	text = strings.ReplaceAll(text, `\"`, `"`)
	text = strings.ReplaceAll(text, `\n`, "\n")
	return text
}

func isDigitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func extractStreamText(line string) string {
	texts := extractCandidateResponseTexts(line)
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func streamTextDelta(previous, current string) string {
	if current == "" || current == previous {
		return ""
	}
	if previous == "" {
		return current
	}
	if strings.HasPrefix(current, previous) {
		return strings.TrimPrefix(current, previous)
	}
	if strings.HasPrefix(previous, current) {
		return ""
	}
	return ""
}

func recentBytes(data []byte, limit int) []byte {
	if limit <= 0 || len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}

func extractStreamTextFromBuffer(data []byte) string {
	s := string(data)
	var latest string
	for _, rawLine := range strings.Split(s, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ")]}'") {
			continue
		}
		text := extractStreamText(line)
		if text != "" {
			latest = text
			continue
		}
		decoded, err := strconv.Unquote(`"` + strings.ReplaceAll(line, `"`, `\"`) + `"`)
		if err == nil {
			text = extractStreamText(decoded)
			if text != "" {
				latest = text
			}
		}
	}
	return latest
}

func extractCandidateTexts(line string) []string {
	entries := parseStreamEntries(line)
	if len(entries) == 0 {
		return nil
	}

	var texts []string
	for _, parsed := range entries {
		texts = append(texts, flattenStringValues(parsed)...)
	}
	return uniqueStrings(texts)
}

func extractCandidateResponseTexts(line string) []string {
	entries := parseStreamEntries(line)
	if len(entries) == 0 {
		return nil
	}

	var texts []string
	for _, parsed := range entries {
		inner, ok := parsed.([]interface{})
		if !ok {
			continue
		}
		candidates, ok := getArrayPath(inner, 4)
		if !ok {
			continue
		}
		for _, rawCandidate := range candidates {
			candidate, ok := rawCandidate.([]interface{})
			if !ok || len(candidate) < 2 {
				continue
			}
			parts, ok := candidate[1].([]interface{})
			if !ok {
				continue
			}
			var candidateText strings.Builder
			for _, rawPart := range parts {
				text, ok := rawPart.(string)
				if ok && shouldKeepStreamText(text) {
					candidateText.WriteString(text)
				}
			}
			if candidateText.Len() > 0 {
				texts = append(texts, candidateText.String())
			}
		}
	}
	return uniqueStrings(texts)
}

func parseStreamEntries(line string) []interface{} {
	var outer []interface{}
	if err := json.Unmarshal([]byte(line), &outer); err != nil {
		return nil
	}
	if len(outer) == 0 {
		return nil
	}

	var parsedEntries []interface{}
	for _, item := range outer {
		entry, ok := item.([]interface{})
		if !ok || len(entry) < 3 {
			continue
		}
		innerStr, ok := entry[2].(string)
		if !ok || strings.TrimSpace(innerStr) == "" {
			continue
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(innerStr), &parsed); err != nil {
			continue
		}
		if nested, ok := parsed.(string); ok {
			if err := json.Unmarshal([]byte(nested), &parsed); err != nil {
				continue
			}
		}
		parsedEntries = append(parsedEntries, parsed)
	}
	return parsedEntries
}

func flattenStringValues(v interface{}) []string {
	switch typed := v.(type) {
	case string:
		if shouldKeepStreamText(typed) {
			return []string{typed}
		}
		return nil
	case []interface{}:
		var result []string
		for _, item := range typed {
			result = append(result, flattenStringValues(item)...)
		}
		return result
	default:
		return nil
	}
}

func shouldKeepStreamText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "c_") || strings.HasPrefix(s, "r_") || strings.HasPrefix(s, "rc_") {
		return false
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "//") {
		return false
	}
	if len(s) > 256 {
		return true
	}
	if strings.ContainsAny(s, " \n\t") {
		return true
	}
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}

func getArrayPath(data []interface{}, path ...int) ([]interface{}, bool) {
	var current interface{} = data
	for _, idx := range path {
		arr, ok := current.([]interface{})
		if !ok || idx >= len(arr) {
			return nil, false
		}
		current = arr[idx]
	}
	result, ok := current.([]interface{})
	return result, ok
}

func processStreamEntries(line string, lastText *string, onChunk StreamCallback, log *zap.Logger) {
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(line), &entries); err != nil {
		log.Debug("stream entries parse failed", zap.Error(err))
		return
	}
	for _, entry := range entries {
		var wrb []interface{}
		if err := json.Unmarshal(entry, &wrb); err != nil || len(wrb) < 3 {
			continue
		}
		innerStr, ok := wrb[2].(string)
		if !ok {
			continue
		}
		var inner []interface{}
		if err := json.Unmarshal([]byte(innerStr), &inner); err != nil || len(inner) < 5 {
			continue
		}
		candidates, ok := getArrayPath(inner, 4)
		if !ok || len(candidates) == 0 {
			continue
		}
		first, ok := candidates[0].([]interface{})
		if !ok || len(first) < 2 {
			continue
		}
		contentParts, ok := first[1].([]interface{})
		if !ok || len(contentParts) == 0 {
			continue
		}
		text, _ := contentParts[0].(string)
		if text == "" || text == *lastText {
			continue
		}
		delta := strings.TrimPrefix(text, *lastText)
		*lastText = text
		if delta != "" {
			if !onChunk(delta) {
				return
			}
		}
	}
}

func resolveAvailableModel(requested string, models []ModelInfo) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}

	for _, model := range models {
		if model.ID == requested {
			return model.ID, true
		}
	}

	var matches []string
	prefix := requested + "-"
	for _, model := range models {
		if strings.HasPrefix(model.ID, prefix) {
			matches = append(matches, model.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}

	return requested, false
}

// buildGenerateInner constructs the inner JSON array for the StreamGenerate
// request body. The structure (81 elements) mirrors exactly what the Gemini
// web frontend sends, which is required for the server to respond in true
// token-level streaming mode. If the structure deviates, the server silently
// falls back to a batch mode that only returns the full response once
// generation is complete (40-60s latency for long outputs).
//
// IMPORTANT: the model ID is NOT part of this structure. It is passed via the
// x-goog-ext-525001261-jspb request header (see setGenerationHeaders).
// inner[3] historically held the model in this codebase, but on the real web
// client it carries an opaque conversation-context token. We leave it nil for
// new conversations, and fill it only when callers opt into server-side context.
func buildGenerateInner(prompt string, files []uploadedFile, language, requestID string, conversationMetadata []interface{}, conversationContext interface{}) []interface{} {
	// inner[0]: message content.
	// No attachments:  [prompt, 0, null, null, null, null, 0]
	// With attachments:[prompt, 0, null, fileData, null, nil, 0]
	var messageContent []interface{}
	if len(files) == 0 {
		messageContent = []interface{}{prompt, 0, nil, nil, nil, nil, 0}
	} else {
		fileData := make([]interface{}, 0, len(files))
		for _, file := range files {
			fileData = append(fileData, []interface{}{[]interface{}{file.ID}, file.Name})
		}
		messageContent = []interface{}{prompt, 0, nil, fileData, nil, nil, 0}
	}

	defaultMetadata := []interface{}{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	if len(conversationMetadata) > 0 {
		defaultMetadata = conversationMetadata
	}

	// 81-element structure captured from the live web client (see debug_req_*).
	inner := make([]interface{}, 81)
	inner[0] = messageContent
	inner[1] = []interface{}{language}
	inner[2] = defaultMetadata
	inner[3] = conversationContext // conversation-context token; nil for first turn
	inner[4] = nil                 // response_id; assigned by server
	inner[6] = []interface{}{0}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{0}}
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	inner[59] = requestID
	inner[61] = []interface{}{}
	inner[68] = 1
	inner[79] = 6
	inner[80] = 1
	return inner
}

func (c *Client) conversationMetadata(id string) []interface{} {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}

	c.conversationMu.RLock()
	defer c.conversationMu.RUnlock()
	meta := c.conversations[id]
	if meta == nil || (meta.ConversationID == "" && meta.ResponseID == "" && meta.ChoiceID == "") {
		return nil
	}
	return []interface{}{meta.ConversationID, meta.ResponseID, meta.ChoiceID, nil, nil, nil, nil, nil, nil, ""}
}

func (c *Client) conversationContextToken(id string) interface{} {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}

	c.conversationMu.RLock()
	defer c.conversationMu.RUnlock()
	meta := c.conversations[id]
	if meta == nil || meta.Extra == nil {
		return nil
	}
	token, _ := meta.Extra["context_token"].(string)
	if token == "" {
		return nil
	}
	return token
}

func (c *Client) conversationSourcePath(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}

	c.conversationMu.RLock()
	defer c.conversationMu.RUnlock()
	meta := c.conversations[id]
	if meta == nil || meta.ConversationID == "" {
		return ""
	}
	conversationPathID := strings.TrimPrefix(meta.ConversationID, "c_")
	if conversationPathID == "" {
		return ""
	}
	return "/app/" + conversationPathID
}

func (c *Client) conversationID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}

	c.conversationMu.RLock()
	defer c.conversationMu.RUnlock()
	meta := c.conversations[id]
	if meta == nil {
		return ""
	}
	return meta.ConversationID
}

func (c *Client) HasConversationState(id string) bool {
	return c.conversationID(id) != ""
}

func (c *Client) ListAccountStatuses() []AccountStatus {
	status := c.AccountStatus()
	status.Active = true
	return []AccountStatus{status}
}

func (c *Client) UpdateAccountCookies(ctx context.Context, accountID, secure1PSID, secure1PSIDTS string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID != "" && accountID != c.accountID {
		return fmt.Errorf("Gemini account %q not found", accountID)
	}
	return c.UpdateCookies(ctx, secure1PSID, secure1PSIDTS)
}

func (c *Client) RefreshAccount(ctx context.Context, accountID string) error {
	_ = ctx
	accountID = strings.TrimSpace(accountID)
	if accountID != "" && accountID != c.accountID {
		return fmt.Errorf("Gemini account %q not found", accountID)
	}
	c.setAccountState(AccountStateRefreshing, nil)
	if err := c.RotateCookies(); err != nil {
		c.setAccountState(AccountStateExpired, err)
		return err
	}
	if err := c.refreshSessionToken(); err != nil {
		c.setAccountState(AccountStateExpired, err)
		return err
	}
	c.mu.Lock()
	c.healthy = true
	c.mu.Unlock()
	c.setAccountState(AccountStateHealthy, nil)
	return nil
}

// UpdateAccountProxy is not supported on single-client mode.
func (c *Client) UpdateAccountProxy(ctx context.Context, accountID, proxyURL string) error {
	return fmt.Errorf("not supported in single-account mode")
}

// AddAccount is not supported on single-client mode.
func (c *Client) AddAccount(ctx context.Context, accountID, secure1PSID, secure1PSIDTS, proxyURL string) error {
	return fmt.Errorf("not supported in single-account mode")
}

// RemoveAccount is not supported on single-client mode.
func (c *Client) RemoveAccount(ctx context.Context, accountID string) error {
	return fmt.Errorf("not supported in single-account mode")
}

// TestAccount sends a simple test message through this client to verify the model works.
func (c *Client) TestAccount(ctx context.Context, accountID string) (string, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID != "" && accountID != c.accountID {
		return "", fmt.Errorf("Gemini account %q not found", accountID)
	}
	resp, err := c.GenerateContent(ctx, "Hi, please reply with only the word: OK")
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("empty response")
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return "", fmt.Errorf("empty text in response")
	}
	return text, nil
}

func (c *Client) checkConversationContinuity(id, expectedCID string, metadata map[string]any, contentAlreadyEmitted bool) error {
	id = strings.TrimSpace(id)
	expectedCID = strings.TrimSpace(expectedCID)
	if id == "" || expectedCID == "" || len(metadata) == 0 {
		return nil
	}
	actualCID, _ := metadata["cid"].(string)
	actualCID = strings.TrimSpace(actualCID)
	if actualCID == "" || actualCID == expectedCID {
		return nil
	}

	if c.log != nil {
		c.log.Warn("Gemini conversation continuity mismatch",
			zap.String("client_conversation_id", id),
			zap.String("expected_cid", expectedCID),
			zap.String("actual_cid", actualCID),
			zap.Bool("content_already_emitted", contentAlreadyEmitted),
		)
	}
	// Mark this provider conversation as untrusted so the OpenAI layer can fall
	// back to a locally reconstructed full-history prompt. Even when content has
	// already been emitted (silent mismatch), we flag the provider conversation
	// so subsequent turns do not keep trusting a broken Gemini-side record.
	c.markConversationUntrusted(id)
	if contentAlreadyEmitted {
		return nil
	}
	return fmt.Errorf("gemini conversation continuity mismatch: expected cid %s, got %s", expectedCID, actualCID)
}

func (c *Client) updateConversation(id string, metadata map[string]any) {
	id = strings.TrimSpace(id)
	if id == "" || len(metadata) == 0 {
		return
	}

	cid, _ := metadata["cid"].(string)
	rid, _ := metadata["rid"].(string)
	rcid, _ := metadata["rcid"].(string)
	contextToken, _ := metadata["context_token"].(string)
	if cid == "" && rid == "" && rcid == "" && contextToken == "" {
		return
	}

	c.conversationMu.Lock()
	defer c.conversationMu.Unlock()
	if c.conversations == nil {
		c.conversations = make(map[string]*SessionMetadata)
	}
	if c.conversationSeen == nil {
		c.conversationSeen = make(map[string]time.Time)
	}
	c.pruneConversationsLocked(time.Now())
	meta := c.conversations[id]
	if meta == nil {
		meta = &SessionMetadata{}
		c.conversations[id] = meta
	}
	c.conversationSeen[id] = time.Now()
	if cid != "" {
		meta.ConversationID = cid
	}
	if rid != "" {
		meta.ResponseID = rid
	}
	if rcid != "" {
		meta.ChoiceID = rcid
	}
	if contextToken != "" {
		if meta.Extra == nil {
			meta.Extra = make(map[string]any)
		}
		meta.Extra["context_token"] = contextToken
	}
}

func (c *Client) pruneConversationsLocked(now time.Time) {
	for id, updatedAt := range c.conversationSeen {
		if now.Sub(updatedAt) > conversationCacheTTL {
			delete(c.conversations, id)
			delete(c.conversationSeen, id)
			delete(c.conversationUntrusted, id)
		}
	}
	for len(c.conversations) > maxConversationCacheEntries {
		var oldestID string
		var oldestTime time.Time
		for id := range c.conversations {
			updatedAt := c.conversationSeen[id]
			if oldestID == "" || updatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = updatedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(c.conversations, oldestID)
		delete(c.conversationSeen, oldestID)
		delete(c.conversationUntrusted, oldestID)
	}
}

// markConversationUntrusted flags a provider conversation id as having a
// potentially broken Gemini-side record (continuity mismatch or bard error).
// The OpenAI layer can consult IsConversationUntrusted to decide whether to
// reconstruct the prompt from the locally retained full history instead of
// continuing to trust the server-side context.
func (c *Client) markConversationUntrusted(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	c.conversationMu.Lock()
	defer c.conversationMu.Unlock()
	if c.conversationUntrusted == nil {
		c.conversationUntrusted = make(map[string]bool)
	}
	c.conversationUntrusted[id] = true
}

// IsConversationUntrusted reports whether the provider conversation id has been
// flagged as untrusted. Safe to call on a Client that has no state for the id.
func (c *Client) IsConversationUntrusted(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	c.conversationMu.RLock()
	defer c.conversationMu.RUnlock()
	return c.conversationUntrusted[id]
}

func extractConversationMetadataFromBuffer(data []byte) map[string]any {
	payload := strings.TrimSpace(string(data))
	if strings.HasPrefix(payload, ")]}'") {
		if idx := strings.Index(payload, "\n"); idx >= 0 {
			payload = strings.TrimSpace(payload[idx+1:])
		}
	}

	var latest map[string]any
	var outer []interface{}
	err := json.Unmarshal([]byte(payload), &outer)
	if err != nil {
		outer, err = decodeStreamEntryArrays(payload)
	}
	if err == nil {
		for _, rawEntry := range outer {
			entry, ok := rawEntry.([]interface{})
			if !ok || len(entry) < 3 {
				continue
			}
			payloadStr, ok := entry[2].(string)
			if !ok || strings.TrimSpace(payloadStr) == "" {
				continue
			}
			var parsedPayload []interface{}
			if err := json.Unmarshal([]byte(payloadStr), &parsedPayload); err != nil {
				continue
			}
			if metadata := extractConversationMetadataFromPayload(parsedPayload); len(metadata) > 0 {
				latest = mergeConversationMetadata(latest, metadata)
			}
		}
		return latest
	}

	for _, entry := range extractCompleteStreamEntries(payload) {
		if len(entry) < 3 {
			continue
		}
		payloadStr, ok := entry[2].(string)
		if !ok || strings.TrimSpace(payloadStr) == "" {
			continue
		}
		var parsedPayload []interface{}
		if err := json.Unmarshal([]byte(payloadStr), &parsedPayload); err != nil {
			continue
		}
		if metadata := extractConversationMetadataFromPayload(parsedPayload); len(metadata) > 0 {
			latest = mergeConversationMetadata(latest, metadata)
		}
	}
	if len(latest) > 0 {
		return latest
	}

	for _, rawLine := range strings.Split(payload, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		for _, parsed := range parseStreamEntries(line) {
			parsedPayload, ok := parsed.([]interface{})
			if !ok {
				continue
			}
			if metadata := extractConversationMetadataFromPayload(parsedPayload); len(metadata) > 0 {
				latest = mergeConversationMetadata(latest, metadata)
			}
		}
	}
	return latest
}

func extractCompleteStreamEntries(payload string) [][]interface{} {
	var entries [][]interface{}
	searchFrom := 0
	for {
		rel := strings.Index(payload[searchFrom:], `["wrb.fr"`)
		if rel < 0 {
			break
		}
		start := searchFrom + rel
		end := findJSONArrayEnd(payload, start)
		if end < 0 {
			break
		}
		var entry []interface{}
		if err := json.Unmarshal([]byte(payload[start:end]), &entry); err == nil {
			entries = append(entries, entry)
		}
		searchFrom = end
	}
	return entries
}

func findJSONArrayEnd(s string, start int) int {
	if start < 0 || start >= len(s) || s[start] != '[' {
		return -1
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func extractConversationMetadataFromPayload(payload []interface{}) map[string]any {
	var cid, rid, rcid string
	if len(payload) > 1 {
		switch ids := payload[1].(type) {
		case string:
			cid = ids
		case []interface{}:
			if len(ids) > 0 {
				cid, _ = ids[0].(string)
			}
			if len(ids) > 1 {
				rid, _ = ids[1].(string)
			}
		}
	}
	if len(payload) > 2 {
		if meta, ok := payload[2].(map[string]interface{}); ok {
			if rid == "" {
				rid, _ = meta["18"].(string)
			}
			if rawTokens, ok := meta["21"].([]interface{}); ok && len(rawTokens) > 0 {
				if token, ok := rawTokens[0].(string); ok && token != "" {
					return map[string]any{
						"cid":           cid,
						"rid":           rid,
						"rcid":          rcid,
						"context_token": token,
					}
				}
			}
		}
	}
	if candidates, ok := getArrayPath(payload, 4); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].([]interface{}); ok && len(candidate) > 0 {
			rcid, _ = candidate[0].(string)
		}
	}
	if cid == "" && rid == "" && rcid == "" {
		return nil
	}
	return map[string]any{
		"cid":  cid,
		"rid":  rid,
		"rcid": rcid,
	}
}

func mergeConversationMetadata(base, next map[string]any) map[string]any {
	if len(base) == 0 {
		base = make(map[string]any)
	}
	for _, key := range []string{"cid", "rid", "rcid", "context_token"} {
		if value, _ := next[key].(string); value != "" {
			base[key] = value
		}
	}
	if len(base) == 0 {
		return nil
	}
	return base
}

// setGenerationHeaders applies the x-goog-ext-* headers that the Gemini web
// client sends with StreamGenerate. The model ID lives in
// x-goog-ext-525001261-jspb (NOT in the request body); omitting these headers
// causes the server to respond in non-streaming batch mode.
//
// Captured structure (debug_req_425):
//
//	x-goog-ext-525001261-jspb: [1,null,null,null,"<modelID>",null,null,0,[4,5,6,8],null,null,1,null,null,6,1,"<reqUUID>"]
//	x-goog-ext-525005358-jspb: ["<reqUUID>",1]
//	x-goog-ext-73010989-jspb:  [0]
//	x-goog-ext-73010990-jspb:  [0,0,0]
//
// Newer Gemini web builds vary one field by model family. For gemini-3.1-pro,
// the 15th element is 3 instead of 1 in live captures. The 16th element carries
// the Flash thinking level: 1=standard, 2=extended.
func setGenerationHeaders(req *http.Request, model, requestID, thinkingLevel string) {
	mode := generationHeaderMode(model)
	thinkingMode := generationThinkingMode(thinkingLevel)
	modelExt := []interface{}{
		1, nil, nil, nil, model, nil, nil, nil,
		[]interface{}{4, 5, 6, 8}, nil, nil, nil, nil, nil, mode, thinkingMode, requestID,
	}
	req.Header.Set("x-goog-ext-525001261-jspb", jsonCompact(modelExt))
	req.Header.Set("x-goog-ext-525005358-jspb", fmt.Sprintf(`[%q,1]`, requestID))
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("x-goog-ext-73010990-jspb", "[0,0,0]")
}

func generationHeaderMode(model string) int {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(model, "pro"), strings.Contains(model, "advanced"):
		return 3
	case model == "9d8ca3786ebdfbea":
		return 3
	default:
		return 1
	}
}

func generationThinkingMode(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "extended", "high", "deep":
		return 2
	default:
		return 1
	}
}

func getEnvBoolStrict(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return parsed
}

func maybeDumpStreamRequest(url, formBody string, headers http.Header) {
	path := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_REQUEST_PATH"))
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	payload := map[string]interface{}{
		"url":       url,
		"form_body": formBody,
		"headers":   redactDebugHeaders(headers),
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, body, 0600)
}

type streamDebugCapture struct {
	log        *zap.Logger
	prefix     string
	rawFile    *os.File
	chunkFile  *os.File
	chunkIndex int
}

func newStreamDebugCapture(requestID, model string, log *zap.Logger) *streamDebugCapture {
	dir := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_STREAM_DIR"))
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Warn("failed to create stream debug directory", zap.String("dir", dir), zap.Error(err))
		return nil
	}

	timestamp := time.Now().Format("20060102_150405.000")
	shortID := requestID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	prefix := filepath.Join(dir, fmt.Sprintf("%s_%s_%s", timestamp, sanitizeDebugFilename(model), shortID))
	rawFile, err := os.Create(prefix + ".raw.txt")
	if err != nil {
		log.Warn("failed to create stream raw debug capture", zap.String("path", prefix+".raw.txt"), zap.Error(err))
		return nil
	}
	chunkFile, err := os.Create(prefix + ".chunks.jsonl")
	if err != nil {
		_ = rawFile.Close()
		log.Warn("failed to create stream chunk debug capture", zap.String("path", prefix+".chunks.jsonl"), zap.Error(err))
		return nil
	}
	return &streamDebugCapture{log: log, prefix: prefix, rawFile: rawFile, chunkFile: chunkFile}
}

func (d *streamDebugCapture) DumpRequest(url, formBody string, headers http.Header) {
	if d == nil {
		return
	}
	payload := map[string]interface{}{
		"url":          url,
		"form_body":    formBody,
		"headers":      redactDebugHeaders(headers),
		"raw_path":     filepath.Base(d.prefix + ".raw.txt"),
		"chunks_path":  filepath.Base(d.prefix + ".chunks.jsonl"),
		"entries_path": filepath.Base(d.prefix + ".entries.jsonl"),
		"created_at":   time.Now().Format(time.RFC3339Nano),
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(d.prefix+".request.json", body, 0600); err != nil {
		d.log.Warn("failed to write stream request debug capture", zap.String("path", d.prefix+".request.json"), zap.Error(err))
	}
}

func redactDebugHeaders(headers http.Header) http.Header {
	redacted := make(http.Header, len(headers))
	for key, values := range headers {
		copied := make([]string, len(values))
		copy(copied, values)
		if isSensitiveDebugHeader(key) {
			for i := range copied {
				copied[i] = "[REDACTED]"
			}
		}
		redacted[key] = copied
	}
	return redacted
}

func isSensitiveDebugHeader(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "cookie" || normalized == "authorization" || normalized == "proxy-authorization" || normalized == "x-goog-authuser" {
		return true
	}
	return strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "credential")
}

func (d *streamDebugCapture) WriteRaw(data []byte, elapsed time.Duration) {
	if d == nil || d.rawFile == nil {
		return
	}
	if _, err := d.rawFile.Write(data); err != nil {
		d.log.Warn("failed to write stream raw debug capture", zap.String("path", d.prefix+".raw.txt"), zap.Error(err))
	}
	d.writeChunkRecord(data, elapsed)
}

type streamChunkRecord struct {
	Index     int    `json:"index"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Preview   string `json:"preview"`
}

func (d *streamDebugCapture) writeChunkRecord(data []byte, elapsed time.Duration) {
	if d == nil || d.chunkFile == nil {
		return
	}
	d.chunkIndex++
	record := streamChunkRecord{
		Index:     d.chunkIndex,
		ElapsedMs: elapsed.Milliseconds(),
		Bytes:     len(data),
		SHA256:    sha256Hex(data),
		Preview:   previewString(string(data), 220),
	}
	body, err := json.Marshal(record)
	if err != nil {
		return
	}
	if _, err := d.chunkFile.Write(append(body, '\n')); err != nil {
		d.log.Warn("failed to write stream chunk debug capture", zap.String("path", d.prefix+".chunks.jsonl"), zap.Error(err))
	}
}

type streamOrderedEntryRecord struct {
	Index           int      `json:"index"`
	Kind            string   `json:"kind"`
	PayloadLen      int      `json:"payload_len,omitempty"`
	PayloadSHA256   string   `json:"payload_sha256,omitempty"`
	HasRC           bool     `json:"has_rc,omitempty"`
	Has7            bool     `json:"has_7,omitempty"`
	Has11           bool     `json:"has_11,omitempty"`
	Has18           bool     `json:"has_18,omitempty"`
	Has21           bool     `json:"has_21,omitempty"`
	ContentTextLen  int      `json:"content_text_len,omitempty"`
	ContentDeltaLen int      `json:"content_delta_len,omitempty"`
	ThinkingTexts   []string `json:"thinking_texts,omitempty"`
	Preview         string   `json:"preview,omitempty"`
}

func (d *streamDebugCapture) DumpOrderedEntries(raw []byte) {
	if d == nil {
		return
	}
	records, err := buildOrderedEntryRecords(raw)
	if err != nil {
		d.log.Warn("failed to parse stream ordered entries", zap.String("path", d.prefix+".entries.jsonl"), zap.Error(err))
		return
	}

	var out bytes.Buffer
	for _, record := range records {
		body, err := json.Marshal(record)
		if err != nil {
			continue
		}
		out.Write(body)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(d.prefix+".entries.jsonl", out.Bytes(), 0600); err != nil {
		d.log.Warn("failed to write stream ordered entries", zap.String("path", d.prefix+".entries.jsonl"), zap.Error(err))
	}
}

func buildOrderedEntryRecords(raw []byte) ([]streamOrderedEntryRecord, error) {
	payload := strings.TrimSpace(string(raw))
	if strings.HasPrefix(payload, ")]}'") {
		if idx := strings.Index(payload, "\n"); idx >= 0 {
			payload = strings.TrimSpace(payload[idx+1:])
		}
	}

	outer, err := decodeStreamEntryArrays(payload)
	if err != nil {
		return nil, err
	}

	records := make([]streamOrderedEntryRecord, 0, len(outer))
	lastContentText := ""
	for i, rawEntry := range outer {
		entry, ok := rawEntry.([]interface{})
		if !ok || len(entry) == 0 {
			continue
		}
		kind, _ := entry[0].(string)
		record := streamOrderedEntryRecord{Index: i + 1, Kind: kind}
		if len(entry) > 2 {
			if payload, ok := entry[2].(string); ok {
				record.PayloadLen = len(payload)
				record.PayloadSHA256 = sha256Hex([]byte(payload))
				record.HasRC = strings.Contains(payload, "rc_")
				record.Has7 = strings.Contains(payload, `"7":`) || strings.Contains(payload, `\"7\":`)
				record.Has11 = strings.Contains(payload, `"11":`) || strings.Contains(payload, `\"11\":`)
				record.Has18 = strings.Contains(payload, `"18":`) || strings.Contains(payload, `\"18\":`)
				record.Has21 = strings.Contains(payload, `"21":`) || strings.Contains(payload, `\"21\":`)
				record.Preview = previewString(payload, 220)
			}
		}

		lineBody, err := json.Marshal([]interface{}{entry})
		if err == nil {
			line := string(lineBody)
			contentText := extractStreamText(line)
			if contentText != "" {
				record.ContentTextLen = len(contentText)
				delta := strings.TrimPrefix(contentText, lastContentText)
				if delta == contentText && lastContentText != "" && !strings.HasPrefix(contentText, lastContentText) {
					delta = contentText
				}
				record.ContentDeltaLen = len(delta)
				lastContentText = contentText
			}
			record.ThinkingTexts = extractStreamStateTexts(line)
		}
		records = append(records, record)
	}
	return records, nil
}

func decodeStreamEntryArrays(payload string) ([]interface{}, error) {
	docs := extractStreamJSONDocuments(payload)
	if len(docs) == 0 {
		docs = []string{payload}
	}

	var outer []interface{}
	for _, doc := range docs {
		var decoded []interface{}
		if err := json.Unmarshal([]byte(doc), &decoded); err != nil {
			return nil, err
		}
		if len(decoded) > 0 {
			if _, ok := decoded[0].(string); ok {
				outer = append(outer, decoded)
				continue
			}
		}
		outer = append(outer, decoded...)
	}
	return outer, nil
}

func extractStreamJSONDocuments(payload string) []string {
	var docs []string
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		docs = append(docs, line)
	}
	return docs
}

func (d *streamDebugCapture) DumpEntryTrace(trace *streamEntryTrace) {
	if d == nil || trace == nil {
		return
	}
	body, err := json.MarshalIndent(trace.records, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(d.prefix+".entry_trace.json", body, 0600); err != nil {
		d.log.Warn("failed to write stream entry trace debug capture", zap.String("path", d.prefix+".entry_trace.json"), zap.Error(err))
	}
}

func (d *streamDebugCapture) Close() {
	if d == nil {
		return
	}
	if d.rawFile != nil {
		if err := d.rawFile.Close(); err != nil {
			d.log.Warn("failed to close stream raw debug capture", zap.String("path", d.prefix+".raw.txt"), zap.Error(err))
		}
		d.rawFile = nil
	}
	if d.chunkFile != nil {
		if err := d.chunkFile.Close(); err != nil {
			d.log.Warn("failed to close stream chunk debug capture", zap.String("path", d.prefix+".chunks.jsonl"), zap.Error(err))
		}
		d.chunkFile = nil
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func previewString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func sanitizeDebugFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

type streamEntryRecord struct {
	Index     int    `json:"index"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Length    int    `json:"length"`
	HasRC     bool   `json:"has_rc"`
	Has7      bool   `json:"has_7"`
	Has11     bool   `json:"has_11"`
	Has18     bool   `json:"has_18"`
	Has21     bool   `json:"has_21"`
	Preview   string `json:"preview"`
}

type streamEntryTrace struct {
	limit   int
	seen    map[string]struct{}
	records []streamEntryRecord
	pending string
}

func newStreamEntryTrace(limit int) *streamEntryTrace {
	return &streamEntryTrace{
		limit: limit,
		seen:  make(map[string]struct{}),
	}
}

func (t *streamEntryTrace) CaptureChunk(data []byte, elapsed time.Duration) {
	if t == nil || len(t.records) >= t.limit {
		return
	}

	text := t.pending + string(data)
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		t.pending = ""
	} else {
		t.pending = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ")]}'") {
			continue
		}
		if !strings.Contains(trimmed, "wrb.fr") {
			continue
		}
		if _, ok := t.seen[trimmed]; ok {
			continue
		}
		t.seen[trimmed] = struct{}{}
		preview := trimmed
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.records = append(t.records, streamEntryRecord{
			Index:     len(t.records) + 1,
			ElapsedMs: elapsed.Milliseconds(),
			Length:    len(trimmed),
			HasRC:     strings.Contains(trimmed, "rc_"),
			Has7:      strings.Contains(trimmed, `\"7\":`) || strings.Contains(trimmed, `"7":`),
			Has11:     strings.Contains(trimmed, `\"11\":`) || strings.Contains(trimmed, `"11":`),
			Has18:     strings.Contains(trimmed, `\"18\":`) || strings.Contains(trimmed, `"18":`),
			Has21:     strings.Contains(trimmed, `\"21\":`) || strings.Contains(trimmed, `"21":`),
			Preview:   preview,
		})
		if len(t.records) >= t.limit {
			break
		}
	}
}

func (t *streamEntryTrace) Flush(elapsed time.Duration) {
	if t == nil || strings.TrimSpace(t.pending) == "" || len(t.records) >= t.limit {
		return
	}
	t.CaptureChunk([]byte("\n"), elapsed)
}

func maybeDumpStreamEntryTrace(trace *streamEntryTrace) {
	path := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_ENTRY_TRACE_PATH"))
	if path == "" || trace == nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	body, err := json.MarshalIndent(trace.records, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, body, 0600)
}

func DetectThinkingStateFromStreamBuffer(data []byte) bool {
	state := ExtractStreamState(data)
	return len(state.ThinkingTexts) > 0
}

func ExtractStreamState(data []byte) StreamState {
	state := StreamState{}
	rawText := string(data)
	state.ThinkingTexts = append(state.ThinkingTexts, extractRawStreamStateTexts(rawText)...)
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ")]}'") {
			continue
		}
		state.ThinkingTexts = append(state.ThinkingTexts, extractStreamStateTexts(line)...)
		decoded, err := strconv.Unquote(`"` + strings.ReplaceAll(line, `"`, `\"`) + `"`)
		if err == nil && decoded != line {
			state.ThinkingTexts = append(state.ThinkingTexts, extractStreamStateTexts(decoded)...)
		}
	}
	if strings.Contains(rawText, "Answer now") {
		state.ThinkingTexts = append(state.ThinkingTexts, "Answer now")
	}
	for _, match := range extractQuotedFieldValues(rawText, `\"7\":\[`, `"7":[`) {
		if shouldKeepStateText(match) {
			state.ThinkingTexts = append(state.ThinkingTexts, match)
		}
	}
	state.ThinkingTexts = uniqueStrings(state.ThinkingTexts)
	state.HasContent = extractStreamTextFromBuffer(data) != ""
	return state
}

func extractRawStreamStateTexts(text string) []string {
	var results []string
	results = append(results, extractEscapedRegexMatches(text, regexp.MustCompile(`\\"7\\":\[null,null,null,null,null,\[\\"((?:\\\\.|[^\\"])*)\\"`))...)
	results = append(results, extractEscapedRegexMatches(text, regexp.MustCompile(`\[\[\\"(\*\*(?:\\\\.|[^\\"])*?I'(?:\\\\.|[^\\"])*)\\"`))...)
	return uniqueStrings(results)
}

func extractEscapedRegexMatches(text string, re *regexp.Regexp) []string {
	matches := re.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	results := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		decoded := decodeEscapedJSONString(match[1])
		if shouldKeepStateText(decoded) {
			results = append(results, decoded)
		}
	}
	return results
}

func decodeEscapedJSONString(text string) string {
	text = strings.ReplaceAll(text, `\\`, `\`)
	decoded, err := strconv.Unquote(`"` + strings.ReplaceAll(text, `"`, `\"`) + `"`)
	if err != nil {
		return text
	}
	return decoded
}

func extractStreamStateTexts(line string) []string {
	entries := parseStreamEntries(line)
	if len(entries) == 0 {
		return nil
	}

	var texts []string
	for _, parsed := range entries {
		inner, ok := parsed.([]interface{})
		if !ok {
			continue
		}
		texts = append(texts, extractStreamStateTextsFromMetadata(inner)...)
		texts = append(texts, extractStreamThinkingSummaries(inner)...)
	}
	return uniqueStrings(texts)
}

func extractStreamStateTextsFromMetadata(inner []interface{}) []string {
	metadata, ok := getMapPath(inner, 2)
	if !ok {
		return nil
	}

	var texts []string
	if field7, ok := metadata["7"].([]interface{}); ok && len(field7) > 5 {
		texts = append(texts, flattenStringValues(field7[5])...)
	}
	return uniqueStrings(texts)
}

func extractStreamThinkingSummaries(inner []interface{}) []string {
	candidates, ok := getArrayPath(inner, 4)
	if !ok {
		return nil
	}

	var texts []string
	for _, rawCandidate := range candidates {
		candidate, ok := rawCandidate.([]interface{})
		if !ok || len(candidate) <= 37 {
			continue
		}
		summaryRoot, ok := candidate[37].([]interface{})
		if !ok || len(summaryRoot) == 0 {
			continue
		}
		texts = append(texts, flattenStringValues(summaryRoot[0])...)
	}
	return uniqueStrings(texts)
}

func extractQuotedFieldValues(text string, prefixes ...string) []string {
	var results []string
	for _, prefix := range prefixes {
		start := 0
		for {
			idx := strings.Index(text[start:], prefix)
			if idx < 0 {
				break
			}
			idx += start + len(prefix)
			end := strings.Index(text[idx:], `\"`)
			if end < 0 {
				end = strings.Index(text[idx:], `"`)
				if end < 0 {
					break
				}
			}
			results = append(results, text[idx:idx+end])
			start = idx + end + 1
		}
	}
	return results
}

func shouldKeepStateText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "r_") || strings.HasPrefix(text, "c_") || strings.HasPrefix(text, "rc_") {
		return false
	}
	return true
}

func getMapPath(data []interface{}, path ...int) (map[string]interface{}, bool) {
	var current interface{} = data
	for _, idx := range path {
		arr, ok := current.([]interface{})
		if !ok || idx >= len(arr) {
			return nil, false
		}
		current = arr[idx]
	}
	result, ok := current.(map[string]interface{})
	return result, ok
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// jsonCompact marshals v to a compact JSON string (no whitespace), matching the
// header format the web client sends.
func jsonCompact(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func (c *Client) StartChat(options ...ChatOption) ChatSession {
	config := &ChatConfig{}
	for _, opt := range options {
		opt(config)
	}

	c.mu.RLock()
	if config.Model == "" || config.Model == "gemini-pro" {
		if len(c.cachedModels) > 0 {
			config.Model = c.cachedModels[0].ID
		}
	}
	c.mu.RUnlock()

	return &GeminiChatSession{
		client:   c,
		model:    config.Model,
		metadata: config.Metadata,
		history:  []Message{},
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.stopRefresh) })
	c.mu.Lock()
	c.healthy = false
	c.mu.Unlock()
	c.setAccountState(AccountStateExpired, errors.New("client closed"))
	return nil
}

func (c *Client) GetName() string {
	return "gemini"
}

func (c *Client) IsHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.healthy
}

func (c *Client) ListModels() []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.cachedModels) == 0 {
		return []ModelInfo{}
	}

	var result []ModelInfo
	ts := c.cachedModels[0].Created
	for alias := range c.cachedAliases {
		result = append(result, ModelInfo{
			ID:       alias,
			Created:  ts,
			OwnedBy:  "google",
			Provider: "gemini",
		})
	}
	return result
}

func (c *Client) ListModelsIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]string, 0, len(c.cachedModels)+len(c.cachedAliases))
	for _, m := range c.cachedModels {
		ids = append(ids, m.ID)
	}
	for alias := range c.cachedAliases {
		ids = append(ids, alias)
	}
	return ids
}

// parseResponse parses Gemini's response format
func (c *Client) parseResponse(text string) (*Response, error) {
	var finalResText string
	var finalMetadata map[string]any
	found := false
	imagesByURL := make(map[string]Image)

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, ")]}'")

		var root []interface{}
		if err := json.Unmarshal([]byte(line), &root); err == nil {
			for _, item := range root {
				itemArray, ok := item.([]interface{})
				if !ok || len(itemArray) < 3 {
					continue
				}

				payloadStr, ok := itemArray[2].(string)
				if !ok {
					continue
				}

				var payload []interface{}
				if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
					continue
				}
				collectImages(payload, imagesByURL)
				if metadata := extractConversationMetadataFromPayload(payload); len(metadata) > 0 {
					finalMetadata = mergeConversationMetadata(finalMetadata, metadata)
				}

				if len(payload) > 4 {
					candidates, ok := payload[4].([]interface{})
					if ok && candidates != nil && len(candidates) > 0 {
						firstCandidate, ok := candidates[0].([]interface{})
						if ok && len(firstCandidate) >= 2 {
							contentParts, ok := firstCandidate[1].([]interface{})
							if ok && len(contentParts) > 0 {
								resText, ok := contentParts[0].(string)
								if ok {
									finalResText = resText
									found = true
								}
							}
						}
					}
				}
			}
		}
	}

	if found || len(imagesByURL) > 0 {
		images := make([]Image, 0, len(imagesByURL))
		for _, image := range imagesByURL {
			images = append(images, image)
		}
		cid, _ := finalMetadata["cid"].(string)
		rid, _ := finalMetadata["rid"].(string)
		return &Response{
			Text:           finalResText,
			Images:         images,
			Metadata:       finalMetadata,
			ConversationID: cid,
			ResponseID:     rid,
		}, nil
	}

	sample := text
	if len(sample) > 500 {
		sample = sample[:500]
	}
	return nil, fmt.Errorf("failed to parse response. Sample: %s", sample)
}

func collectImages(value any, out map[string]Image) {
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			collectImages(item, out)
		}
	case map[string]interface{}:
		for _, item := range v {
			collectImages(item, out)
		}
	case string:
		for _, rawURL := range imageURLRegex.FindAllString(v, -1) {
			imageURL := normalizeImageURL(rawURL)
			if imageURL == "" {
				continue
			}
			if _, exists := out[imageURL]; exists {
				continue
			}
			out[imageURL] = Image{
				URL:      imageURL,
				MimeType: mimeTypeFromImageURL(imageURL),
			}
		}
	}
}

func normalizeImageURL(rawURL string) string {
	cleaned := html.UnescapeString(strings.TrimSpace(rawURL))
	cleaned = strings.TrimRight(cleaned, ".,);]")
	if strings.HasPrefix(cleaned, "//") {
		cleaned = "https:" + cleaned
	}
	lowerCleaned := strings.ToLower(cleaned)
	if strings.HasPrefix(lowerCleaned, "googleusercontent.com/") || strings.HasSuffix(strings.SplitN(lowerCleaned, "/", 2)[0], ".googleusercontent.com") {
		cleaned = "https://" + cleaned
	}
	parsed, err := url.Parse(cleaned)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	if !looksLikeImageURL(parsed) {
		return ""
	}
	if strings.Contains(strings.ToLower(parsed.Hostname()), "googleusercontent.com") {
		parsed.Scheme = "https"
	}
	return parsed.String()
}

func looksLikeImageURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.EscapedPath())

	if strings.HasSuffix(path, ".svg") || strings.Contains(host, "fonts.gstatic.com") {
		return false
	}
	if host == "googleusercontent.com" {
		return false
	}
	if strings.HasSuffix(host, ".googleusercontent.com") {
		return true
	}
	switch {
	case strings.HasSuffix(path, ".png"),
		strings.HasSuffix(path, ".jpg"),
		strings.HasSuffix(path, ".jpeg"),
		strings.HasSuffix(path, ".webp"),
		strings.HasSuffix(path, ".gif"):
		return true
	default:
		return false
	}
}

func mimeTypeFromImageURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	path := strings.ToLower(u.EscapedPath())
	switch {
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".webp"):
		return "image/webp"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	default:
		return ""
	}
}

func (cs *CookieStore) ToHTTPCookies() []*http.Cookie {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	cookies := []*http.Cookie{}
	domain := ".google.com"

	if cs.Secure1PSID != "" {
		cookies = append(cookies, &http.Cookie{
			Name:     "__Secure-1PSID",
			Value:    cleanCookie(cs.Secure1PSID),
			Domain:   domain,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteNoneMode,
		})
	}
	if cs.Secure1PSIDTS != "" {
		cookies = append(cookies, &http.Cookie{
			Name:     "__Secure-1PSIDTS",
			Value:    cleanCookie(cs.Secure1PSIDTS),
			Domain:   domain,
			Path:     "/",
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteNoneMode,
		})
	}
	return cookies
}

func cleanCookie(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"")
	v = strings.Trim(v, "'")
	v = strings.TrimSuffix(v, ";")
	return v
}

// LoadCachedCookies attempts to read the saved 1PSIDTS from disk
func (c *Client) LoadCachedCookies() (string, error) {
	if c.cookies.Secure1PSID == "" {
		return "", errors.New("no PSID available")
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}

	ts := strings.TrimSpace(string(data))
	if ts == "" {
		return "", errors.New("empty cache file")
	}
	return ts, nil
}

// SaveCachedCookies writes the current 1PSIDTS to disk
func (c *Client) SaveCachedCookies() error {
	if c.cookies.Secure1PSID == "" || c.cookies.Secure1PSIDTS == "" {
		return nil
	}

	// Create directory if not exists
	if err := os.MkdirAll(".cookies", 0755); err != nil {
		return err
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	err := os.WriteFile(filename, []byte(c.cookies.Secure1PSIDTS), 0600)
	if err == nil {
		c.log.Debug("Saved __Secure-1PSIDTS to local cache for future use", zap.String("file", filename))
	} else {
		c.log.Warn("Failed to save cookies to cache", zap.String("file", filename), zap.Error(err))
	}
	if c.cookieCache {
		source := strings.TrimSpace(c.cookieSource)
		if source == "" || source == "env" || source == "cache" {
			source = "runtime"
		}
		if cacheErr := saveAccountCookieCache(c.cookieCachePath, c.accountID, c.cookies.Secure1PSID, c.cookies.Secure1PSIDTS, c.proxyURL, source); cacheErr != nil {
			if err == nil {
				err = cacheErr
			}
			c.log.Warn("Failed to save account cookie cache", zap.String("account", c.accountID), zap.String("path", c.cookieCachePath), zap.Error(cacheErr))
		}
	}
	return err
}

// ClearCookieCache deletes the cached cookie file for the current PSID
func (c *Client) ClearCookieCache() error {
	if c.cookies.Secure1PSID == "" {
		return nil
	}

	hash := sha256.Sum256([]byte(c.cookies.Secure1PSID))
	filename := filepath.Join(".cookies", hex.EncodeToString(hash[:])+".txt")

	err := os.Remove(filename)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

const (
	EndpointGoogle        = "https://www.google.com"
	EndpointInit          = "https://gemini.google.com/app"
	EndpointGenerate      = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	EndpointRotateCookies = "https://accounts.google.com/RotateCookies"
	EndpointUpload        = "https://content-push.googleapis.com/upload"
	EndpointBatchExec     = "https://gemini.google.com/_/BardChatUi/data/batchexecute"
)

var DefaultHeaders = map[string]string{
	"Content-Type":  "application/x-www-form-urlencoded;charset=utf-8",
	"Origin":        "https://gemini.google.com",
	"Referer":       "https://gemini.google.com/",
	"User-Agent":    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"X-Same-Domain": "1",
}
