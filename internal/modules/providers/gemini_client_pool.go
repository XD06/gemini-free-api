package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"gemini-free-api/internal/commons/configs"

	"github.com/imroc/req/v3"
	"go.uber.org/zap"
)

type GeminiClient interface {
	Provider
	GenerateContentStreamForOpenAI(ctx context.Context, prompt string, onEvent func(event StreamEvent) bool, options ...GenerateOption) error
	HasConversationState(id string) bool
	IsConversationUntrusted(id string) bool
}

type AccountManager interface {
	ListAccountStatuses() []AccountStatus
	UpdateAccountCookies(ctx context.Context, accountID, secure1PSID, secure1PSIDTS string) error
	UpdateAccountProxy(ctx context.Context, accountID, proxyURL string) error
	RefreshAccount(ctx context.Context, accountID string) error
	AddAccount(ctx context.Context, accountID, secure1PSID, secure1PSIDTS, proxyURL string) error
	RemoveAccount(ctx context.Context, accountID string) error
	TestAccount(ctx context.Context, accountID string) (string, error)
}

type ClientPool struct {
	mu              sync.Mutex
	clients         []*Client
	clientsByID     map[string]*Client
	stayByID        map[string]time.Duration
	conversationTo  map[string]string
	refreshing      map[string]bool
	externalRefresh cookieRefreshFunc
	internalRefresh func(*Client) error
	activeIndex     int
	activeUntil     time.Time
	statePath       string
	cfg             *configs.Config
	log             *zap.Logger
}

type cookieRefreshFunc func(ctx context.Context, accountID string) error

func NewGeminiClient(cfg *configs.Config, log *zap.Logger) GeminiClient {
	return NewClientPool(cfg, log)
}

func NewClientPool(cfg *configs.Config, log *zap.Logger) *ClientPool {
	accounts := cfg.Gemini.Accounts
	if len(accounts) == 0 {
		accounts = []configs.GeminiAccountConfig{defaultGeminiAccountConfig(cfg)}
	}
	accounts = applyCookieCache(cfg, accounts)
	accounts = orderedAccounts(accounts)

	pool := &ClientPool{
		clients:         make([]*Client, 0, len(accounts)),
		clientsByID:     make(map[string]*Client),
		stayByID:        make(map[string]time.Duration),
		conversationTo:  make(map[string]string),
		refreshing:      make(map[string]bool),
		externalRefresh: newExternalCookieRefresher(cfg, log),
		internalRefresh: refreshClientSessionInPlace,
		activeIndex:     -1,
		statePath:       cfg.Gemini.AccountStatePath,
		cfg:             cfg,
		log:             log,
	}
	for _, account := range accounts {
		client := NewClientForAccount(cfg, account, log)
		pool.clients = append(pool.clients, client)
		pool.clientsByID[account.ID] = client
		stay := accountStayDuration(account)
		pool.stayByID[account.ID] = stay
	}
	return pool
}

func accountStayDuration(account configs.GeminiAccountConfig) time.Duration {
	baseMinutes := account.StayMinutes
	if baseMinutes <= 0 {
		baseMinutes = 180
	}
	multiplier := account.Priority
	if multiplier <= 0 {
		multiplier = 1
	}
	return time.Duration(baseMinutes*multiplier) * time.Minute
}

func orderedAccounts(accounts []configs.GeminiAccountConfig) []configs.GeminiAccountConfig {
	ordered := append([]configs.GeminiAccountConfig(nil), accounts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority > ordered[j].Priority
	})
	return ordered
}

func (p *ClientPool) Init(ctx context.Context) error {
	var lastErr error
	healthy := 0
	for _, client := range p.clients {
		if err := client.Init(ctx); err != nil {
			lastErr = err
			if p.log != nil {
				p.log.Error("Gemini account initialization failed",
					zap.String("account", client.accountID),
					zap.Error(err),
				)
			}
			continue
		}
		healthy++
	}
	if healthy == 0 {
		p.refreshInvalidClientsAsync("init_no_healthy")
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("no Gemini accounts configured")
	}
	p.mu.Lock()
	p.activeIndex = -1
	if !p.restoreActiveLocked(time.Now()) {
		_, _ = p.selectActiveLocked(time.Now(), true)
	}
	p.mu.Unlock()
	return nil
}

