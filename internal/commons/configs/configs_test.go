package configs

import "testing"

func TestLoadGeminiAccountsFromEnvironment(t *testing.T) {
	t.Setenv("GEMINI_ACCOUNTS", "main, backup-1")
	t.Setenv("GEMINI_ACCOUNT_MAIN_1PSID", "main-psid")
	t.Setenv("GEMINI_ACCOUNT_MAIN_1PSIDTS", "main-ts")
	t.Setenv("GEMINI_ACCOUNT_MAIN_PROXY", "socks5h://127.0.0.1:1080")
	t.Setenv("GEMINI_ACCOUNT_MAIN_PRIORITY", "100")
	t.Setenv("GEMINI_ACCOUNT_MAIN_STAY_MINUTES", "720")
	t.Setenv("GEMINI_ACCOUNT_BACKUP_1_1PSID", "backup-psid")
	t.Setenv("GEMINI_ACCOUNT_BACKUP_1_STAY_MINUTES", "180")

	cfg := Config{}
	cfg.Gemini.RefreshInterval = 2
	cfg.Gemini.MaxRetries = 3

	accounts := loadGeminiAccounts(cfg)
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].ID != "main" || accounts[0].Secure1PSID != "main-psid" || accounts[0].Secure1PSIDTS != "main-ts" {
		t.Fatalf("unexpected main account: %#v", accounts[0])
	}
	if accounts[0].ProxyURL != "socks5h://127.0.0.1:1080" {
		t.Fatalf("expected main proxy to be parsed, got %q", accounts[0].ProxyURL)
	}
	if accounts[0].StayMinutes != 720 {
		t.Fatalf("expected main stay minutes 720, got %d", accounts[0].StayMinutes)
	}
	if accounts[0].Priority != 100 {
		t.Fatalf("expected main priority 100, got %d", accounts[0].Priority)
	}
	if accounts[1].ID != "backup-1" || accounts[1].Secure1PSID != "backup-psid" {
		t.Fatalf("unexpected backup account: %#v", accounts[1])
	}
}

func TestLoadGeminiAccountsFallsBackToLegacyConfig(t *testing.T) {
	cfg := Config{}
	cfg.Server.ProxyURL = "http://127.0.0.1:7890"
	cfg.Gemini.Secure1PSID = "legacy-psid"
	cfg.Gemini.Secure1PSIDTS = "legacy-ts"
	cfg.Gemini.RefreshInterval = 2
	cfg.Gemini.MaxRetries = 3

	accounts := loadGeminiAccounts(cfg)
	if len(accounts) != 1 {
		t.Fatalf("expected 1 default account, got %d", len(accounts))
	}
	if accounts[0].ID != "default" || accounts[0].Secure1PSID != "legacy-psid" || accounts[0].ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("unexpected default account: %#v", accounts[0])
	}
}

func TestValidateAllowsNamedAccountsWithoutInitialCookies(t *testing.T) {
	t.Setenv("GEMINI_ACCOUNTS", "acc1,acc2")
	cfg := Config{
		Server: ServerConfig{Port: "8787"},
		Gemini: GeminiConfig{
			Accounts: []GeminiAccountConfig{
				{ID: "acc1"},
				{ID: "acc2"},
			},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected named accounts without cookies to validate, got %v", err)
	}
}

func TestNewLoadsStartupCookieRotateFlag(t *testing.T) {
	t.Setenv("PORT", "8787")
	t.Setenv("GEMINI_ACCOUNTS", "acc1")
	t.Setenv("GEMINI_STARTUP_COOKIE_ROTATE", "false")

	cfg, err := New()
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if cfg.Gemini.StartupCookieRotate {
		t.Fatal("expected startup cookie rotation to be disabled")
	}
}
