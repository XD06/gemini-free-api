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
			continue
		}
		applied[i].Secure1PSID = entry.Secure1PSID
		applied[i].Secure1PSIDTS = entry.Secure1PSIDTS
		applied[i].CookieSource = "cache"
	}
	return applied
}

func saveAccountCookieCache(path, accountID, secure1PSID, secure1PSIDTS, source string) error {
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
	cache.Accounts[accountID] = cookieCacheEntry{
		Secure1PSID:   secure1PSID,
		Secure1PSIDTS: secure1PSIDTS,
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
	return os.WriteFile(path, data, 0600)
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
