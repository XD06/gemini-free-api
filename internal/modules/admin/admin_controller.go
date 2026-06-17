package admin

import (
	"context"
	"fmt"
	"os"
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
	group.Post("/accounts/:account_id/cookies", c.HandleUpdateCookies)
	group.Post("/accounts/:account_id/refresh", c.HandleRefreshAccount)
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
	reqCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
		c.log.Info("admin cookie update succeeded",
			zap.String("account", accountID),
			zap.String("source", req.Source),
		)
		c.logAccountAudit("cookie_sync_ok", accountID, req.Source, "validated")
	}
	return ctx.JSON(fiber.Map{"status": "ok", "account": accountID})
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
