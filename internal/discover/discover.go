// Package discover implements JMAP server auto-configuration (FR-A7):
// given the email address an account will use, it finds and verifies the
// server that account lives on, so the wizard can ask for an address
// instead of a URL.
//
// The probe chain follows RFC 8620 §2.2. A JMAP host for example.com
// SHOULD publish _jmap._tcp.example.com giving a hostname and port, and a
// client MAY use the domain part of an email-shaped username to attempt
// discovery — so the SRV record is tried first and the bare domain is the
// fallback (a Stalwart box behind a domain with no SRV record answers the
// second probe). Both candidates are verified with a single unauthenticated
// GET of /.well-known/jmap: discovery never carries credentials, and the
// credential test that follows decides whether the server actually accepts
// this account.
package discover

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
)

// MaxTimeout is the ceiling on one Discover call, whatever the caller
// asks for: the wizard runs it on Enter, so an offline resolver must cost
// seconds, not the connection test's full budget (and never hang).
const MaxTimeout = 10 * time.Second

// SRVResolver looks up DNS SRV records. *net.Resolver satisfies it, and
// tests substitute a fake so no unit test touches the network.
type SRVResolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// Prober verifies one candidate base URL with an unauthenticated request
// and returns the URL the probe finished on — redirects followed, so a
// server that moves /.well-known/jmap elsewhere reports where the session
// document actually lives.
type Prober interface {
	Probe(ctx context.Context, baseURL string) (finalURL string, err error)
}

// Result is a verified discovery.
type Result struct {
	// URL is the candidate base the probe succeeded against — what the
	// account's `url` becomes.
	URL string

	// SessionURL is set when the session document answered from another
	// origin than URL: the client refuses cross-origin redirects, so that
	// endpoint is recorded as `session_url` (the config key and the
	// client's trust anchor already exist for it). Same-origin answers
	// leave it empty — the client follows those itself.
	SessionURL string

	// Source is how the candidate was found: "srv" or "domain".
	Source string
}

// Discoverer runs the probe chain. Lookup, Prober and Scheme are wired by
// New; tests build one directly with fakes. A zero Scheme means https —
// "http" exists only for loopback tests, mirroring the URL policy in
// config.ValidateServerURL.
type Discoverer struct {
	Lookup  SRVResolver   // nil skips the SRV step
	Prober  Prober        // required
	Scheme  string        // "" → https
	Timeout time.Duration // whole-run budget, capped at MaxTimeout
}

// New returns a Discoverer wired to the stdlib resolver and the HTTP
// prober, bounded by min(timeout, MaxTimeout).
func New(timeout time.Duration) *Discoverer {
	if timeout <= 0 || timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	return &Discoverer{
		Lookup:  net.DefaultResolver,
		Prober:  HTTPProber{},
		Timeout: timeout,
	}
}

// Domain extracts the discovery domain from an email-shaped username:
// the text after the last "@", lowercased and stripped of a trailing dot
// (DNS names are case-insensitive FQDNs). Input that is not email-shaped
// is an error, and the caller then offers the Server URL field instead —
// a bare server username is legitimate (FR-A2), it just cannot be
// discovered from.
func Domain(email string) (string, error) {
	s := strings.TrimSpace(email)
	at := strings.LastIndex(s, "@")
	if at < 0 || at == len(s)-1 || at == 0 {
		return "", errors.New("not an email address — expected something like you@example.com")
	}
	domain := strings.ToLower(strings.TrimSpace(s[at+1:]))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return "", errors.New("not an email address — expected something like you@example.com")
	}
	for _, r := range domain {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':':
		default:
			return "", errors.New("not an email address — expected something like you@example.com")
		}
	}
	return domain, nil
}