func (p *ClientPool) GenerateContent(ctx context.Context, prompt string, options ...GenerateOption) (*Response, error) {
	client, err := p.clientForOptions(ctx, options...)
	if err != nil {
		return nil, err
	}
	response, err := client.GenerateContent(ctx, prompt, options...)
	if err != nil {
		p.markClientError(client, err)
	}
	return response, err
}

func (p *ClientPool) GenerateContentStreamForOpenAI(ctx context.Context, prompt string, onEvent func(event StreamEvent) bool, options ...GenerateOption) error {
	client, err := p.clientForOptions(ctx, options...)
	if err != nil {
		return err
	}
	err = client.GenerateContentStreamForOpenAI(ctx, prompt, onEvent, options...)
	if err != nil {
		p.markClientError(client, err)
	}
	return err
}

func (p *ClientPool) StartChat(options ...ChatOption) ChatSession {
	client, err := p.clientForOptions(context.Background())
	if err != nil {
		return &errorChatSession{err: err}
	}
	return client.StartChat(options...)
}

func (p *ClientPool) Close() error {
	for _, client := range p.clients {
		if err := client.Close(); err != nil && p.log != nil {
			p.log.Warn("failed to close Gemini account", zap.String("account", client.accountID), zap.Error(err))
		}
	}
	return nil
}

func (p *ClientPool) GetName() string {
	return "gemini"
}

func (p *ClientPool) IsHealthy() bool {
	for _, client := range p.clients {
		if client.IsHealthy() {
			return true
		}
	}
	return false
}

