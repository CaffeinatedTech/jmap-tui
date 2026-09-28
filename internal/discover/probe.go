package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxProbeBody caps how much of a probe response is read: a discovery
// answer is a few kilobytes of JSON, and anything larger is not one.
const maxProbeBody = 1 << 20

// maxProbeRedirects reproduces http.Client's default so a same-origin
// redirect loop still fails fast.
const maxProbeRedirects = 10

// HTTPProber verifies a candidate base URL with one unauthenticated GET of
// <base>/.well-known/jmap.
//
// Acceptance is deliberately loose: 200 with session-shaped JSON (a
// capabilities map or apiUrl — RFC 8620 §2), or 401/403, which means the
// host is right and wants credentials — the credential test that follows
// does the real auth. Anything else is a miss. The probe never carries
// credentials and never echoes the response body: it is server-supplied
// text headed for the terminal (FR-K2).
type HTTPProber struct{}

// Probe implements Prober.
func (HTTPProber) Probe(ctx context.Context, baseURL string) (string, error) {
	raw := strings.TrimRight(baseURL, "/") + "/.well-known/jmap"
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("malformed discovery URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported discovery scheme %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{CheckRedirect: redirectPolicy(u.Scheme)}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	final := resp.Request.URL.String()

	switch resp.StatusCode {
	case http.StatusOK:
		var session struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
			APIURL       string                     `json:"apiUrl"`
		}
		if err := json.Unmarshal(body, &session); err != nil {
			return "", fmt.Errorf("not a JMAP session document: %w", err)
		}
		if len(session.Capabilities) == 0 && session.APIURL == "" {
			return "", errors.New("not a JMAP session document (no capabilities or apiUrl)")
		}
		return final, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return final, nil
	default:
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
}

// redirectPolicy follows redirects inside the probe — including
// cross-origin ones, whose final URL Discover records as session_url —
// with three stops: the 10-hop cap above, no scheme change (an https
// candidate can never be talked into cleartext), and no credentials in
// the target. It is the unauthenticated counterpart of jmapclient's
// refuseCrossOriginRedirects: nothing is at risk but the GET itself until
// a verified URL is written to config.
func redirectPolicy(scheme string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxProbeRedirects {
			return errors.New("stopped after 10 redirects")
		}
		if !strings.EqualFold(req.URL.Scheme, scheme) {
			return fmt.Errorf("refusing %s to %s redirect", scheme, req.URL.Scheme)
		}
		if req.URL.User != nil {
			return errors.New("refusing redirect with embedded credentials")
		}
		return nil
	}
}
