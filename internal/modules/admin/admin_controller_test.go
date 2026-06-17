package admin

import (
	"net/http/httptest"
	"testing"

	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

func TestAdminAccountsRequiresToken(t *testing.T) {
	app := fiber.New()
	controller := NewController(&providers.Client{}, &configs.Config{
		Admin: configs.AdminConfig{CookieSyncToken: "secret"},
	}, zap.NewNop())
	controller.Register(app.Group("/admin"))

	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAdminAccountsReturnsStatusesWithToken(t *testing.T) {
	app := fiber.New()
	controller := NewController(&providers.Client{}, &configs.Config{
		Admin: configs.AdminConfig{CookieSyncToken: "secret"},
	}, zap.NewNop())
	controller.Register(app.Group("/admin"))

	req := httptest.NewRequest("GET", "/admin/accounts", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
