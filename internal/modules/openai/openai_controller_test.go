package openai

import (
	"net/http/httptest"
	"testing"

	"gemini-free-api/internal/commons/configs"

	"github.com/gofiber/fiber/v3"
)

func TestTrimJSONBOM(t *testing.T) {
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"messages":[]}`)...)

	got := trimJSONBOM(body)

	if string(got) != `{"messages":[]}` {
		t.Fatalf("expected BOM to be removed, got %q", string(got))
	}
}

func TestTrimJSONBOMLeavesNormalJSON(t *testing.T) {
	body := []byte(`{"messages":[]}`)

	got := trimJSONBOM(body)

	if string(got) != string(body) {
		t.Fatalf("expected normal JSON to be unchanged, got %q", string(got))
	}
}

func TestFileRoutesRequireAdminToken(t *testing.T) {
	app := fiber.New()
	controller := &OpenAIController{
		service: &OpenAIService{fileStore: newOpenAIFileStore(t.TempDir())},
		cfg:     &configs.Config{Admin: configs.AdminConfig{CookieSyncToken: "secret"}},
	}
	controller.Register(app.Group("/openai/v1"))

	request := httptest.NewRequest("GET", "/openai/v1/files", nil)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("expected unauthenticated file request to return 401, got %d", response.StatusCode)
	}

	request = httptest.NewRequest("GET", "/openai/v1/files", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response, err = app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("expected authenticated file request to return 200, got %d", response.StatusCode)
	}
}
