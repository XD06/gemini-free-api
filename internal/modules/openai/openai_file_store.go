package openai

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gemini-free-api/internal/commons/models"
	"gemini-free-api/internal/modules/providers"
)

const defaultOpenAIFileStoreDir = "data/openai-files"

var openAIFileIDPattern = regexp.MustCompile(`^file-[A-Za-z0-9_-]+$`)

type openAIFileStore struct {
	dir      string
	maxFile  int64
	maxTotal int64
	ttl      time.Duration
	mu       sync.Mutex
}

type openAIFileObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

type openAIFileMetadata struct {
	openAIFileObject
	Path     string `json:"path"`
	MimeType string `json:"mime_type,omitempty"`
}

func newOpenAIFileStore(dir string) *openAIFileStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = defaultOpenAIFileStoreDir
	}
	store := &openAIFileStore{
		dir:      dir,
		maxFile:  envBytes("OPENAI_FILE_MAX_BYTES", 32<<20),
		maxTotal: envBytes("OPENAI_FILE_STORE_MAX_BYTES", 1<<30),
		ttl:      time.Duration(envInt64("OPENAI_FILE_TTL_HOURS", 24)) * time.Hour,
	}
	_ = store.cleanup()
	return store
}

func (s *openAIFileStore) saveUploadedFile(ctx context.Context, header *multipart.FileHeader, purpose string) (openAIFileObject, error) {
	if s == nil {
		return openAIFileObject{}, fmt.Errorf("file store is not configured")
	}
	if header == nil {
		return openAIFileObject{}, fmt.Errorf("file is required")
	}
	if header.Size > s.maxFile {
		return openAIFileObject{}, fmt.Errorf("file exceeds %d byte limit", s.maxFile)
	}
	src, err := header.Open()
	if err != nil {
		return openAIFileObject{}, fmt.Errorf("open uploaded file: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return openAIFileObject{}, fmt.Errorf("create file store: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cleanupLocked(); err != nil {
		return openAIFileObject{}, err
	}
	used, err := s.totalSizeLocked()
	if err != nil {
		return openAIFileObject{}, err
	}
	if header.Size >= 0 && used+header.Size > s.maxTotal {
		return openAIFileObject{}, fmt.Errorf("file store exceeds %d byte limit", s.maxTotal)
	}

	id, err := newOpenAIFileID()
	if err != nil {
		return openAIFileObject{}, err
	}
	filename := sanitizeStoredFilename(header.Filename)
	dataPath := filepath.Join(s.dir, id+"_"+filename)
	dst, err := os.OpenFile(dataPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return openAIFileObject{}, fmt.Errorf("create stored file: %w", err)
	}
	written, copyErr := copyWithContext(ctx, dst, io.LimitReader(src, s.maxFile+1))
	closeErr := dst.Close()
	if copyErr == nil && written > s.maxFile {
		copyErr = fmt.Errorf("file exceeds %d byte limit", s.maxFile)
	}
	if copyErr == nil && used+written > s.maxTotal {
		copyErr = fmt.Errorf("file store exceeds %d byte limit", s.maxTotal)
	}
	if copyErr != nil {
		_ = os.Remove(dataPath)
		return openAIFileObject{}, fmt.Errorf("store uploaded file: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dataPath)
		return openAIFileObject{}, fmt.Errorf("close stored file: %w", closeErr)
	}

	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	}
	meta := openAIFileMetadata{
		openAIFileObject: openAIFileObject{
			ID:        id,
			Object:    "file",
			Bytes:     written,
			CreatedAt: time.Now().Unix(),
			Filename:  filename,
			Purpose:   strings.TrimSpace(purpose),
		},
		Path:     dataPath,
		MimeType: mimeType,
	}
	if meta.Purpose == "" {
		meta.Purpose = "assistants"
	}
	if err := s.writeMetadata(meta); err != nil {
		_ = os.Remove(dataPath)
		return openAIFileObject{}, err
	}
	return meta.openAIFileObject, nil
}

func (s *openAIFileStore) listFiles() ([]openAIFileObject, error) {
	if s == nil {
		return nil, fmt.Errorf("file store is not configured")
	}
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return []openAIFileObject{}, nil
	}
	if err != nil {
		return nil, err
	}
	files := make([]openAIFileObject, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		meta, err := s.readMetadata(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		files = append(files, meta.openAIFileObject)
	}
	return files, nil
}

func (s *openAIFileStore) getFile(id string) (openAIFileObject, error) {
	meta, err := s.readMetadata(id)
	if err != nil {
		return openAIFileObject{}, err
	}
	return meta.openAIFileObject, nil
}

func (s *openAIFileStore) deleteFile(id string) (map[string]interface{}, error) {
	meta, err := s.readMetadata(id)
	if err != nil {
		return nil, err
	}
	dataPath, err := s.safeStoredDataPath(meta.Path)
	if err != nil {
		return nil, err
	}
	metaPath, err := s.metadataPath(id)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(dataPath)
	_ = os.Remove(metaPath)
	return map[string]interface{}{
		"id":      meta.ID,
		"object":  "file",
		"deleted": true,
	}, nil
}

func (s *openAIFileStore) readFileContent(id string) ([]byte, string, string, error) {
	meta, err := s.readMetadata(id)
	if err != nil {
		return nil, "", "", err
	}
	dataPath, err := s.safeStoredDataPath(meta.Path)
	if err != nil {
		return nil, "", "", err
	}
	data, err := os.ReadFile(dataPath)
	if err != nil {
		return nil, "", "", err
	}
	return data, meta.Filename, meta.MimeType, nil
}

func (s *openAIFileStore) attachmentForFileID(id, fallbackName, fallbackMime string) (models.Attachment, error) {
	data, filename, mimeType, err := s.readFileContent(id)
	if err != nil {
		return models.Attachment{}, err
	}
	if strings.TrimSpace(fallbackName) != "" {
		filename = strings.TrimSpace(fallbackName)
	}
	if strings.TrimSpace(fallbackMime) != "" {
		mimeType = strings.TrimSpace(fallbackMime)
	}
	return models.Attachment{
		Name:     filename,
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(data),
		FileID:   id,
	}, nil
}

func (s *openAIFileStore) writeMetadata(meta openAIFileMetadata) error {
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	path, err := s.metadataPath(meta.ID)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0600)
}

func (s *openAIFileStore) readMetadata(id string) (openAIFileMetadata, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return openAIFileMetadata{}, fmt.Errorf("file_id is required")
	}
	path, err := s.metadataPath(id)
	if err != nil {
		return openAIFileMetadata{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return openAIFileMetadata{}, fmt.Errorf("file %q not found", id)
	}
	var meta openAIFileMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return openAIFileMetadata{}, fmt.Errorf("decode file metadata %q: %w", id, err)
	}
	return meta, nil
}

