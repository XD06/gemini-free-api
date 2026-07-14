package admin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRequestRecordSerializesSafeDiagnosticMetadata(t *testing.T) {
	record := RequestRecord{
		ID:               "req-1",
		Timestamp:        time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		AccountID:        "acc1",
		Protocol:         "openai",
		Model:            "gemini-3.5-flash",
		ThinkingLevel:    "extended",
		Stream:           true,
		Status:           "success",
		Duration:         3100,
		FirstByteLatency: 900,
		RequestPath:      "/v1/chat/completions",
	}

	body, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal request record: %v", err)
	}
	text := string(body)
	for _, want := range []string{`"protocol":"openai"`, `"thinking_level":"extended"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %s in %s", want, text)
		}
	}
}

func TestRequestLoggerKeepsNewestRecords(t *testing.T) {
	logger := NewRequestLogger(2)
	logger.LogRequest(RequestRecord{ID: "first"})
	logger.LogRequest(RequestRecord{ID: "second"})
	logger.LogRequest(RequestRecord{ID: "third"})

	records := logger.GetRecent(10)
	if len(records) != 2 || records[0].ID != "third" || records[1].ID != "second" {
		t.Fatalf("unexpected records: %#v", records)
	}
}
