package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestUpdateAccountCookiesPersistsBeforeBackgroundValidation(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "accounts.json")
	validationStarted := make(chan struct{})
	validationRelease := make(chan struct{})
	client := &Client{
		accountID:       "main",
		cookieSource:    "env",
		cookieCache:     true,
		cookieCachePath: cachePath,
		httpClient:      req.NewClient(),
		cookies:         &CookieStore{Secure1PSID: "old-psid", Secure1PSIDTS: "old-ts"},
		at:              "old-session-token",
		cookieHeader:    "old-cookie-header",
		healthy:         true,
		healthState:     AccountStateHealthy,
		log:             zap.NewNop(),
	}
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
		internalRefresh: func(context.Context, *Client) error {
			close(validationStarted)
			<-validationRelease
			return nil
		},
	}

	started := time.Now()
	if err := pool.UpdateAccountCookies(context.Background(), "main", "new-psid", "new-ts"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("save waited for network validation: %v", elapsed)
	}
	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("background validation did not start")
	}
	status := client.AccountStatus()
	if status.State != AccountStateRefreshing || status.Healthy {
		t.Fatalf("expected saved pair to be pending validation, got %#v", status)
	}
	if client.sessionInitialized() {
		t.Fatal("staging new cookies must clear the previous session token")
	}
	cached, err := readCookieCache(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := cached.Accounts["main"]
	if !ok || entry.Secure1PSID != "new-psid" || entry.Secure1PSIDTS != "new-ts" {
		t.Fatalf("new pair was not persisted before validation: %#v", cached.Accounts)
	}

	close(validationRelease)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if client.AccountStatus().State == AccountStateHealthy && !pool.accountRefreshInProgress("main") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("validation did not complete: %#v", client.AccountStatus())
}

func TestUpdateAccountCookiesKeepsSavedPairWhenValidationFails(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "accounts.json")
	validationDone := make(chan struct{})
	client := &Client{
		accountID:       "main",
		cookieCache:     true,
		cookieCachePath: cachePath,
		httpClient:      req.NewClient(),
		cookies:         &CookieStore{Secure1PSID: "old-psid", Secure1PSIDTS: "old-ts"},
		healthy:         true,
		healthState:     AccountStateHealthy,
		log:             zap.NewNop(),
	}
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
		internalRefresh: func(context.Context, *Client) error {
			defer close(validationDone)
			return errors.New("upstream rejected staged cookies")
		},
	}

	if err := pool.UpdateAccountCookies(context.Background(), "main", "new-psid", "new-ts"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-validationDone:
	case <-time.After(time.Second):
		t.Fatal("background validation did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for pool.accountRefreshInProgress("main") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := client.GetCookies()
	if got.Secure1PSID != "new-psid" || got.Secure1PSIDTS != "new-ts" {
		t.Fatalf("failed validation rolled back the saved pair: %#v", got)
	}
	if status := client.AccountStatus(); status.State == AccountStateHealthy || status.LastError == "" {
		t.Fatalf("expected visible validation failure, got %#v", status)
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

func TestValidateProxyCandidateAcceptsChallengeRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/sorry", http.StatusFound)
	}))
	defer server.Close()

	oldURL := validateProxyURL
	validateProxyURL = server.URL
	defer func() { validateProxyURL = oldURL }()

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if err := validateProxyCandidate(context.Background(), client, ""); err != nil {
		t.Fatalf("challenge redirect should prove proxy reachability: %v", err)
	}
}

func TestAccountTestAndRefreshUseLiveProbeToRestoreHealth(t *testing.T) {
	client := &Client{accountID: "main", at: "session-token", healthy: false, healthState: AccountStateExpired}
	pool := &ClientPool{clients: []*Client{client}, clientsByID: map[string]*Client{"main": client}, refreshing: make(map[string]bool)}
	original := accountLivenessProbe
	accountLivenessProbe = func(context.Context, *Client) (string, error) { return "OK", nil }
	defer func() { accountLivenessProbe = original }()

	text, err := pool.TestAccount(context.Background(), "main")
	if err != nil || text != "OK" {
		t.Fatalf("test result = %q, %v", text, err)
	}
	status := client.AccountStatus()
	if !status.Healthy || status.State != AccountStateHealthy {
		t.Fatalf("test did not restore health: %#v", status)
	}

	client.setAccountState(AccountStateExpired, errors.New("stale bootstrap"))
	if err := pool.RefreshAccount(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	status = client.AccountStatus()
	if !status.Healthy || status.State != AccountStateHealthy {
		t.Fatalf("refresh did not restore health: %#v", status)
	}
}

func TestAccountTestReturnsInitializationReasonWithoutProbe(t *testing.T) {
	client := &Client{accountID: "main"}
	client.setAccountState(AccountStateNeedsManualLogin, errors.New("authentication failed: Cookie pair rejected. __Secure-1PSID must match __Secure-1PSIDTS"))
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
	}
	original := accountLivenessProbe
	probeCalled := false
	accountLivenessProbe = func(context.Context, *Client) (string, error) {
		probeCalled = true
		return "", errors.New("probe should not run")
	}
	defer func() { accountLivenessProbe = original }()

	_, err := pool.TestAccount(context.Background(), "main")
	if probeCalled {
		t.Fatal("uninitialized account test must not run the generation probe")
	}
	var operationErr *AccountOperationError
	if !errors.As(err, &operationErr) {
		t.Fatalf("expected AccountOperationError, got %T: %v", err, err)
	}
	if operationErr.Code != AccountErrorCookiePairMismatch || operationErr.State != AccountStateNeedsManualLogin || operationErr.Retryable {
		t.Fatalf("unexpected operation error: %#v", operationErr)
	}
}