// Discover finds and verifies the JMAP server for email. The first
// candidate that verifies wins; when neither does, the error names both
// attempts so the user can see which half failed (DNS or HTTP) before
// reaching for the Server URL field.
func (d *Discoverer) Discover(ctx context.Context, email string) (Result, error) {
	domain, err := Domain(email)
	if err != nil {
		return Result{}, err
	}
	if d.Prober == nil {
		return Result{}, errors.New("discover: no prober configured")
	}
	scheme := d.Scheme
	if scheme == "" {
		scheme = "https"
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()

	var srvErr error
	if d.Lookup != nil {
		res, err := d.fromSRV(ctx, scheme, domain)
		if err == nil {
			return res, nil
		}
		srvErr = err
	}
	res, err := d.fromDomain(ctx, scheme, domain)
	if err == nil {
		return res, nil
	}
	if srvErr != nil {
		return Result{}, fmt.Errorf("couldn't find a JMAP server for %s: %w; %w", domain, srvErr, err)
	}
	return Result{}, fmt.Errorf("couldn't find a JMAP server for %s: %w", domain, err)
}

// fromSRV probes the host published in _jmap._tcp.<domain>. The lowest
// priority wins (RFC 2782); among equals the resolver's own order stands —
// *net.Resolver shuffles those by weight for us. Remaining targets are
// left alone rather than guessed at: a stale secondary is exactly what the
// domain fallback, and then the manual Server URL field, are for.
func (d *Discoverer) fromSRV(ctx context.Context, scheme, domain string) (Result, error) {
	_, records, err := d.Lookup.LookupSRV(ctx, "jmap", "tcp", domain)
	if err != nil {
		return Result{}, fmt.Errorf("SRV _jmap._tcp.%s: %w", domain, err)
	}
	if len(records) == 0 {
		return Result{}, fmt.Errorf("SRV _jmap._tcp.%s: no records", domain)
	}
	target := records[0]
	for _, r := range records[1:] {
		if r.Priority < target.Priority {
			target = r
		}
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target.Target)), ".")
	if host == "" {
		return Result{}, errors.New("SRV record has an empty target")
	}
	base := scheme + "://" + host
	if port := target.Port; port != 0 && port != defaultPort(scheme) {
		base = fmt.Sprintf("%s://%s:%d", scheme, host, port)
	}
	res, err := d.verify(ctx, base, "srv")
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", base, err)
	}
	return res, nil
}

// fromDomain probes the email's own domain — the RFC 8620 §2.2 MAY path,
// and the one that carries a bare Stalwart install with no SRV record.
func (d *Discoverer) fromDomain(ctx context.Context, scheme, domain string) (Result, error) {
	base := scheme + "://" + domain
	res, err := d.verify(ctx, base, "domain")
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", base, err)
	}
	return res, nil
}

// verify runs the probe against base and packages a hit as a Result. Both
// URLs leave through config.ValidateServerURL, so what discovery hands
// back is already legal for the account to be written with.
func (d *Discoverer) verify(ctx context.Context, base, source string) (Result, error) {
	final, err := d.Prober.Probe(ctx, base)
	if err != nil {
		return Result{}, err
	}
	res := Result{URL: base, Source: source}
	if !sameOrigin(base, final) {
		res.SessionURL = final
	}
	if err := config.ValidateServerURL(res.URL); err != nil {
		return Result{}, err
	}
	if res.SessionURL != "" {
		if err := config.ValidateServerURL(res.SessionURL); err != nil {
			return Result{}, fmt.Errorf("session endpoint: %w", err)
		}
	}
	return res, nil
}

// timeout is the whole-run budget: whatever the caller asked for, capped
// at MaxTimeout, defaulting to it when unset.
func (d *Discoverer) timeout() time.Duration {
	if d.Timeout <= 0 || d.Timeout > MaxTimeout {
		return MaxTimeout
	}
	return d.Timeout
}

// defaultPort is the port the scheme implies and a base URL may omit.
func defaultPort(scheme string) uint16 {
	if scheme == "http" {
		return 80
	}
	return 443
}

// sameOrigin reports whether a and b are the same origin: scheme, host
// (case-insensitive) and effective port, with the scheme's default port
// folded away. Unparseable URLs are never same-origin, so a redirect target
// that cannot be read is recorded — and rejected — as a cross-origin one.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return originKey(ua) == originKey(ub) && ua.Scheme != "" && ub.Scheme != ""
}

// originKey normalizes u to scheme://host[:port] with default ports
// dropped and everything lowercased — the same fold jmapclient uses for
// its credential trust decisions.
func originKey(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}
