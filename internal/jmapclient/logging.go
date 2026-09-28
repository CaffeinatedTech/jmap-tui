package jmapclient

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// loggingTransport wraps the auth transport with structured request logs.
// It records method, URL, status, and duration only — never headers,
// bodies, or credentials (FR-K2, NFR-5). JMAP URLs normally carry no
// secret material: Basic auth lives in the Authorization header, which is
// not logged — but a hand-edited config may put user:pass@ into the
// server URL, so userinfo is stripped before logging.
type loggingTransport struct {
	next   http.RoundTripper
	logger *slog.Logger
}

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	fields := []any{
		"method", req.Method,
		"url", logURL(req.URL),
		"duration_ms", time.Since(start).Milliseconds(),
	}
	if err != nil {
		fields = append(fields, "error", err.Error())
		t.logger.Error("jmap request failed", fields...)
		return resp, err
	}
	fields = append(fields, "status", resp.StatusCode)
	t.logger.Info("jmap request", fields...)
	return resp, nil
}

// logURL renders u for the log with any userinfo removed — the request's
// URL is never mutated, only a stripped copy for the log line.
func logURL(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	c := *u
	c.User = nil
	return c.String()
}
