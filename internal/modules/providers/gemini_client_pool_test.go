package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gemini-free-api/internal/commons/configs"

	"github.com/imroc/req/v3"
	"go.uber.org/zap"
)

func TestClientPoolRotatesOnlyForNewConversations(t *testing.T) {
	now := time.Now()
	main := &Client{accountID: "main", healthy: true}
	backup := &Client{accountID: "backup", healthy: true}
	pool := &ClientPool{
		clients:        []*Client{main, backup},
		clientsByID:    map[string]*Client{"main": main, "backup": backup},
		stayByID:       map[string]time.Duration{"main": time.Hour, "backup": time.Hour},
		conversationTo: make(map[string]string),
		activeIndex:    -1,
	}

	active, err := pool.selectActiveLocked(now, false)
	if err != nil {
		t.Fatal(err)
	}
	if active.accountID != "main" {
		t.Fatalf("expected first active account main, got %q", active.accountID)
	}

	sticky, err := pool.clientForOptions(context.Background(), WithConversationID("thread-1"))
	if err != nil {
		t.Fatal(err)
	}
	if sticky.accountID != "main" {
		t.Fatalf("expected thread-1 to bind to main, got %q", sticky.accountID)
	}

	rotated, err := pool.selectActiveLocked(now.Add(2*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.accountID != "backup" {
		t.Fatalf("expected rotation to backup, got %q", rotated.accountID)
	}

	stickyAgain, err := pool.clientForOptions(context.Background(), WithConversationID("thread-1"))
	if err != nil {
		t.Fatal(err)
	}
	if stickyAgain.accountID != "main" {
		t.Fatalf("expected existing thread to stay on main, got %q", stickyAgain.accountID)
	}

	newThread, err := pool.clientForOptions(context.Background(), WithConversationID("thread-2"))
	if err != nil {
		t.Fatal(err)
	}
	if newThread.accountID != "backup" {
		t.Fatalf("expected new thread to use current active backup, got %q", newThread.accountID)
	}
}

func TestClientPoolSkipsUnhealthyActiveAccount(t *testing.T) {
	main := &Client{accountID: "main", healthy: false}
	backup := &Client{accountID: "backup", healthy: true}
	pool := &ClientPool{
		clients:        []*Client{main, backup},
		clientsByID:    map[string]*Client{"main": main, "backup": backup},
		stayByID:       map[string]time.Duration{"main": time.Hour, "backup": time.Hour},
		conversationTo: make(map[string]string),
		activeIndex:    -1,
	}

	selected, err := pool.clientForOptions(context.Background(), WithConversationID("thread-1"))
	if err != nil {
		t.Fatal(err)
	}
	if selected.accountID != "backup" {
		t.Fatalf("expected unhealthy main to be skipped, got %q", selected.accountID)
	}
}

func TestOrderedAccountsPrefersHigherPriority(t *testing.T) {
	accounts := orderedAccounts([]configs.GeminiAccountConfig{
		{ID: "low", Priority: 10},
		{ID: "high", Priority: 100},
		{ID: "middle", Priority: 50},
	})

	got := []string{accounts[0].ID, accounts[1].ID, accounts[2].ID}
	want := []string{"high", "middle", "low"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected order %v, got %v", want, got)
		}
	}
}

func TestAccountStayDurationUsesPriorityMultiplier(t *testing.T) {
	stay := accountStayDuration(configs.GeminiAccountConfig{
		ID:          "main",
		Priority:    3,
		StayMinutes: 60,
	})

	if stay != 3*time.Hour {
		t.Fatalf("expected priority 3 with base 60m to stay 3h, got %v", stay)
	}
}

func TestAccountStayDurationUsesBaseWhenPriorityMissing(t *testing.T) {
	stay := accountStayDuration(configs.GeminiAccountConfig{
		ID:          "main",
		StayMinutes: 60,
	})

	if stay != time.Hour {
		t.Fatalf("expected no priority to stay base 1h, got %v", stay)
	}
}

func TestClientPoolRestoresUnexpiredActiveAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	if err := writeAccountPoolState(path, accountPoolState{
		ActiveAccountID: "backup",
		ActiveUntil:     now.Add(time.Hour),
		UpdatedAt:       now,
	}); err != nil {
		t.Fatal(err)
	}

	main := &Client{accountID: "main", healthy: true}
	backup := &Client{accountID: "backup", healthy: true}
	pool := &ClientPool{
		clients:     []*Client{main, backup},
		clientsByID: map[string]*Client{"main": main, "backup": backup},
		activeIndex: -1,
		statePath:   path,
	}

	if !pool.restoreActiveLocked(now) {
		t.Fatal("expected active account to be restored")
	}
	if pool.activeIndex != 1 || !pool.activeUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("unexpected restored state: index=%d until=%v", pool.activeIndex, pool.activeUntil)
	}
}

