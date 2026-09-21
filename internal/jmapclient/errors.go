package jmapclient

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrAuth wraps 401/403 responses so callers can surface a re-auth prompt
// instead of a crash (FR-A3).
var ErrAuth = errors.New("authentication failed (check username and app password)")

// ErrNoServerURL is returned by Connect when neither ServerURL nor
// SessionURL is configured.
var ErrNoServerURL = errors.New("no server URL configured")

// ErrUnimplemented marks provider methods whose milestone has not landed.
var ErrUnimplemented = errors.New("jmapclient: method not implemented in this milestone")

// ErrNotFound wraps a server notFound response for a requested object id.
var ErrNotFound = errors.New("object not found on server")

// ServerError is a non-2xx HTTP response from the server that is not an auth
// failure. Detail holds the (possibly empty) response body; it never
// contains credentials.
type ServerError struct {
	Status int
	Detail string
}

func (e *ServerError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("jmapclient: server error: HTTP %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("jmapclient: server error: HTTP %d", e.Status)
}

// MethodCallError is a per-method "error" invocation from the server, e.g.
// unknownMethod, invalidArguments, or serverFail.
type MethodCallError struct {
	Type        string
	Description string
}

func (e *MethodCallError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("jmapclient: %s: %s", e.Type, e.Description)
	}
	return fmt.Sprintf("jmapclient: %s", e.Type)
}

// basicAuthTransport applies HTTP Basic credentials to every request so that
// session discovery, API calls, and later upload/download/EventSource all
// authenticate uniformly (FR-A2). Credentials are never logged.
type basicAuthTransport struct {
	base     http.RoundTripper
	username string
	password string
}

func (t basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.SetBasicAuth(t.username, t.password)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
