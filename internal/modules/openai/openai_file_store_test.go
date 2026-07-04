package openai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gemini-free-api/internal/commons/models"
)

func TestReadMetadataRejectsPathTraversalFileID(t *testing.T) {
	store := newOpenAIFileStore(t.TempDir())

	_, err := store.readMetadata("../cookies/accounts")

	if err == nil {
		t.Fatal("expected path traversal file_id to be rejected")
	}
	if !strings.Contains(err.Error(), "invalid file_id") {
		t.Fatalf("expected invalid file_id error, got %v", err)
	}
}

func TestMetadataPathRejectsInvalidFileID(t *testing.T) {
	store := newOpenAIFileStore(t.TempDir())

	if _, err := store.metadataPath("../cookies/accounts"); err == nil {
		t.Fatal("expected path traversal file_id to be rejected")
	}
	if _, err := store.metadataPath("file-abc123"); err != nil {
		t.Fatalf("expected generated-style file_id to be accepted, got %v", err)
	}
}

func TestFetchAttachmentURLRejectsLocalhost(t *testing.T) {
	_, err := fetchAttachmentURL(t.Context(), models.Attachment{
		URL: "http://127.0.0.1:8787/health",
	})

	if err == nil {
		t.Fatal("expected localhost attachment URL to be rejected")
	}
	if !strings.Contains(err.Error(), "disallowed attachment host") {
		t.Fatalf("expected disallowed host error, got %v", err)
	}
}

func TestReadFileContentRejectsMetadataPathOutsideStore(t *testing.T) {
	dir := t.TempDir()
	store := newOpenAIFileStore(filepath.Join(dir, "files"))
	outsidePath := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	meta := openAIFileMetadata{
		openAIFileObject: openAIFileObject{
			ID:       "file-bad",
			Object:   "file",
			Filename: "outside.txt",
			Purpose:  "assistants",
		},
		Path:     outsidePath,
		MimeType: "text/plain",
	}
	if err := os.MkdirAll(store.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.writeMetadata(meta); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := store.readFileContent("file-bad")

	if err == nil {
		t.Fatal("expected metadata path outside store to be rejected")
	}
	if !strings.Contains(err.Error(), "outside file store") {
		t.Fatalf("expected outside store error, got %v", err)
	}
}