func TestRefreshUninitializedAccountRecoversSessionBeforeProbe(t *testing.T) {
	client := &Client{accountID: "main", healthState: AccountStateUninitialized}
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
	}
	recoveryCalls := 0
	pool.internalRefresh = func(_ context.Context, got *Client) error {
		recoveryCalls++
		got.mu.Lock()
		got.at = "recovered-session"
		got.mu.Unlock()
		return nil
	}
	original := accountLivenessProbe
	probeCalls := 0
	accountLivenessProbe = func(_ context.Context, got *Client) (string, error) {
		probeCalls++
		if !got.sessionInitialized() {
			t.Fatal("probe ran before session recovery")
		}
		return "OK", nil
	}
	defer func() { accountLivenessProbe = original }()

	if err := pool.RefreshAccount(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if recoveryCalls != 1 || probeCalls != 1 {
		t.Fatalf("expected one recovery and one probe, got recovery=%d probe=%d", recoveryCalls, probeCalls)
	}
	if status := client.AccountStatus(); !status.Healthy || status.State != AccountStateHealthy {
		t.Fatalf("unexpected recovered status: %#v", status)
	}
}

func TestRefreshInitializedAccountSkipsSessionRecoveryWhenProbeSucceeds(t *testing.T) {
	client := &Client{accountID: "main", at: "live-session", healthy: true, healthState: AccountStateHealthy}
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
	}
	recoveryCalled := false
	pool.internalRefresh = func(context.Context, *Client) error {
		recoveryCalled = true
		return nil
	}
	original := accountLivenessProbe
	accountLivenessProbe = func(context.Context, *Client) (string, error) { return "OK", nil }
	defer func() { accountLivenessProbe = original }()

	if err := pool.RefreshAccount(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	if recoveryCalled {
		t.Fatal("healthy initialized account should not rotate or refresh its session after a successful probe")
	}
}

func TestConcurrentAccountRefreshesShareOneRecovery(t *testing.T) {
	client := &Client{accountID: "main", healthState: AccountStateUninitialized}
	pool := &ClientPool{
		clients:     []*Client{client},
		clientsByID: map[string]*Client{"main": client},
		refreshing:  make(map[string]bool),
	}
	started := make(chan struct{})
	release := make(chan struct{})
	recoveryCalls := 0
	pool.internalRefresh = func(_ context.Context, got *Client) error {
		recoveryCalls++
		close(started)
		<-release
		got.mu.Lock()
		got.at = "shared-session"
		got.mu.Unlock()
		return nil
	}
	original := accountLivenessProbe
	probeCalls := 0
	accountLivenessProbe = func(context.Context, *Client) (string, error) {
		probeCalls++
		return "OK", nil
	}
	defer func() { accountLivenessProbe = original }()

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { firstDone <- pool.RefreshAccount(context.Background(), "main") }()
	<-started
	go func() { secondDone <- pool.RefreshAccount(context.Background(), "main") }()
	// Give the second call time to observe the in-flight refresh before the
	// first recovery is released.
	time.Sleep(20 * time.Millisecond)
	close(release)

	if err := <-firstDone; err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("joined refresh failed: %v", err)
	}
	if recoveryCalls != 1 || probeCalls != 1 {
		t.Fatalf("expected one shared recovery/probe, got recovery=%d probe=%d", recoveryCalls, probeCalls)
	}
}

func TestTerminalAccountStateIsNeverHealthy(t *testing.T) {
	client := &Client{healthy: true}
	client.setAccountState(AccountStateExpired, errors.New("failed"))
	status := client.AccountStatus()
	if status.Healthy || status.State != AccountStateExpired {
		t.Fatalf("inconsistent terminal status: %#v", status)
	}
}