func (s *openAIFileStore) metadataPath(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !openAIFileIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid file_id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *openAIFileStore) safeStoredDataPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("stored file path is required")
	}
	storeDir, err := filepath.Abs(s.dir)
	if err != nil {
		return "", err
	}
	dataPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(storeDir, dataPath)
	if err != nil {
		return "", err
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("stored file path %q is outside file store", path)
	}
	return dataPath, nil
}

func (s *OpenAIService) inputFilesFromModelMessages(ctx context.Context, messages []models.Message) ([]providers.InputFile, error) {
	files := make([]providers.InputFile, 0)
	for _, msg := range messages {
		for _, attachment := range msg.Attachments {
			file, ok, err := s.inputFileFromAttachment(ctx, attachment)
			if err != nil {
				return nil, err
			}
			if ok {
				files = append(files, file)
			}
		}
	}
	return files, nil
}

func (s *OpenAIService) inputFileFromAttachment(ctx context.Context, attachment models.Attachment) (providers.InputFile, bool, error) {
	if strings.TrimSpace(attachment.FileID) != "" {
		if s == nil || s.fileStore == nil {
			return providers.InputFile{}, false, fmt.Errorf("file store is not configured")
		}
		resolved, err := s.fileStore.attachmentForFileID(attachment.FileID, attachment.Name, attachment.MimeType)
		if err != nil {
			return providers.InputFile{}, false, err
		}
		attachment = resolved
	}
	if strings.TrimSpace(attachment.Data) != "" {
		data, err := providers.DecodeBase64Data(attachment.Data)
		if err != nil {
			return providers.InputFile{}, false, fmt.Errorf("decode attachment %q: %w", attachment.Name, err)
		}
		if int64(len(data)) > s.fileStore.maxFile {
			return providers.InputFile{}, false, fmt.Errorf("attachment exceeds %d byte limit", s.fileStore.maxFile)
		}
		return providers.InputFile{
			Name:     attachment.Name,
			MimeType: attachment.MimeType,
			Data:     data,
		}, true, nil
	}
	if strings.TrimSpace(attachment.URL) != "" {
		file, err := fetchAttachmentURL(ctx, attachment)
		if err != nil {
			return providers.InputFile{}, false, err
		}
		return file, true, nil
	}
	return providers.InputFile{}, false, nil
}