func TestClientPoolDoesNotRestoreExpiredActiveAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	now := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	if err := writeAccountPoolState(path, accountPoolState{
		ActiveAccountID: "backup",
		ActiveUntil:     now.Add(-time.Minute),
		UpdatedAt:       now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	backup := &Client{accountID: "backup", healthy: true}
	pool := &ClientPool{
		clients:     []*Client{backup},
		clientsByID: map[string]*Client{"backup": backup},
		activeIndex: -1,
		statePath:   path,
	}

	if pool.restoreActiveLocked(now) {
		t.Fatal("expected expired active account not to be restored")
	}
}

func TestClientPoolRecoversNoHealthyAccountWithExternalRefresh(t *testing.T) {
	main := &Client{accountID: "main", healthy: false}
	pool := &ClientPool{
		clients:        []*Client{main},
		clientsByID:    map[string]*Client{"main": main},
		stayByID:       map[string]time.Duration{"main": time.Hour},
		conversationTo: make(map[string]string),
		refreshing:     make(map[string]bool),
		activeIndex:    -1,
	}
	called := false
	pool.externalRefresh = func(ctx context.Context, accountID string) error {
		called = true
		if accountID != "main" {
			t.Fatalf("expected main to be refreshed, got %q", accountID)
		}
		main.mu.Lock()
		main.healthy = true
		main.mu.Unlock()
		return nil
	}

	selected, err := pool.clientForOptions(context.Background(), WithConversationID("thread-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected external refresh to be called")
	}
	if selected.accountID != "main" {
		t.Fatalf("expected recovered main account, got %q", selected.accountID)
	}
}

func TestBackgroundRefreshDoesNotMarkExpiredAccountHealthyWithoutExternalCookies(t *testing.T) {
	main := &Client{accountID: "main", healthy: false}
	main.setAccountState(AccountStateExpired, errors.New("BardErrorInfo [1060]"))
	pool := &ClientPool{
		clients:        []*Client{main},
		clientsByID:    map[string]*Client{"main": main},
		stayByID:       map[string]time.Duration{"main": time.Hour},
		conversationTo: make(map[string]string),
		refreshing:     make(map[string]bool),
		activeIndex:    -1,
		internalRefresh: func(context.Context, *Client) error {
			return nil
		},
	}

	pool.refreshClientAsync(main)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := main.AccountStatus()
		if status.State == AccountStateNeedsManualLogin {
			if main.IsHealthy() {
				t.Fatal("expired account must not become healthy after internal refresh without external cookies")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected account to require external cookies, got %#v", main.AccountStatus())
}

func TestBoundConversationMovesOffUnhealthyAccount(t *testing.T) {
	main := &Client{accountID: "main", healthy: false}
	backup := &Client{accountID: "backup", healthy: true}
	pool := &ClientPool{
		clients:          []*Client{main, backup},
		clientsByID:      map[string]*Client{"main": main, "backup": backup},
		stayByID:         map[string]time.Duration{"main": time.Hour, "backup": time.Hour},
		conversationTo:   map[string]string{"thread": "main"},
		conversationSeen: map[string]time.Time{"thread": time.Now()},
		activeIndex:      0,
		activeUntil:      time.Now().Add(time.Hour),
	}

	selected, err := pool.clientForOptions(context.Background(), WithConversationID("thread"))
	if err != nil {
		t.Fatal(err)
	}
	if selected != backup {
		t.Fatalf("expected unhealthy binding to move to backup, got %q", selected.accountID)
	}
	if got := pool.conversationTo["thread"]; got != "backup" {
		t.Fatalf("expected binding updated to backup, got %q", got)
	}
}

func TestUpdateAccountCookiesKeepsOldCookiesWhenValidationFails(t *testing.T) {
	oldPSID, oldTS := "old-psid", "old-ts"
	client := &Client{
		accountID:  "main",
		cookies:    &CookieStore{Secure1PSID: oldPSID, Secure1PSIDTS: oldTS},
		httpClient: req.NewClient().SetTimeout(time.Second),
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("no session token")), Request: req}, nil
		})},
		log: zap.NewNop(),
	}
	pool := &ClientPool{clients: []*Client{client}, clientsByID: map[string]*Client{"main": client}}

	err := pool.UpdateAccountCookies(context.Background(), "main", "bad-psid", "bad-ts")
	if err == nil {
		t.Fatal("expected invalid cookies to fail validation")
	}
	got := client.GetCookies()
	if got.Secure1PSID != oldPSID || got.Secure1PSIDTS != oldTS {
		t.Fatalf("expected old cookies preserved, got %#v", got)
	}
}

