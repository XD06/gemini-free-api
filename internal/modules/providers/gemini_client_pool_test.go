package providers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gemini-free-api/internal/commons/configs"
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
