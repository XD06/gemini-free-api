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