func fetchAttachmentURL(ctx context.Context, attachment models.Attachment) (providers.InputFile, error) {
	attachmentURL, err := validateAttachmentURL(attachment.URL)
	if err != nil {
		return providers.InputFile{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, attachmentURL.String(), nil)
	if err != nil {
		return providers.InputFile{}, fmt.Errorf("build attachment fetch request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	resp, err := newAttachmentHTTPClient().Do(req)
	if err != nil {
		return providers.InputFile{}, fmt.Errorf("fetch attachment %q: %w", attachment.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providers.InputFile{}, fmt.Errorf("fetch attachment %q failed with status %d", attachment.URL, resp.StatusCode)
	}
	maxSize := envBytes("OPENAI_FILE_MAX_BYTES", 32<<20)
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return providers.InputFile{}, fmt.Errorf("read attachment %q: %w", attachment.URL, err)
	}
	if int64(len(data)) > maxSize {
		return providers.InputFile{}, fmt.Errorf("attachment exceeds %d byte limit", maxSize)
	}
	if len(data) == 0 {
		return providers.InputFile{}, fmt.Errorf("attachment %q is empty", attachment.URL)
	}
	mimeType := strings.TrimSpace(attachment.MimeType)
	if mimeType == "" {
		mimeType = resp.Header.Get("Content-Type")
	}
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = strings.TrimSpace(mimeType[:idx])
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	name := strings.TrimSpace(attachment.Name)
	if name == "" || strings.HasPrefix(name, "image_") {
		name = filenameFromURL(attachment.URL, mimeType)
	}
	return providers.InputFile{Name: name, MimeType: mimeType, Data: data}, nil
}

func newAttachmentHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid attachment address %q: %w", address, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve attachment host %q: %w", host, err)
			}
			for _, candidate := range ips {
				if isDisallowedAttachmentIP(candidate.IP) {
					continue
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			}
			return nil, fmt.Errorf("disallowed attachment host %q", host)
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many attachment redirects")
			}
			_, err := validateAttachmentURL(req.URL.String())
			return err
		},
	}
}

func validateAttachmentURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid attachment URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported attachment URL scheme %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("attachment URL host is required")
	}
	if strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("disallowed attachment host %q", host)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve attachment host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve attachment host %q: no addresses", host)
	}
	for _, ip := range ips {
		if isDisallowedAttachmentIP(ip) {
			return nil, fmt.Errorf("disallowed attachment host %q", host)
		}
	}
	return parsed, nil
}

func isDisallowedAttachmentIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	// Go's IsPrivate intentionally excludes shared and benchmark ranges, but
	// neither is a valid destination for user-supplied remote attachments.
	for _, network := range attachmentBlockedNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

var attachmentBlockedNetworks = []*net.IPNet{
	mustParseCIDR("100.64.0.0/10"),
	mustParseCIDR("198.18.0.0/15"),
}

func mustParseCIDR(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}

func filenameFromURL(rawURL, mimeType string) string {
	base := filepath.Base(strings.SplitN(strings.TrimSpace(rawURL), "?", 2)[0])
	if base == "." || base == "/" || base == "" {
		base = "remote"
	}
	if filepath.Ext(base) == "" {
		if exts, _ := mime.ExtensionsByType(mimeType); len(exts) > 0 {
			base += exts[0]
		}
	}
	return sanitizeStoredFilename(base)
}

func newOpenAIFileID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "file-" + hex.EncodeToString(buf[:]), nil
}

func sanitizeStoredFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		return "file.bin"
	}
	var b strings.Builder
	for _, r := range filename {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "file.bin"
	}
	return b.String()
}

func envInt64(name string, fallback int64) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
func envBytes(name string, fallback int64) int64 { return envInt64(name, fallback) }
func (s *openAIFileStore) cleanup() error        { s.mu.Lock(); defer s.mu.Unlock(); return s.cleanupLocked() }
func (s *openAIFileStore) cleanupLocked() error {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	metadata := map[string]openAIFileMetadata{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if meta, err := s.readMetadata(id); err == nil {
				metadata[id] = meta
			} else {
				_ = os.Remove(filepath.Join(s.dir, entry.Name()))
			}
		}
	}
	now := time.Now()
	for id, meta := range metadata {
		dataPath, err := s.safeStoredDataPath(meta.Path)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dataPath); os.IsNotExist(err) {
			path, _ := s.metadataPath(id)
			_ = os.Remove(path)
			continue
		}
		if s.ttl > 0 && now.Sub(time.Unix(meta.CreatedAt, 0)) > s.ttl {
			_ = os.Remove(dataPath)
			path, _ := s.metadataPath(id)
			_ = os.Remove(path)
			delete(metadata, id)
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		matched := false
		for _, meta := range metadata {
			if filepath.Clean(meta.Path) == filepath.Clean(filepath.Join(s.dir, entry.Name())) {
				matched = true
				break
			}
		}
		if !matched {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
	return nil
}
func (s *openAIFileStore) totalSizeLocked() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			m, writeErr := dst.Write(buf[:n])
			written += int64(m)
			if writeErr != nil {
				return written, writeErr
			}
			if m != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