func (p *ClientPool) ListAccountStatuses() []AccountStatus {
	p.mu.Lock()
	activeID := ""
	activeUntil := p.activeUntil
	if p.activeIndex >= 0 && p.activeIndex < len(p.clients) {
		activeID = p.clients[p.activeIndex].accountID
	}
	clients := append([]*Client(nil), p.clients...)
	p.mu.Unlock()

	statuses := make([]AccountStatus, 0, len(clients))
	for _, client := range clients {
		status := client.AccountStatus()
		if status.ID == activeID {
			status.Active = true
			status.ActiveUntil = activeUntil
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (p *ClientPool) UpdateAccountCookies(ctx context.Context, accountID, secure1PSID, secure1PSIDTS string) error {
	client := p.clientByID(accountID)
	if client == nil {
		return fmt.Errorf("Gemini account %q not found", accountID)
	}
	return client.UpdateCookies(ctx, secure1PSID, secure1PSIDTS)
}

func (p *ClientPool) RefreshAccount(ctx context.Context, accountID string) error {
	client := p.clientByID(accountID)
	if client == nil {
		return fmt.Errorf("Gemini account %q not found", accountID)
	}
	_ = ctx
	client.setAccountState(AccountStateRefreshing, nil)
	if err := client.RotateCookies(); err != nil {
		client.setAccountState(AccountStateExpired, err)
		return err
	}
	if err := client.refreshSessionToken(); err != nil {
		client.setAccountState(AccountStateExpired, err)
		return err
	}
	client.setAccountState(AccountStateHealthy, nil)
	client.mu.Lock()
	client.healthy = true
	client.mu.Unlock()
	return nil
}

func (p *ClientPool) ListModels() []ModelInfo {
	client, err := p.clientForOptions(context.Background())
	if err == nil {
		if models := client.ListModels(); len(models) > 0 {
			return models
		}
	}

	seen := make(map[string]ModelInfo)
	for _, c := range p.clients {
		for _, model := range c.ListModels() {
			seen[model.ID] = model
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	models := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, seen[id])
	}
	return models
}

func (p *ClientPool) HasConversationState(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	p.mu.Lock()
	accountID := p.conversationTo[id]
	p.mu.Unlock()
	if accountID != "" {
		if client := p.clientsByID[accountID]; client != nil {
			return client.HasConversationState(id)
		}
		return false
	}
	for _, client := range p.clients {
		if client.HasConversationState(id) {
			p.bindConversation(id, client.accountID)
			return true
		}
	}
	return false
}

// IsConversationUntrusted delegates to the account client that owns the
// conversation id. It mirrors HasConversationState's account lookup so the
// OpenAI layer gets the untrusted flag for the bound account.
func (p *ClientPool) IsConversationUntrusted(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	p.mu.Lock()
	accountID := p.conversationTo[id]
	p.mu.Unlock()
	if accountID != "" {
		if client := p.clientsByID[accountID]; client != nil {
			return client.IsConversationUntrusted(id)
		}
		return false
	}
	for _, client := range p.clients {
		if client.HasConversationState(id) {
			p.bindConversation(id, client.accountID)
			return client.IsConversationUntrusted(id)
		}
	}
	return false
}

func (p *ClientPool) clientByID(accountID string) *Client {
	accountID = strings.TrimSpace(accountID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if accountID == "" && len(p.clients) == 1 {
		return p.clients[0]
	}
	return p.clientsByID[accountID]
}

func (p *ClientPool) markClientError(client *Client, err error) {
	if client == nil || err == nil {
		return
	}
	if !isAuthOrSessionError(err) {
		return
	}
	client.mu.Lock()
	client.healthy = false
	client.mu.Unlock()
	client.setAccountState(AccountStateExpired, err)
	if p.log != nil {
		p.log.Warn("Gemini account marked unhealthy after request error",
			zap.String("account", client.accountID),
			zap.Error(err),
		)
		p.logAccountAudit("account_marked_unhealthy", client, "", "auth_or_session_error")
	}
	p.refreshClientAsync(client)
}

func (p *ClientPool) refreshClientAsync(client *Client) {
	if client == nil {
		return
	}
	requiresExternalCookies := clientRequiresExternalCookieRefresh(client)
	p.mu.Lock()
	if p.refreshing == nil {
		p.refreshing = make(map[string]bool)
	}
	if p.refreshing[client.accountID] {
		p.mu.Unlock()
		return
	}
	p.refreshing[client.accountID] = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.refreshing, client.accountID)
			p.mu.Unlock()
		}()

		client.setAccountState(AccountStateRefreshing, nil)
		if p.log != nil {
			p.logAccountAudit("background_refresh_started", client, "", "request_error_recovery")
		}
		refresh := p.internalRefresh
		if refresh == nil {
			refresh = refreshClientSessionInPlace
		}
		if err := refresh(client); err != nil {
			client.setAccountState(AccountStateExpired, err)
			if p.log != nil {
				p.log.Warn("background Gemini account refresh failed",
					zap.String("account", client.accountID),
					zap.Error(err),
				)
				p.logAccountAudit("background_refresh_failed", client, "", "session_refresh_failed")
			}
			return
		}
		if requiresExternalCookies {
			client.mu.Lock()
			client.healthy = false
			client.mu.Unlock()
			client.setAccountState(AccountStateNeedsManualLogin, fmt.Errorf("external cookie sync required after auth/session error"))
			if p.log != nil {
				p.log.Warn("background Gemini account refresh did not mark account healthy; waiting for external cookie sync",
					zap.String("account", client.accountID),
				)
				p.logAccountAudit("background_refresh_external_required", client, "", "request_error_recovery")
			}
			return
		}
		client.mu.Lock()
		client.healthy = true
		client.mu.Unlock()
		client.setAccountState(AccountStateHealthy, nil)
		if p.log != nil {
			p.log.Info("background Gemini account refresh succeeded", zap.String("account", client.accountID))
			p.logAccountAudit("background_refresh_succeeded", client, "", "request_error_recovery")
		}
	}()
}

func refreshClientSessionInPlace(client *Client) error {
	if client == nil {
		return fmt.Errorf("nil Gemini account")
	}
	if err := client.RotateCookies(); err != nil {
		return err
	}
	return client.refreshSessionToken()
}

func clientRequiresExternalCookieRefresh(client *Client) bool {
	if client == nil {
		return false
	}
	status := client.AccountStatus()
	return status.State == AccountStateExpired || status.State == AccountStateNeedsManualLogin
}

func isAuthOrSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, token := range []string{
		"snlm0e",
		"authentication failed",
		"cookies invalid",
		"status 401",
		"status 403",
		"barderrorinfo",
		"no healthy gemini accounts",
	} {
		if strings.Contains(msg, strings.ToLower(token)) {
			return true
		}
	}
	return false
}

