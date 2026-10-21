package providers

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gemini-free-api/internal/commons/configs"
)

func TestApplyCookieCacheFillsMissingAccountCookies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "acc1", "cached-psid", "cached-ts", "", "worker"); err != nil {
		t.Fatal(err)
	}

	cfg := &configs.Config{}
	cfg.Gemini.CookieCache = true
	cfg.Gemini.CookieCachePath = path

	accounts := applyCookieCache(cfg, []configs.GeminiAccountConfig{{ID: "acc1"}})
	if accounts[0].Secure1PSID != "cached-psid" || accounts[0].Secure1PSIDTS != "cached-ts" {
		t.Fatalf("expected cached cookies to be applied, got %#v", accounts[0])
	}
	if accounts[0].CookieSource != "worker" {
		t.Fatalf("expected original cache source, got %q", accounts[0].CookieSource)
	}
}

func TestApplyCookieCacheUsesPersistedOperatorUpdateAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "acc1", "cached-psid", "cached-ts", "", "worker"); err != nil {
		t.Fatal(err)
	}

	cfg := &configs.Config{}
	cfg.Gemini.CookieCache = true
	cfg.Gemini.CookieCachePath = path

	accounts := applyCookieCache(cfg, []configs.GeminiAccountConfig{{
		ID:            "acc1",
		Secure1PSID:   "env-psid",
		Secure1PSIDTS: "env-ts",
		CookieSource:  "env",
	}})
	if accounts[0].Secure1PSID != "cached-psid" || accounts[0].Secure1PSIDTS != "cached-ts" {
		t.Fatalf("persisted worker update must survive restart, got %#v", accounts[0])
	}
	if accounts[0].CookieSource != "worker" {
		t.Fatalf("expected cookie source worker, got %q", accounts[0].CookieSource)
	}
}

func TestApplyCookieCachePreservesEnvAgainstNonAuthoritativeCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "acc1", "cached-psid", "cached-ts", "", "env"); err != nil {
		t.Fatal(err)
	}

	cfg := &configs.Config{}
	cfg.Gemini.CookieCache = true
	cfg.Gemini.CookieCachePath = path
	accounts := applyCookieCache(cfg, []configs.GeminiAccountConfig{{
		ID:            "acc1",
		Secure1PSID:   "env-psid",
		Secure1PSIDTS: "env-ts",
		CookieSource:  "env",
	}})
	if accounts[0].Secure1PSID != "env-psid" || accounts[0].Secure1PSIDTS != "env-ts" {
		t.Fatalf("non-authoritative cache replaced env cookies: %#v", accounts[0])
	}
}

func TestApplyCookieCacheAddsConsoleOnlyAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "console-only", "console-psid", "console-ts", "", "console"); err != nil {
		t.Fatal(err)
	}

	cfg := &configs.Config{}
	cfg.Gemini.CookieCache = true
	cfg.Gemini.CookieCachePath = path
	cfg.Server.ProxyURL = "http://127.0.0.1:10808"

	accounts := applyCookieCache(cfg, nil)
	if len(accounts) != 1 {
		t.Fatalf("expected console-only account to be restored, got %#v", accounts)
	}
	if accounts[0].ID != "console-only" || accounts[0].Secure1PSID != "console-psid" || accounts[0].Secure1PSIDTS != "console-ts" {
		t.Fatalf("unexpected restored account: %#v", accounts[0])
	}
	if accounts[0].CookieSource != "console" {
		t.Fatalf("expected cookie source console, got %q", accounts[0].CookieSource)
	}
	if accounts[0].ProxyURL != cfg.Server.ProxyURL {
		t.Fatalf("expected console account to inherit the global proxy, got %q", accounts[0].ProxyURL)
	}
}

func TestApplyCookieCacheIgnoresNonAuthoritativeCacheOnlyEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "env-only", "env-psid", "env-ts", "", "env"); err != nil {
		t.Fatal(err)
	}

	cfg := &configs.Config{}
	cfg.Gemini.CookieCache = true
	cfg.Gemini.CookieCachePath = path

	if accounts := applyCookieCache(cfg, nil); len(accounts) != 0 {
		t.Fatalf("expected env-only cache entries not to create accounts, got %#v", accounts)
	}
}

func TestSaveAccountCookieCacheWritesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	if err := saveAccountCookieCache(path, "acc1", "psid", "ts", "", "worker"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("expected cache permissions 0600, got %v", info.Mode().Perm())
	}
}
