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

// DownloadBlob implements mail.Provider (FR-E4): GET the session downloadUrl
// with every RFC 8620 §2 template placeholder expanded. The stream client
// (no overall timeout) carries the request — large attachments must not be
// cut off by the per-request timeout; the caller's ctx governs cancellation
// and the auth+logging transport still applies (FR-A2, FR-K2).
func (c *Client) DownloadBlob(ctx context.Context, blobID mail.ID, name, mediaType string) (io.ReadCloser, error) {
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}
	if c.session.DownloadURL == "" {
		return nil, errors.New("jmapclient: server advertises no downloadUrl")
	}
	if name == "" {
		name = "attachment"
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	u := strings.NewReplacer(
		"{accountId}", url.PathEscape(c.accountID),
		"{blobId}", url.PathEscape(string(blobID)),
		"{name}", url.PathEscape(name),
		"{type}", url.QueryEscape(mediaType),
	).Replace(c.session.DownloadURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: build download request: %w", err)
	}
	resp, err := c.streamHC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: GET %s: %w", u, err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("jmapclient: GET %s: %w", u, ErrAuth)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &ServerError{Status: resp.StatusCode}
	}
	return resp.Body, nil
}