func TestHasConversationStateDoesNotRebindUnhealthyOwner(t *testing.T) {
	main := &Client{
		accountID:     "main",
		healthy:       false,
		conversations: map[string]*SessionMetadata{"thread": {ConversationID: "provider-thread"}},
	}
	pool := &ClientPool{
		clients:          []*Client{main},
		clientsByID:      map[string]*Client{"main": main},
		conversationTo:   map[string]string{"thread": "main"},
		conversationSeen: map[string]time.Time{"thread": time.Now()},
	}
	if pool.HasConversationState("thread") {
		t.Fatal("expected unhealthy conversation owner to be unavailable")
	}
	if _, ok := pool.conversationTo["thread"]; ok {
		t.Fatal("expected unhealthy conversation binding to remain removed")
	}
}

func TestNewConversationDoesNotCountAsExistingProviderState(t *testing.T) {
	client := &Client{conversations: make(map[string]*SessionMetadata)}
	if hasConversationStateOption(client, WithConversationID("new-thread")) {
		t.Fatal("expected a new conversation id to remain eligible for failover")
	}
	client.conversations["existing-thread"] = &SessionMetadata{ConversationID: "provider-thread"}
	if !hasConversationStateOption(client, WithConversationID("existing-thread")) {
		t.Fatal("expected established provider state to require history-aware fallback")
	}
}

func TestUpdateAccountProxyDoesNotCommitFailedCandidate(t *testing.T) {
	oldRaw := &http.Client{}
	client := &Client{accountID: "main", proxyURL: "http://old", cookies: &CookieStore{Secure1PSID: "cookie"}, httpClient: req.NewClient(), rawHTTPClient: oldRaw}
	pool := &ClientPool{clients: []*Client{client}, clientsByID: map[string]*Client{"main": client}, cfg: &configs.Config{}}
	original := validateProxyClient
	validateProxyClient = func(context.Context, *http.Client, string) error { return errors.New("unreachable") }
	defer func() { validateProxyClient = original }()

	if err := pool.UpdateAccountProxy(context.Background(), "main", "http://new"); err == nil {
		t.Fatal("expected validation error")
	}
	client.mu.RLock()
	defer client.mu.RUnlock()
	if client.proxyURL != "http://old" || client.rawHTTPClient != oldRaw {
		t.Fatalf("failed candidate was committed: proxy=%q", client.proxyURL)
	}
}

func TestUpdateAccountProxyCopiesCookiesToCandidate(t *testing.T) {
	client := &Client{accountID: "main", cookies: &CookieStore{Secure1PSID: "cookie", Secure1PSIDTS: "ts"}, httpClient: req.NewClient(), rawHTTPClient: &http.Client{}}
	pool := &ClientPool{clients: []*Client{client}, clientsByID: map[string]*Client{"main": client}, cfg: &configs.Config{}}
	original := validateProxyClient
	var gotCookieHeader string
	validateProxyClient = func(_ context.Context, _ *http.Client, header string) error { gotCookieHeader = header; return nil }
	defer func() { validateProxyClient = original }()

	if err := pool.UpdateAccountProxy(context.Background(), "main", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotCookieHeader, "__Secure-1PSID=cookie") || !strings.Contains(gotCookieHeader, "__Secure-1PSIDTS=ts") {
		t.Fatalf("expected current auth cookies copied to candidate validation, got %q", gotCookieHeader)
	}
}
