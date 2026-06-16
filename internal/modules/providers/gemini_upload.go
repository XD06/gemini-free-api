package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"gemini-free-api/internal/commons/models"
)

type uploadedFile struct {
	ID   string
	Name string
}

// InputFilesFromAttachments decodes base64 API attachments into provider upload inputs.
func InputFilesFromAttachments(messages []models.Message) ([]InputFile, error) {
	var files []InputFile
	for _, msg := range messages {
		msgFiles, err := InputFilesFromAttachmentList(msg.Attachments)
		if err != nil {
			return nil, err
		}
		files = append(files, msgFiles...)
	}
	return files, nil
}

// InputFilesFromAttachmentList decodes base64 API attachments into provider upload inputs.
func InputFilesFromAttachmentList(attachments []models.Attachment) ([]InputFile, error) {
	files := make([]InputFile, 0, len(attachments))
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.Data) == "" {
			continue
		}
		data, err := decodeBase64Data(attachment.Data)
		if err != nil {
			return nil, fmt.Errorf("decode attachment %q: %w", attachment.Name, err)
		}
		files = append(files, InputFile{
			Name:     attachment.Name,
			MimeType: attachment.MimeType,
			Data:     data,
		})
	}
	return files, nil
}

func DecodeBase64Data(value string) ([]byte, error) {
	return decodeBase64Data(value)
}

func decodeBase64Data(value string) ([]byte, error) {
	cleaned := strings.TrimSpace(value)
	if data, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(cleaned); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(cleaned); err == nil {
		return data, nil
	}
	return base64.RawURLEncoding.DecodeString(cleaned)
}

func (c *Client) uploadRequestFiles(ctx context.Context, cfg *GenerateConfig, cookieHdr string) ([]uploadedFile, error) {
	total := len(cfg.Files) + len(cfg.InputFiles)
	if total == 0 {
		return nil, nil
	}

	out := make([]uploadedFile, 0, total)
	for _, path := range cfg.Files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read file %q: %w", path, err)
		}
		name := filepath.Base(path)
		mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		uploaded, err := c.uploadFile(ctx, name, mimeType, data, cookieHdr)
		if err != nil {
			return nil, err
		}
		out = append(out, uploaded)
	}

	for i, file := range cfg.InputFiles {
		name := strings.TrimSpace(file.Name)
		if name == "" {
			name = fmt.Sprintf("input_%d%s", i+1, extensionForMimeType(file.MimeType))
		}
		mimeType := strings.TrimSpace(file.MimeType)
		if mimeType == "" {
			mimeType = http.DetectContentType(file.Data)
		}
		uploaded, err := c.uploadFile(ctx, name, mimeType, file.Data, cookieHdr)
		if err != nil {
			return nil, err
		}
		out = append(out, uploaded)
	}

	return out, nil
}

func (c *Client) uploadFile(ctx context.Context, filename, mimeType string, data []byte, cookieHdr string) (uploadedFile, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeQuotes(filename)))
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return uploadedFile{}, fmt.Errorf("create upload part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return uploadedFile{}, fmt.Errorf("write upload part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return uploadedFile{}, fmt.Errorf("close upload form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, EndpointUpload, &body)
	if err != nil {
		return uploadedFile{}, fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Tenant-Id", "bard-storage")
	req.Header.Set("Push-ID", c.pushIDOrDefault())
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	if cookieHdr != "" {
		req.Header.Set("Cookie", cookieHdr)
	}

	resp, err := c.httpClient.GetClient().Do(req)
	if err != nil {
		return uploadedFile{}, fmt.Errorf("upload %q failed: %w", filename, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uploadedFile{}, fmt.Errorf("upload %q failed with status %d: %s", filename, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	id := strings.TrimSpace(string(respBody))
	if id == "" {
		return uploadedFile{}, fmt.Errorf("upload %q returned empty file id", filename)
	}
	return uploadedFile{ID: id, Name: filename}, nil
}

func (c *Client) pushIDOrDefault() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.pushID != "" {
		return c.pushID
	}
	return "feeds/mcudyrk2a4khkz"
}

func escapeQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

func extensionForMimeType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}
