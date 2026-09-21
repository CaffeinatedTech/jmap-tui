package jmapclient

import (
	"log/slog"
	"net/http"
	"time"
)

// loggingTransport wraps the auth transport with structured request logs.
// It records method, URL, status, and duration only — never headers,
// bodies, or credentials (FR-K2, NFR-5). JMAP URLs carry no secret
// material: Basic auth lives in the Authorization header, which is not
// logged.
type loggingTransport struct {
	next   http.RoundTripper
	logger *slog.Logger
}

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	fields := []any{
		"method", req.Method,
		"url", req.URL.String(),
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
