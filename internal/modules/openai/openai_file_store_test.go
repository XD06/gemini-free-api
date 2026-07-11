package openai

import (
	"context"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestAttachmentHTTPClientRejectsRedirectToLocalhost(t *testing.T) {
	client := newAttachmentHTTPClient()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/private", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = client.CheckRedirect(request, nil)
	if err == nil {
		t.Fatal("expected redirect to localhost to be rejected")
	}
	if !strings.Contains(err.Error(), "disallowed attachment host") {
		t.Fatalf("expected disallowed host error, got %v", err)
	}
}

func TestDisallowedAttachmentIPRejectsSharedAddressSpace(t *testing.T) {
	if !isDisallowedAttachmentIP(net.ParseIP("100.64.0.1")) {
		t.Fatal("expected carrier-grade NAT address to be rejected")
	}
	if isDisallowedAttachmentIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("expected public address to be allowed")
	}
}

func TestFileStoreCleanupRemovesExpiredAndOrphanFiles(t *testing.T) {
	store := newOpenAIFileStore(t.TempDir())
	store.ttl = time.Hour
	if err := os.MkdirAll(store.dir, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(store.dir, "orphan.bin")
	if err := os.WriteFile(orphan, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(store.dir, "file-old_data.bin")
	if err := os.WriteFile(dataPath, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	meta := openAIFileMetadata{openAIFileObject: openAIFileObject{ID: "file-old", CreatedAt: time.Now().Add(-2 * time.Hour).Unix(), Filename: "data.bin"}, Path: dataPath}
	if err := store.writeMetadata(meta); err != nil {
		t.Fatal(err)
	}
	if err := store.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("expected orphan removed")
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatal("expected expired data removed")
	}
}

func TestFileStoreRejectsOversizedCopy(t *testing.T) {
	store := newOpenAIFileStore(t.TempDir())
	store.maxFile = 2
	header := &multipart.FileHeader{Filename: "large.bin", Size: 3}
	_, err := store.saveUploadedFile(context.Background(), header, "assistants")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}