func (p *ClientPool) clientForOptions(ctx context.Context, options ...GenerateOption) (*Client, error) {
	config := &GenerateConfig{}
	for _, opt := range options {
		opt(config)
	}

	p.mu.Lock()

	if config.ConversationID != "" {
		if accountID := p.conversationTo[config.ConversationID]; accountID != "" {
			client := p.clientsByID[accountID]
			if client == nil {
				p.mu.Unlock()
				return nil, fmt.Errorf("Gemini conversation %q is bound to missing account %q", config.ConversationID, accountID)
			}
			p.logAccountAudit("conversation_reused", client, config.ConversationID, "bound_conversation")
			p.mu.Unlock()
			return client, nil
		}
	}

	client, err := p.selectActiveLocked(time.Now(), false)
	if err != nil && isNoHealthyAccountsError(err) {
		p.mu.Unlock()
		if refreshErr := p.recoverNoHealthyAccount(ctx, "select_active_no_healthy"); refreshErr != nil && p.log != nil {
			p.log.Warn("Gemini external cookie refresh did not recover a healthy account", zap.Error(refreshErr))
		}
		p.mu.Lock()
		client, err = p.selectActiveLocked(time.Now(), false)
	}
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if config.ConversationID != "" {
		p.conversationTo[config.ConversationID] = client.accountID
		p.logAccountAudit("conversation_bound", client, config.ConversationID, "new_conversation")
	} else if p.log != nil {
		p.logAccountAudit("active_account_used", client, "", "stateless_request")
	}
	p.mu.Unlock()
	return client, nil
}

func (p *ClientPool) selectActiveLocked(now time.Time, force bool) (*Client, error) {
	if len(p.clients) == 0 {
		return nil, fmt.Errorf("no Gemini accounts configured")
	}
	if !force && p.activeIndex >= 0 && p.activeIndex < len(p.clients) && now.Before(p.activeUntil) {
		active := p.clients[p.activeIndex]
		if active.IsHealthy() {
			return active, nil
		}
	}

	start := p.activeIndex
	for i := 0; i < len(p.clients); i++ {
		idx := (start + 1 + i + len(p.clients)) % len(p.clients)
		client := p.clients[idx]
		if !client.IsHealthy() {
			continue
		}
		p.activeIndex = idx
		stay := p.stayByID[client.accountID]
		if stay <= 0 {
			stay = 3 * time.Hour
		}
		p.activeUntil = now.Add(stay)
		p.saveActiveStateLocked()
		if p.log != nil {
			p.log.Info("Gemini active account selected",
				zap.String("account", client.accountID),
				zap.Duration("stay", stay),
				zap.Time("until", p.activeUntil),
			)
			p.logAccountAudit("active_account_selected", client, "", "rotation_window")
		}
		return client, nil
	}
	return nil, fmt.Errorf("no healthy Gemini accounts available")
}

func isNoHealthyAccountsError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no healthy gemini accounts")
}

func (p *ClientPool) recoverNoHealthyAccount(ctx context.Context, reason string) error {
	if p == nil {
		return fmt.Errorf("nil Gemini client pool")
	}
	client := p.highestPriorityClient()
	if client == nil {
		return fmt.Errorf("no Gemini accounts configured")
	}
	if p.log != nil {
		p.logAccountAudit("no_healthy_accounts", client, "", reason)
	}
	if err := p.refreshClientExternal(ctx, client, reason); err != nil {
		return err
	}
	if !client.IsHealthy() {
		return fmt.Errorf("external cookie refresh completed but account %q is still unhealthy", client.accountID)
	}
	return nil
}

func (p *ClientPool) highestPriorityClient() *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.clients) == 0 {
		return nil
	}
	return p.clients[0]
}

