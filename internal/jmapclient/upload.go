package jmapclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// UploadBlob implements mail.Provider (FR-H3): POST the bytes to the
// session uploadUrl with the {accountId} placeholder expanded and the
// attachment's media type as Content-Type (RFC 8620 §6.1). The stream
// client carries it for the same reason downloads do — a large attachment
// must not be cut off by the per-request timeout — while ctx stays the
// cancellation authority (FR-K1). size is sent as Content-Length so the
// server can reject oversize uploads before the body is read.
func (c *Client) UploadBlob(ctx context.Context, name, mediaType string, size int64, r io.Reader) (mail.Attachment, error) {
	if c.session == nil {
		return mail.Attachment{}, errors.New("jmapclient: not connected")
	}
	if c.session.UploadURL == "" {
		return mail.Attachment{}, errors.New("jmapclient: server advertises no uploadUrl")
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	u := strings.ReplaceAll(c.session.UploadURL, "{accountId}", url.PathEscape(c.accountID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return mail.Attachment{}, fmt.Errorf("jmapclient: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mediaType)
	if size >= 0 {
		req.ContentLength = size
	}

	resp, err := c.streamHC.Do(req)
	if err != nil {
		return mail.Attachment{}, fmt.Errorf("jmapclient: POST %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return mail.Attachment{}, fmt.Errorf("jmapclient: POST %s: %w", u, ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return mail.Attachment{}, &ServerError{Status: resp.StatusCode, Detail: strings.TrimSpace(string(detail))}
	}

	var out struct {
		BlobID string `json:"blobId"`
		Type   string `json:"type"`
		Size   int64  `json:"size"`
	}
	if err := decodeJSON(resp.Body, &out); err != nil {
		return mail.Attachment{}, fmt.Errorf("jmapclient: decode upload response: %w", err)
	}
	if out.BlobID == "" {
		return mail.Attachment{}, errors.New("jmapclient: upload returned no blobId")
	}
	if out.Type == "" {
		out.Type = mediaType
	}
	return mail.Attachment{BlobID: mail.ID(out.BlobID), Name: name, Type: out.Type, Size: out.Size}, nil
}
