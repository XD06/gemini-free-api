package admin

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type Controller struct {
	client providers.GeminiClient
	cfg    *configs.Config
	log    *zap.Logger
}

func NewController(client providers.GeminiClient, cfg *configs.Config, log *zap.Logger) *Controller {
	return &Controller{
		client: client,
		cfg:    cfg,
		log:    log,
	}
}

func (c *Controller) Register(group fiber.Router) {
	group.Get("/accounts", c.HandleListAccounts)
	group.Post("/accounts", c.HandleAddAccount)
	group.Delete("/accounts/:account_id", c.HandleRemoveAccount)
	group.Post("/accounts/:account_id/cookies", c.HandleUpdateCookies)
	group.Post("/accounts/:account_id/proxy", c.HandleUpdateProxy)
	group.Post("/accounts/:account_id/refresh", c.HandleRefreshAccount)
	group.Post("/accounts/:account_id/test", c.HandleTestAccount)
	group.Post("/proxy-test", c.HandleTestProxy)
	group.Get("/requests", c.HandleListRequests)
	group.Get("/requests/stats", c.HandleRequestStats)
	group.Delete("/requests", c.HandleClearRequests)
}

func (c *Controller) HandleListAccounts(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	return ctx.JSON(fiber.Map{
		"accounts": manager.ListAccountStatuses(),
	})
}

type updateCookiesRequest struct {
	Secure1PSID   string `json:"secure_1psid"`
	Secure1PSIDTS string `json:"secure_1psidts"`
	Source        string `json:"source,omitempty"`
	ObservedAt    int64  `json:"observed_at,omitempty"`
}

type addAccountRequest struct {
	AccountID     string `json:"account_id"`
	Secure1PSID   string `json:"secure_1psid"`
	Secure1PSIDTS string `json:"secure_1psidts"`
	ProxyURL      string `json:"proxy_url,omitempty"`
}

type updateProxyRequest struct {
	ProxyURL string `json:"proxy_url"`
}

func (c *Controller) HandleUpdateCookies(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}

	var req updateCookiesRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	accountID := ctx.Params("account_id")
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.UpdateAccountCookies(reqCtx, accountID, req.Secure1PSID, req.Secure1PSIDTS); err != nil {
		if c.log != nil {
			c.log.Warn("admin cookie update failed",
				zap.String("account", accountID),
				zap.String("source", req.Source),
				zap.Error(err),
			)
			c.logAccountAudit("cookie_sync_failed", accountID, req.Source, "validation_failed")
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "cookie_update_failed"))
	}

	if c.log != nil {
		c.log.Info("admin cookie update accepted, validating in background",
			zap.String("account", accountID),
			zap.String("source", req.Source),
		)
		c.logAccountAudit("cookie_sync_ok", accountID, req.Source, "accepted")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": accountID, "message": "cookies saved, validating in background"})
}

func (c *Controller) HandleRefreshAccount(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	accountID := ctx.Params("account_id")
	reqCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.RefreshAccount(reqCtx, accountID); err != nil {
		if c.log != nil {
			c.logAccountAudit("manual_refresh_failed", accountID, "", "admin_refresh_failed")
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "refresh_failed"))
	}
	if c.log != nil {
		c.logAccountAudit("manual_refresh_ok", accountID, "", "admin_refresh")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": accountID})
}

func (c *Controller) HandleAddAccount(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	var req addAccountRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}
	if strings.TrimSpace(req.AccountID) == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("account_id is required"), "invalid_request_error"))
	}
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.AddAccount(reqCtx, req.AccountID, req.Secure1PSID, req.Secure1PSIDTS, req.ProxyURL); err != nil {
		if c.log != nil {
			c.log.Warn("admin add account failed",
				zap.String("account", req.AccountID),
				zap.Error(err),
			)
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "add_account_failed"))
	}
	if c.log != nil {
		c.log.Info("admin add account succeeded",
			zap.String("account", req.AccountID),
		)
		c.logAccountAudit("add_account", req.AccountID, "console", "created")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": req.AccountID})
}

func (c *Controller) HandleRemoveAccount(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	accountID := ctx.Params("account_id")
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.RemoveAccount(reqCtx, accountID); err != nil {
		if c.log != nil {
			c.log.Warn("admin remove account failed",
				zap.String("account", accountID),
				zap.Error(err),
			)
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "remove_account_failed"))
	}
	if c.log != nil {
		c.log.Info("admin remove account succeeded",
			zap.String("account", accountID),
		)
		c.logAccountAudit("remove_account", accountID, "console", "deleted")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": accountID})
}

func (c *Controller) HandleUpdateProxy(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	var req updateProxyRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}
	accountID := ctx.Params("account_id")
	reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.UpdateAccountProxy(reqCtx, accountID, req.ProxyURL); err != nil {
		if c.log != nil {
			c.log.Warn("admin proxy update failed",
				zap.String("account", accountID),
				zap.Error(err),
			)
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "proxy_update_failed"))
	}
	if c.log != nil {
		c.log.Info("admin proxy update succeeded",
			zap.String("account", accountID),
		)
		c.logAccountAudit("proxy_update", accountID, "console", "updated")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": accountID})
}