func (p *ClientPool) refreshInvalidClientsAsync(reason string) {
	if p == nil || p.externalRefresh == nil {
		if p != nil && p.log != nil {
			p.log.Info("Gemini external cookie refresh skipped",
				zap.String("reason", reason),
				zap.String("detail", "GEMINI_COOKIE_WORKER_ENABLED is false or worker command is unavailable"),
			)
		}
		return
	}
	clients := append([]*Client(nil), p.clients...)
	go func() {
		time.Sleep(2 * time.Second)
		for _, client := range clients {
			if client == nil || client.IsHealthy() {
				continue
			}
			for attempt := 1; attempt <= 3 && !client.IsHealthy(); attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), p.externalRefreshTimeout())
				err := p.refreshClientExternal(ctx, client, reason)
				cancel()
				if err == nil {
					break
				}
				if p.log != nil {
					p.log.Warn("background external Gemini cookie refresh failed",
						zap.String("account", client.accountID),
						zap.Int("attempt", attempt),
						zap.Error(err),
					)
				}
				time.Sleep(time.Duration(attempt*5) * time.Second)
			}
		}
	}()
}

func (p *ClientPool) refreshClientExternal(ctx context.Context, client *Client, reason string) error {
	if client == nil {
		return fmt.Errorf("nil Gemini account")
	}
	if p.externalRefresh == nil {
		return fmt.Errorf("external cookie worker is not configured")
	}
	p.mu.Lock()
	if p.refreshing == nil {
		p.refreshing = make(map[string]bool)
	}
	if p.refreshing[client.accountID] {
		p.mu.Unlock()
		return p.waitForClientRefresh(ctx, client)
	}
	p.refreshing[client.accountID] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.refreshing, client.accountID)
		p.mu.Unlock()
	}()

	client.setAccountState(AccountStateRefreshing, nil)
	if p.log != nil {
		p.logAccountAudit("external_cookie_refresh_started", client, "", reason)
	}
	err := p.externalRefresh(ctx, client.accountID)
	if err != nil {
		client.setAccountState(AccountStateExpired, err)
		if p.log != nil {
			p.logAccountAudit("external_cookie_refresh_failed", client, "", reason)
		}
		return err
	}
	if client.IsHealthy() {
		client.setAccountState(AccountStateHealthy, nil)
	}
	if p.log != nil {
		p.logAccountAudit("external_cookie_refresh_succeeded", client, "", reason)
	}
	return nil
}

