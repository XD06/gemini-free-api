package openai

import "testing"

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
