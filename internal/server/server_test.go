package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
)

type readinessClient struct{ healthy bool }

func (c readinessClient) Init(context.Context) error { return nil }
func (c readinessClient) GenerateContent(context.Context, string, ...providers.GenerateOption) (*providers.Response, error) {
	return nil, nil
}
func (c readinessClient) GenerateContentStreamForOpenAI(context.Context, string, func(providers.StreamEvent) bool, ...providers.GenerateOption) error {
	return nil
}
func (c readinessClient) StartChat(...providers.ChatOption) providers.ChatSession { return nil }
func (c readinessClient) Close() error                                            { return nil }
func (c readinessClient) GetName() string                                         { return "test" }
func (c readinessClient) IsHealthy() bool                                         { return c.healthy }
func (c readinessClient) ListModels() []providers.ModelInfo                       { return nil }
func (c readinessClient) HasConversationState(string) bool                        { return false }
func (c readinessClient) IsConversationUntrusted(string) bool                     { return false }

func TestReadinessCheck(t *testing.T) {
	for _, test := range []struct {
		healthy bool
		want    int
	}{{false, fiber.StatusServiceUnavailable}, {true, fiber.StatusOK}} {
		app := fiber.New()
		app.Get("/ready", ReadinessCheck(readinessClient{healthy: test.healthy}))
		resp, err := app.Test(httptest.NewRequest("GET", "/ready", nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != test.want {
			t.Fatalf("healthy=%v: expected %d, got %d", test.healthy, test.want, resp.StatusCode)
		}
	}
}
