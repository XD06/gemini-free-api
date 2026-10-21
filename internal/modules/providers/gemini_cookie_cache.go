package providers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gemini-free-api/internal/commons/configs"
)

var cookieCacheFileMu sync.Mutex

type cookieCacheFile struct {
	Accounts map[string]cookieCacheEntry `json:"accounts"`
}

type cookieCacheEntry struct {
	Secure1PSID   string    `json:"secure_1psid"`
	Secure1PSIDTS string    `json:"secure_1psidts,omitempty"`
	ProxyURL      string    `json:"proxy_url,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	Source        string    `json:"source,omitempty"`
}

func applyCookieCache(cfg *configs.Config, accounts []configs.GeminiAccountConfig) []configs.GeminiAccountConfig {
	if cfg == nil || !cfg.Gemini.CookieCache || strings.TrimSpace(cfg.Gemini.CookieCachePath) == "" {
		return accounts
	}
	cache, err := readCookieCache(cfg.Gemini.CookieCachePath)
	if err != nil || len(cache.Accounts) == 0 {
		return accounts
	}

	applied := append([]configs.GeminiAccountConfig(nil), accounts...)
	index := make(map[string]int, len(applied))
	for i := range applied {
		index[applied[i].ID] = i
		entry, ok := cache.Accounts[applied[i].ID]
		if !ok {
			continue
		}
		applyCookieCacheEntry(&applied[i], entry)
	}

	// Accounts added from the console exist only in the cache. Without this
	// merge they would vanish on restart unless the same ID was also declared
	// in GEMINI_ACCOUNTS.
	ids := make([]string, 0, len(cache.Accounts))
	for id := range cache.Accounts {
		if _, exists := index[id]; !exists {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		entry := cache.Accounts[id]
		source := strings.ToLower(strings.TrimSpace(entry.Source))
		if !cacheSourceIsAuthoritative(source) || strings.TrimSpace(entry.Secure1PSID) == "" {
			continue
		}
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		if proxyURL == "" {
			proxyURL = strings.TrimSpace(cfg.Server.ProxyURL)
		}
		applied = append(applied, configs.GeminiAccountConfig{
			ID:              id,
			Secure1PSID:     entry.Secure1PSID,
			Secure1PSIDTS:   entry.Secure1PSIDTS,
			CookieSource:    source,
			ProxyURL:        proxyURL,
			Priority:        0,
			StayMinutes:     180,
			RefreshInterval: cfg.Gemini.RefreshInterval,
			MaxRetries:      cfg.Gemini.MaxRetries,
		})
	}
	return applied
}

func applyCookieCacheEntry(dst *configs.GeminiAccountConfig, entry cookieCacheEntry) {
	// Console/worker updates are deliberate runtime operator changes and must
	// survive restart even when .env still contains the original pair.
	cacheSource := strings.ToLower(strings.TrimSpace(entry.Source))
	cacheIsAuthoritative := cacheSourceIsAuthoritative(cacheSource)
	if (cacheIsAuthoritative || strings.TrimSpace(dst.Secure1PSID) == "") && strings.TrimSpace(entry.Secure1PSID) != "" {
		dst.Secure1PSID = entry.Secure1PSID
		dst.Secure1PSIDTS = entry.Secure1PSIDTS
		dst.CookieSource = cacheSource
		if dst.CookieSource == "" {
			dst.CookieSource = "cache"
		}
	}
	if strings.TrimSpace(dst.ProxyURL) == "" && strings.TrimSpace(entry.ProxyURL) != "" {
		dst.ProxyURL = entry.ProxyURL
	}
}

func cacheSourceIsAuthoritative(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "console", "worker", "runtime":
		return true
	default:
		return false
	}
}

func saveAccountCookieCache(path, accountID, secure1PSID, secure1PSIDTS, proxyURL, source string) error {
	path = strings.TrimSpace(path)
	accountID = strings.TrimSpace(accountID)
	secure1PSID = strings.TrimSpace(secure1PSID)
	if path == "" || accountID == "" || secure1PSID == "" {
		return nil
	}

	cookieCacheFileMu.Lock()
	defer cookieCacheFileMu.Unlock()

	cache, err := readCookieCache(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cache.Accounts == nil {
		cache.Accounts = make(map[string]cookieCacheEntry)
	}
	// Preserve existing proxy if not explicitly provided.
	if strings.TrimSpace(proxyURL) == "" {
		if existing, ok := cache.Accounts[accountID]; ok {
			proxyURL = existing.ProxyURL
		}
	}
	cache.Accounts[accountID] = cookieCacheEntry{
		Secure1PSID:   secure1PSID,
		Secure1PSIDTS: secure1PSIDTS,
		ProxyURL:      strings.TrimSpace(proxyURL),
		UpdatedAt:     time.Now(),
		Source:        source,
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

// saveAccountProxyCache updates only the proxy URL for an account in the cookie
// cache file, leaving cookies and other fields untouched.
func saveAccountProxyCache(path, accountID, proxyURL string) error {
	path = strings.TrimSpace(path)
	accountID = strings.TrimSpace(accountID)
	proxyURL = strings.TrimSpace(proxyURL)
	if path == "" || accountID == "" {
		return nil
	}

	cookieCacheFileMu.Lock()
	defer cookieCacheFileMu.Unlock()

	cache, err := readCookieCache(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cache.Accounts == nil {
		cache.Accounts = make(map[string]cookieCacheEntry)
	}
	entry, ok := cache.Accounts[accountID]
	if !ok {
		entry = cookieCacheEntry{}
	}
	entry.ProxyURL = proxyURL
	entry.UpdatedAt = time.Now()
	cache.Accounts[accountID] = entry

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

// removeAccountCookieCache removes an account entry from the cookie cache file.
func removeAccountCookieCache(path, accountID string) error {
	path = strings.TrimSpace(path)
	accountID = strings.TrimSpace(accountID)
	if path == "" || accountID == "" {
		return nil
	}

	cookieCacheFileMu.Lock()
	defer cookieCacheFileMu.Unlock()

	cache, err := readCookieCache(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing to remove
		}
		return err
	}
	if _, ok := cache.Accounts[accountID]; !ok {
		return nil // already absent
	}
	delete(cache.Accounts, accountID)

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0600)
}

func readCookieCache(path string) (cookieCacheFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cookieCacheFile{}, err
	}
	var cache cookieCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		return cookieCacheFile{}, err
	}
	return cache, nil
}

// atomicWriteFile writes data to path atomically by first writing to a
// temporary file in the same directory and then renaming it. This prevents
// truncated/corrupted files if the process is killed mid-write (e.g. Docker
// restart).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
