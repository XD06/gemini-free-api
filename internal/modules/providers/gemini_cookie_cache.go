package providers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	for i := range applied {
		entry, ok := cache.Accounts[applied[i].ID]
		if !ok || strings.TrimSpace(entry.Secure1PSID) == "" {
			// Even if no cookies, still apply cached proxy if present.
			if ok && strings.TrimSpace(entry.ProxyURL) != "" {
				applied[i].ProxyURL = entry.ProxyURL
			}
			continue
		}
		applied[i].Secure1PSID = entry.Secure1PSID
		applied[i].Secure1PSIDTS = entry.Secure1PSIDTS
		applied[i].CookieSource = "cache"
		if strings.TrimSpace(entry.ProxyURL) != "" {
			applied[i].ProxyURL = entry.ProxyURL
		}
	}
	return applied
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
