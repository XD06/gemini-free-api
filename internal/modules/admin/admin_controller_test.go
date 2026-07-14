package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type adminAccountManagerStub struct {
	providers.Client
	refreshErr      error
	testErr         error
	updateCookieErr error
}

func (s *adminAccountManagerStub) UpdateAccountCookies(context.Context, string, string, string) error {
	return s.updateCookieErr
}

func (s *adminAccountManagerStub) RefreshAccount(context.Context, string) error {
	return s.refreshErr
}

func (s *adminAccountManagerStub) TestAccount(context.Context, string) (string, error) {
	if s.testErr != nil {
		return "", s.testErr
	}
	return "OK", nil
}

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

func TestAdminCookieUpdateAcknowledgesSaveBeforeValidation(t *testing.T) {
	app := fiber.New()
	controller := NewController(&adminAccountManagerStub{}, &configs.Config{
		Admin: configs.AdminConfig{CookieSyncToken: "secret"},
	}, zap.NewNop())
	controller.Register(app.Group("/admin"))

	req := httptest.NewRequest("POST", "/admin/accounts/main/cookies", strings.NewReader(`{"secure_1psid":"psid","secure_1psidts":"ts"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
	var payload struct {
		Status     string `json:"status"`
		Validation string `json:"validation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "saved" || payload.Validation != "pending" {
		t.Fatalf("unexpected response: %#v", payload)
	}
}

func TestAdminRefreshMapsCookiePairMismatchToActionableError(t *testing.T) {
	stub := &adminAccountManagerStub{refreshErr: &providers.AccountOperationError{
		Code:      providers.AccountErrorCookiePairMismatch,
		State:     providers.AccountStateNeedsManualLogin,
		Message:   "Gemini rejected the cookie pair.",
		Retryable: false,
		Action:    "Update both cookies from the same browser session.",
	}}
	app := fiber.New()
	controller := NewController(stub, &configs.Config{
		Admin: configs.AdminConfig{CookieSyncToken: "secret"},
	}, zap.NewNop())
	controller.Register(app.Group("/admin"))

	req := httptest.NewRequest("POST", "/admin/accounts/main/refresh", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			State     string `json:"state"`
			Retryable bool   `json:"retryable"`
			Action    string `json:"action"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != providers.AccountErrorCookiePairMismatch || payload.Error.State != providers.AccountStateNeedsManualLogin || payload.Error.Retryable || payload.Error.Action == "" {
		t.Fatalf("unexpected error payload: %#v", payload.Error)
	}
}

func TestAdminTestMapsRefreshInProgressToConflict(t *testing.T) {
	stub := &adminAccountManagerStub{testErr: &providers.AccountOperationError{
		Code:      providers.AccountErrorRefreshInProgress,
		State:     providers.AccountStateRefreshing,
		Message:   "The Gemini account is currently refreshing.",
		Retryable: true,
		Action:    "Wait and retry.",
	}}
	app := fiber.New()
	controller := NewController(stub, &configs.Config{
		Admin: configs.AdminConfig{CookieSyncToken: "secret"},
	}, zap.NewNop())
	controller.Register(app.Group("/admin"))

	req := httptest.NewRequest("POST", "/admin/accounts/main/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}
