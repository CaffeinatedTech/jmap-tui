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

// ErrNotFound wraps a server notFound response for a requested object id.
var ErrNotFound = errors.New("object not found on server")

// ErrNotSupported wraps an operation the connected server never advertised
// as a capability (FR-A6). Callers degrade — hide the feature or report it
// as unavailable — never treat it as a failure.
var ErrNotSupported = errors.New("server does not support this capability")

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

// basicAuthTransport applies HTTP Basic credentials to every request for a
// configured origin so that session discovery, API calls, and later
// upload/download/EventSource all authenticate uniformly (FR-A2).
// Credentials are never logged, and they never leave the origins the
// user configured: session resources may advertise
// apiUrl/downloadUrl/uploadUrl/eventSourceUrl on another host, redirects
// may point anywhere, and a hostile server gets a 401 at worst — never a
// credential. Off-origin requests leave with no Authorization header even
// if another layer attached one.
type basicAuthTransport struct {
	base     http.RoundTripper
	username string
	password string

	// trusted holds normalized origins (scheme://host[:port]) derived from
	// the user-configured ServerURL/SessionURL — the trust anchor. An
	// empty map means no origin was parseable: no request authenticates
	// (fail closed) instead of guessing.
	trusted map[string]bool
}

func (t basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.trusted[originKeyURL(req.URL)] {
		clone.SetBasicAuth(t.username, t.password)
	} else {
		clone.Header.Del("Authorization")
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