func (p *ClientPool) waitForClientRefresh(ctx context.Context, client *Client) error {
	if client == nil {
		return fmt.Errorf("nil Gemini account")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if client.IsHealthy() {
			return nil
		}
		p.mu.Lock()
		stillRefreshing := p.refreshing[client.accountID]
		p.mu.Unlock()
		if !stillRefreshing {
			if client.IsHealthy() {
				return nil
			}
			return fmt.Errorf("external cookie refresh finished but account %q is still unhealthy", client.accountID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *ClientPool) externalRefreshTimeout() time.Duration {
	return 120 * time.Second
}

func newExternalCookieRefresher(cfg *configs.Config, log *zap.Logger) cookieRefreshFunc {
	if cfg == nil || !cfg.Gemini.CookieWorkerEnabled {
		return nil
	}
	command := strings.TrimSpace(cfg.Gemini.CookieWorkerCommand)
	if command == "" {
		return nil
	}
	workerDir := strings.TrimSpace(cfg.Gemini.CookieWorkerDir)
	if workerDir == "" {
		workerDir = "tools/cookie-worker"
	}
	if !filepath.IsAbs(workerDir) {
		workerDir = filepath.Clean(workerDir)
	}
	if _, err := os.Stat(workerDir); err != nil {
		if log != nil {
			log.Warn("Gemini cookie worker directory unavailable",
				zap.String("dir", workerDir),
				zap.Error(err),
			)
		}
		return nil
	}
	timeout := time.Duration(cfg.Gemini.CookieWorkerTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	apiBase := "http://127.0.0.1:" + cfg.Server.Port
	return func(ctx context.Context, accountID string) error {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		env := map[string]string{
			"API_BASE":                apiBase,
			"COOKIE_WORKER_ACCOUNT":   accountID,
			"COOKIE_WORKER_ONCE":      "true",
			"COOKIE_WORKER_OPEN_ONLY": "false",
		}
		if cfg.Admin.CookieSyncToken != "" {
			env["COOKIE_SYNC_TOKEN"] = cfg.Admin.CookieSyncToken
		}
		return runCookieWorkerCommand(runCtx, workerDir, command, env)
	}
}

func runCookieWorkerCommand(ctx context.Context, dir, command string, env map[string]string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(output.String())
		if len(text) > 2000 {
			text = text[len(text)-2000:]
		}
		if text == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, text)
	}
	return nil
}

type accountPoolState struct {
	ActiveAccountID string    `json:"active_account_id"`
	ActiveUntil     time.Time `json:"active_until"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (p *ClientPool) restoreActiveLocked(now time.Time) bool {
	state, err := readAccountPoolState(p.statePath)
	if err != nil || state.ActiveAccountID == "" || !state.ActiveUntil.After(now) {
		return false
	}
	for idx, client := range p.clients {
		if client.accountID != state.ActiveAccountID || !client.IsHealthy() {
			continue
		}
		p.activeIndex = idx
		p.activeUntil = state.ActiveUntil
		if p.log != nil {
			p.log.Info("Gemini active account restored",
				zap.String("account", client.accountID),
				zap.Time("until", p.activeUntil),
			)
			p.logAccountAudit("active_account_restored", client, "", "state_file")
		}
		return true
	}
	return false
}

func (p *ClientPool) saveActiveStateLocked() {
	if p == nil || strings.TrimSpace(p.statePath) == "" || p.activeIndex < 0 || p.activeIndex >= len(p.clients) {
		return
	}
	state := accountPoolState{
		ActiveAccountID: p.clients[p.activeIndex].accountID,
		ActiveUntil:     p.activeUntil,
		UpdatedAt:       time.Now(),
	}
	if err := writeAccountPoolState(p.statePath, state); err != nil && p.log != nil {
		p.log.Warn("failed to save Gemini account state", zap.String("path", p.statePath), zap.Error(err))
	}
}

func readAccountPoolState(path string) (accountPoolState, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return accountPoolState{}, os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return accountPoolState{}, err
	}
	var state accountPoolState
	if err := json.Unmarshal(data, &state); err != nil {
		return accountPoolState{}, err
	}
	return state, nil
}

func writeAccountPoolState(path string, state accountPoolState) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (p *ClientPool) logAccountAudit(event string, client *Client, conversationID, reason string) {
	if p == nil || p.log == nil || !accountAuditLogEnabled() {
		return
	}
	accountID := ""
	healthy := false
	state := ""
	if client != nil {
		accountID = client.accountID
		healthy = client.IsHealthy()
		state = client.AccountStatus().State
	}
	fields := []zap.Field{
		zap.String("event", event),
		zap.String("account", accountID),
		zap.Bool("healthy", healthy),
		zap.String("state", state),
		zap.String("reason", reason),
	}
	if client != nil {
		fields = append(fields,
			zap.Bool("proxy_enabled", strings.TrimSpace(client.proxyURL) != ""),
			zap.String("proxy", redactProxyURL(client.proxyURL)),
		)
	}
	if conversationID != "" {
		fields = append(fields, zap.String("conversation_id", conversationID))
	}
	p.log.Info("Gemini account audit", fields...)
}

func accountAuditLogEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("GEMINI_ACCOUNT_AUDIT_LOG")))
	switch value {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// AddAccount creates a new Gemini account client at runtime and adds it to the pool.
func (p *ClientPool) AddAccount(ctx context.Context, accountID, secure1PSID, secure1PSIDTS, proxyURL string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return fmt.Errorf("account ID is required")
	}
	if secure1PSID == "" {
		return fmt.Errorf("secure_1psid is required")
	}

	p.mu.Lock()
	if p.clientsByID[accountID] != nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q already exists", accountID)
	}
	p.mu.Unlock()

	account := configs.GeminiAccountConfig{
		ID:              accountID,
		Secure1PSID:     secure1PSID,
		Secure1PSIDTS:   secure1PSIDTS,
		CookieSource:    "console",
		ProxyURL:        proxyURL,
		Priority:        1,
		StayMinutes:     60,
		RefreshInterval: p.cfg.Gemini.RefreshInterval,
		MaxRetries:      p.cfg.Gemini.MaxRetries,
	}

	client := NewClientForAccount(p.cfg, account, p.log)

	p.mu.Lock()
	p.clients = append(p.clients, client)
	p.clientsByID[accountID] = client
	p.stayByID[accountID] = accountStayDuration(account)
	p.mu.Unlock()

	if p.log != nil {
		p.log.Info("Gemini account added via console",
			zap.String("account", accountID),
			zap.String("proxy", redactProxyURL(proxyURL)),
		)
	}

	// Initialize in background so the API call doesn't block.
	go func() {
		if err := client.Init(context.Background()); err != nil {
			if p.log != nil {
				p.log.Warn("Console-added account init failed",
					zap.String("account", accountID),
					zap.Error(err),
				)
			}
		}
	}()

	return nil
}

// RemoveAccount removes an account from the pool and closes its client.
func (p *ClientPool) RemoveAccount(ctx context.Context, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	p.mu.Lock()
	client, ok := p.clientsByID[accountID]
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", accountID)
	}

	// Remove from slices/maps.
	delete(p.clientsByID, accountID)
	delete(p.stayByID, accountID)
	newClients := make([]*Client, 0, len(p.clients)-1)
	for _, c := range p.clients {
		if c.accountID != accountID {
			newClients = append(newClients, c)
		}
	}
	p.clients = newClients

	// Fix activeIndex if needed.
	if p.activeIndex >= len(p.clients) {
		p.activeIndex = -1
	}

	// Remove conversation bindings for this account.
	for convID, accID := range p.conversationTo {
		if accID == accountID {
			delete(p.conversationTo, convID)
		}
	}
	p.mu.Unlock()

	if err := client.Close(); err != nil && p.log != nil {
		p.log.Warn("failed to close removed account", zap.String("account", accountID), zap.Error(err))
	}

	if p.log != nil {
		p.log.Info("Gemini account removed via console", zap.String("account", accountID))
	}
	return nil
}

// UpdateAccountProxy changes the proxy URL for an existing account.
func (p *ClientPool) UpdateAccountProxy(ctx context.Context, accountID, proxyURL string) error {
	_ = ctx
	client := p.clientByID(accountID)
	if client == nil {
		return fmt.Errorf("account %q not found", accountID)
	}
	client.mu.Lock()
	client.proxyURL = strings.TrimSpace(proxyURL)
	client.mu.Unlock()

	// Rebuild HTTP clients with the new proxy.
	newReqClient := req.NewClient().
		SetTimeout(10 * time.Minute).
		SetCommonHeaders(DefaultHeaders)
	if strings.TrimSpace(proxyURL) != "" {
		newReqClient.SetProxyURL(proxyURL)
	}
	newRawTransport := &http.Transport{
		Proxy:                 proxyFunc(proxyURL),
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	newRawClient := &http.Client{
		Transport: newRawTransport,
		Timeout:   5 * time.Minute,
	}

	client.mu.Lock()
	client.httpClient = newReqClient
	client.rawHTTPClient = newRawClient
	client.mu.Unlock()

	if p.log != nil {
		p.log.Info("Gemini account proxy updated via console",
			zap.String("account", accountID),
			zap.String("proxy", redactProxyURL(proxyURL)),
		)
	}
	return nil
}

// TestAccount sends a simple test message through the specified account's client
// to verify the model truly works end-to-end. Returns the response text on success.
func (p *ClientPool) TestAccount(ctx context.Context, accountID string) (string, error) {
	client := p.clientByID(accountID)
	if client == nil {
		return "", fmt.Errorf("Gemini account %q not found", accountID)
	}
	if !client.IsHealthy() {
		return "", fmt.Errorf("Gemini account %q is not healthy (state: %s)", accountID, client.AccountStatus().State)
	}
	resp, err := client.GenerateContent(ctx, "Hi, please reply with only the word: OK")
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", fmt.Errorf("empty response from account %q", accountID)
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return "", fmt.Errorf("empty text in response from account %q", accountID)
	}
	return text, nil
}

func (p *ClientPool) bindConversation(conversationID, accountID string) {
	if conversationID == "" || accountID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conversationTo[conversationID] = accountID
}

type errorChatSession struct {
	err error
}

func (s *errorChatSession) SendMessage(context.Context, string, ...GenerateOption) (*Response, error) {
	return nil, s.err
}

func (s *errorChatSession) GetMetadata() *SessionMetadata { return nil }
func (s *errorChatSession) GetHistory() []Message         { return nil }
func (s *errorChatSession) Clear()                        {}