// HandleTestAccount sends a simple test chat message through the specified account
// to verify the model truly works end-to-end (not just model listing).
func (c *Controller) HandleTestAccount(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	manager, err := c.accountManager()
	if err != nil {
		return ctx.Status(fiber.StatusNotImplemented).JSON(utils.ErrorToResponse(err, "not_supported"))
	}
	accountID := ctx.Params("account_id")
	reqCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t0 := time.Now()
	text, err := manager.TestAccount(reqCtx, accountID)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		if c.log != nil {
			c.log.Warn("admin account test failed",
				zap.String("account", accountID),
				zap.Int64("latency_ms", latency),
				zap.Error(err),
			)
			c.logAccountAudit("account_test_failed", accountID, "console", "test_failed")
		}
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "account_test_failed"))
	}
	if c.log != nil {
		c.log.Info("admin account test succeeded",
			zap.String("account", accountID),
			zap.Int64("latency_ms", latency),
		)
		c.logAccountAudit("account_test_ok", accountID, "console", "test_passed")
	}
	// Truncate response text for display
	displayText := text
	if len(displayText) > 200 {
		displayText = displayText[:200] + "..."
	}
	return ctx.JSON(fiber.Map{
		"status":  "ok",
		"account": accountID,
		"latency": latency,
		"reply":   displayText,
	})
}

type proxyTestRequest struct {
	ProxyURL string `json:"proxy_url"`
}

// HandleTestProxy tests whether a proxy URL is reachable by making a simple
// HTTP request through it to a known endpoint.
func (c *Controller) HandleTestProxy(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	var req proxyTestRequest
	if err := ctx.Bind().Body(&req); err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("proxy_url is required"), "invalid_request_error"))
	}

	// Parse and validate proxy URL
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return ctx.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid proxy URL: %w", err), "invalid_request_error"))
	}

	// Build HTTP client with proxy
	transport := &http.Transport{
		Proxy:               http.ProxyURL(parsed),
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}

	testURL := "https://gemini.google.com"
	t0 := time.Now()
	resp, err := httpClient.Get(testURL)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		return ctx.JSON(fiber.Map{
			"status":  "fail",
			"latency": latency,
			"error":   err.Error(),
		})
	}
	defer resp.Body.Close()

	// Any HTTP response (even 403/302) means the proxy is reachable
	ok := resp.StatusCode < 500
	status := "ok"
	if !ok {
		status = "fail"
	}
	return ctx.JSON(fiber.Map{
		"status":     status,
		"latency":    latency,
		"http_code":  resp.StatusCode,
		"proxy_url":  proxyURL,
	})
}

func (c *Controller) logAccountAudit(event, accountID, source, reason string) {
	if c == nil || c.log == nil || !adminAccountAuditLogEnabled() {
		return
	}
	fields := []zap.Field{
		zap.String("event", event),
		zap.String("account", accountID),
		zap.String("reason", reason),
	}
	if strings.TrimSpace(source) != "" {
		fields = append(fields, zap.String("source", source))
	}
	c.log.Info("Gemini account audit", fields...)
}

func adminAccountAuditLogEnabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("GEMINI_ACCOUNT_AUDIT_LOG")))
	switch value {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (c *Controller) accountManager() (providers.AccountManager, error) {
	manager, ok := c.client.(providers.AccountManager)
	if !ok {
		return nil, fmt.Errorf("Gemini client does not expose account management")
	}
	return manager, nil
}

func (c *Controller) requireToken(ctx fiber.Ctx) error {
	expected := ""
	if c.cfg != nil {
		expected = strings.TrimSpace(c.cfg.Admin.CookieSyncToken)
	}
	if expected == "" {
		return ctx.Status(fiber.StatusForbidden).JSON(utils.ErrorToResponse(fmt.Errorf("COOKIE_SYNC_TOKEN is not configured"), "forbidden"))
	}
	token := strings.TrimSpace(ctx.Get("X-Cookie-Sync-Token"))
	if token == "" {
		auth := strings.TrimSpace(ctx.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if token != expected {
		return ctx.Status(fiber.StatusUnauthorized).JSON(utils.ErrorToResponse(fmt.Errorf("invalid admin token"), "unauthorized"))
	}
	return nil
}

// HandleListRequests returns recent API requests
func (c *Controller) HandleListRequests(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}

	limit := 100
	if limitStr := ctx.Query("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}

	logger := GetGlobalLogger()
	records := logger.GetRecent(limit)

	return ctx.JSON(fiber.Map{
		"requests": records,
		"total":    len(records),
	})
}

// HandleRequestStats returns request statistics
func (c *Controller) HandleRequestStats(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}

	logger := GetGlobalLogger()
	stats := logger.GetStats()

	return ctx.JSON(stats)
}

// HandleClearRequests clears all stored request records.
func (c *Controller) HandleClearRequests(ctx fiber.Ctx) error {
	if err := c.requireToken(ctx); err != nil {
		return err
	}
	logger := GetGlobalLogger()
	logger.Clear()
	return ctx.JSON(fiber.Map{"status": "ok"})
}
