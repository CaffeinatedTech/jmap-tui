package discover

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- fakes (no test touches DNS or a real network) ---

type fakeSRV struct {
	records []*net.SRV
	err     error
	calls   int
	saw     string // domain the lookup was asked for
}

func (f *fakeSRV) LookupSRV(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	f.calls++
	f.saw = name
	if service != "jmap" || proto != "tcp" {
		return "", nil, fmt.Errorf("unexpected lookup %s.%s", service, proto)
	}
	return "", f.records, f.err
}

type probeAnswer struct {
	final string
	err   error
}

type fakeProber struct {
	answers map[string]probeAnswer
	calls   []string
}

func (f *fakeProber) Probe(_ context.Context, baseURL string) (string, error) {
	f.calls = append(f.calls, baseURL)
	a, ok := f.answers[baseURL]
	if !ok {
		return "", errors.New("no canned answer for " + baseURL)
	}
	return a.final, a.err
}

func srvRecord(target string, port uint16) *net.SRV {
	return &net.SRV{Target: target, Port: port, Priority: 0, Weight: 0}
}

func newChainTest(lookup SRVResolver, prober Prober) *Discoverer {
	return &Discoverer{Lookup: lookup, Prober: prober, Scheme: "https", Timeout: time.Second}
}

// --- Domain ---

func TestDomain(t *testing.T) {
	ok := []struct{ in, want string }{
		{"you@example.com", "example.com"},
		{"You@Example.COM", "example.com"},
		{"  you@example.com  ", "example.com"},
		{"you@example.com.", "example.com"},
		{"you@mail.example.co.uk", "mail.example.co.uk"},
		{"you@127.0.0.1:8080", "127.0.0.1:8080"},
		{`"weird local"@example.com`, "example.com"},
	}
	for _, c := range ok {
		got, err := Domain(c.in)
		if err != nil || got != c.want {
			t.Errorf("Domain(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"",
		"   ",
		"not-an-email",
		"@example.com",
		"you@",
		"you@   ",
		"you@exam ple.com",
		"you@exa/mple.com",
	}
	for _, in := range bad {
		if got, err := Domain(in); err == nil {
			t.Errorf("Domain(%q) = %q, nil; want an error", in, got)
		}
	}
}

// --- the probe chain ---

func TestDiscoverChain(t *testing.T) {
	sessErr := errors.New("connection refused")
	cases := []struct {
		name       string
		email      string
		lookup     *fakeSRV
		prober     *fakeProber
		wantURL    string
		wantSource string
		wantSess   string
		wantErr    []string // substrings the combined error must name
		wantCalls  []string // probe order
	}{
		{
			name:  "srv hit",
			email: "you@example.com",
			lookup: &fakeSRV{records: []*net.SRV{
				srvRecord("mail.example.com.", 443),
			}},
			prober: &fakeProber{answers: map[string]probeAnswer{
				"https://mail.example.com": {final: "https://mail.example.com/.well-known/jmap"},
			}},
			wantURL:    "https://mail.example.com",
			wantSource: "srv",
			wantCalls:  []string{"https://mail.example.com"},
		},
		{
			name:  "srv port other than 443 is kept",
			email: "you@example.com",
			lookup: &fakeSRV{records: []*net.SRV{
				srvRecord("mail.example.com.", 8443),
			}},
			prober: &fakeProber{answers: map[string]probeAnswer{
				"https://mail.example.com:8443": {final: "https://mail.example.com:8443/.well-known/jmap"},
			}},
			wantURL:    "https://mail.example.com:8443",
			wantSource: "srv",
		},
		{
			name:  "srv target is a trailing-dot FQDN, lowercased",
			email: "you@Example.com",
			lookup: &fakeSRV{records: []*net.SRV{
				srvRecord("MAIL.Example.COM.", 443),
			}},
			prober: &fakeProber{answers: map[string]probeAnswer{
				"https://mail.example.com": {final: "https://mail.example.com/.well-known/jmap"},
			}},
			wantURL:    "https://mail.example.com",
			wantSource: "srv",
		},
		{
			name:       "srv missing falls back to the domain",
			email:      "you@example.com",
			lookup:     &fakeSRV{err: &net.DNSError{Err: "no such host", Name: "_jmap._tcp.example.com", IsNotFound: true}},
			prober:     &fakeProber{answers: map[string]probeAnswer{"https://example.com": {final: "https://example.com/.well-known/jmap"}}},
			wantURL:    "https://example.com",
			wantSource: "domain",
		},
		{
			name:  "srv record present but probe fails, domain still tried",
			email: "you@example.com",
			lookup: &fakeSRV{records: []*net.SRV{
				srvRecord("stale.example.com.", 443),
			}},
			prober: &fakeProber{answers: map[string]probeAnswer{
				"https://stale.example.com": {err: sessErr},
				"https://example.com":       {final: "https://example.com/.well-known/jmap"},
			}},
			wantURL:    "https://example.com",
			wantSource: "domain",
			wantCalls:  []string{"https://stale.example.com", "https://example.com"},
		},
		{
			name:       "both attempts fail, the error names both",
			email:      "you@example.com",
			lookup:     &fakeSRV{err: errors.New("resolver unavailable")},
			prober:     &fakeProber{answers: map[string]probeAnswer{"https://example.com": {err: errors.New("HTTP 404")}}},
			wantErr:    []string{"couldn't find a JMAP server for example.com", "SRV _jmap._tcp.example.com", "resolver unavailable", "https://example.com", "HTTP 404"},
			wantCalls:  []string{"https://example.com"},
			wantSource: "",
		},
		{
			name:       "cross-origin session document becomes session_url",
			email:      "you@example.com",
			lookup:     &fakeSRV{records: []*net.SRV{srvRecord("www.example.com.", 443)}},
			prober:     &fakeProber{answers: map[string]probeAnswer{"https://www.example.com": {final: "https://session.example.net/jmap"}}},
			wantURL:    "https://www.example.com",
			wantSource: "srv",
			wantSess:   "https://session.example.net/jmap",
		},
		{
			name:       "same-origin move is not a session_url",
			email:      "you@example.com",
			lookup:     &fakeSRV{records: []*net.SRV{srvRecord("www.example.com.", 443)}},
			prober:     &fakeProber{answers: map[string]probeAnswer{"https://www.example.com": {final: "https://www.example.com/jmap/session"}}},
			wantURL:    "https://www.example.com",
			wantSource: "srv",
		},
		{
			name:       "cross-origin cleartext session is rejected",
			email:      "you@example.com",
			lookup:     &fakeSRV{records: []*net.SRV{srvRecord("mail.example.com.", 443)}},
			prober:     &fakeProber{answers: map[string]probeAnswer{"https://mail.example.com": {final: "http://plain.example.net/jmap"}}},
			wantErr:    []string{"session endpoint", "cleartext http"},
			wantSource: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newChainTest(c.lookup, c.prober)
			res, err := d.Discover(context.Background(), c.email)

			if len(c.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Discover = %+v, nil; want an error", res)
				}
				for _, want := range c.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if res.URL != c.wantURL || res.Source != c.wantSource || res.SessionURL != c.wantSess {
				t.Errorf("Result = %+v; want URL %q Source %q SessionURL %q", res, c.wantURL, c.wantSource, c.wantSess)
			}
			if c.wantCalls != nil && strings.Join(c.prober.calls, "\n") != strings.Join(c.wantCalls, "\n") {
				t.Errorf("probe calls = %v; want %v", c.prober.calls, c.wantCalls)
			}
		})
	}
}

func TestDiscoverRejectsNonEmailBeforeAnyNetwork(t *testing.T) {
	prober := &fakeProber{answers: map[string]probeAnswer{}}
	lookup := &fakeSRV{}
	d := newChainTest(lookup, prober)
	if _, err := d.Discover(context.Background(), "server-login"); err == nil {
		t.Fatal("non-email input accepted")
	}
	if lookup.calls != 0 || len(prober.calls) != 0 {
		t.Errorf("lookup calls = %d, probe calls = %v; want no network at all", lookup.calls, prober.calls)
	}
}

func TestDiscoverSRVLookupSeesTheLowercasedDomain(t *testing.T) {
	lookup := &fakeSRV{err: errors.New("nx")}
	prober := &fakeProber{answers: map[string]probeAnswer{"https://example.com": {final: "https://example.com/.well-known/jmap"}}}
	d := newChainTest(lookup, prober)
	if _, err := d.Discover(context.Background(), "You@Example.COM"); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if lookup.saw != "example.com" {
		t.Errorf("SRV lookup asked for %q, want example.com", lookup.saw)
	}
	if got := lookup.calls; got != 1 {
		t.Errorf("SRV lookups = %d, want 1", got)
	}
}

func TestDiscoverWithoutSRVResolverGoesStraightToTheDomain(t *testing.T) {
	prober := &fakeProber{answers: map[string]probeAnswer{"https://example.com": {final: "https://example.com/.well-known/jmap"}}}
	d := &Discoverer{Prober: prober, Scheme: "https", Timeout: time.Second}
	res, err := d.Discover(context.Background(), "you@example.com")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Source != "domain" {
		t.Errorf("Source = %q, want domain", res.Source)
	}
	if len(prober.calls) != 1 || prober.calls[0] != "https://example.com" {
		t.Errorf("calls = %v", prober.calls)
	}
}

func TestDiscoverRequiresAProber(t *testing.T) {
	d := &Discoverer{Lookup: &fakeSRV{}}
	if _, err := d.Discover(context.Background(), "you@example.com"); err == nil {
		t.Fatal("nil prober accepted")
	}
}

func TestDiscoverTimeoutIsCapped(t *testing.T) {
	cases := []struct {
		asked time.Duration
		want  time.Duration
	}{
		{0, MaxTimeout},
		{-1, MaxTimeout},
		{2 * time.Second, 2 * time.Second},
		{time.Hour, MaxTimeout},
	}
	for _, c := range cases {
		d := &Discoverer{Timeout: c.asked}
		if got := d.timeout(); got != c.want {
			t.Errorf("timeout(%s) = %s, want %s", c.asked, got, c.want)
		}
	}
}

// --- the HTTP prober (httptest, loopback http) ---

func probeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jmap" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// portOf extracts the port of an httptest URL, so a fake SRV record can
// point at it the way a real one points at a host.
func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", raw, err)
	}
	return p
}

const sessionBody = `{"capabilities":{"urn:ietf:params:jmap:core":{}},"apiUrl":"https://mail.example.com/api"}`

func TestHTTPProberAcceptance(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "session document", status: 200, body: sessionBody},
		{name: "capabilities only", status: 200, body: `{"capabilities":{"urn:ietf:params:jmap:mail":{}}}`},
		{name: "apiUrl only", status: 200, body: `{"apiUrl":"https://mail.example.com/api"}`},
		{name: "unauthorized means the host is right", status: 401, body: "No Authorization header"},
		{name: "forbidden is also the host", status: 403, body: "forbidden"},
		{name: "not found", status: 404, body: "nope", wantErr: "HTTP 404"},
		{name: "server error", status: 500, body: "boom", wantErr: "HTTP 500"},
		{name: "200 but not a session", status: 200, body: `{"hello":"world"}`, wantErr: "not a JMAP session document"},
		{name: "200 but not JSON", status: 200, body: "<html>welcome</html>", wantErr: "not a JMAP session document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := probeServer(t, c.status, c.body)
			final, err := HTTPProber{}.Probe(context.Background(), srv.URL)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("Probe = %q, nil; want error %q", final, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error %q does not contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if want := srv.URL + "/.well-known/jmap"; final != want {
				t.Errorf("final = %q, want %q", final, want)
			}
		})
	}
}

func TestHTTPProberFollowsRedirects(t *testing.T) {
	// The shape every real server has: /.well-known/jmap 30x to the
	// session resource, which answers with the document.
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/jmap":
			http.Redirect(w, r, "/jmap/session", http.StatusTemporaryRedirect)
		case "/jmap/session":
			_, _ = w.Write([]byte(sessionBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(moved.Close)

	final, err := HTTPProber{}.Probe(context.Background(), moved.URL)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := moved.URL + "/jmap/session"; final != want {
		t.Errorf("final = %q, want %q (the probe reports where the document lives)", final, want)
	}
}

func TestHTTPProberMalformedBase(t *testing.T) {
	for _, base := range []string{"", "not a url", "ftp://mail.example.com", "https://"} {
		if _, err := (HTTPProber{}).Probe(context.Background(), base); err == nil {
			t.Errorf("Probe(%q) accepted; want an error", base)
		}
	}
}

// TestDiscoverRecordsCrossOriginSessionURL drives the whole chain over
// loopback http (Scheme override): candidate A redirects to candidate B,
// so the result carries B as session_url while url stays on A.
func TestDiscoverRecordsCrossOriginSessionURL(t *testing.T) {
	session := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jmap/session" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sessionBody))
	}))
	t.Cleanup(session.Close)

	candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jmap" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, session.URL+"/jmap/session", http.StatusFound)
	}))
	t.Cleanup(candidate.Close)

	port := portOf(t, candidate.URL)
	lookup := &fakeSRV{records: []*net.SRV{srvRecord("127.0.0.1", uint16(port))}}
	d := &Discoverer{Lookup: lookup, Prober: HTTPProber{}, Scheme: "http", Timeout: time.Second}

	// The domain part only sets the SRV name here; the target carries the
	// port, exactly as a real _jmap._tcp record would.
	res, err := d.Discover(context.Background(), "you@example.com")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Source != "srv" {
		t.Errorf("Source = %q, want srv", res.Source)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", port); res.URL != want {
		t.Errorf("URL = %q, want %q", res.URL, want)
	}
	if want := session.URL + "/jmap/session"; res.SessionURL != want {
		t.Errorf("SessionURL = %q, want %q", res.SessionURL, want)
	}
}

// TestDiscoverDomainFallbackOverLoopback covers the no-SRV path against a
// live httptest server: the email's own domain is probed and the session
// document verifies.
func TestDiscoverDomainFallbackOverLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jmap" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sessionBody))
	}))
	t.Cleanup(srv.Close)

	port := portOf(t, srv.URL)
	lookup := &fakeSRV{err: &net.DNSError{Err: "no such host", Name: "_jmap._tcp.test", IsNotFound: true}}
	d := &Discoverer{Lookup: lookup, Prober: HTTPProber{}, Scheme: "http", Timeout: time.Second}
	res, err := d.Discover(context.Background(), "you@127.0.0.1:"+fmt.Sprint(port))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.Source != "domain" {
		t.Errorf("Source = %q, want domain", res.Source)
	}
	if res.URL != srv.URL {
		t.Errorf("URL = %q, want %q", res.URL, srv.URL)
	}
	if res.SessionURL != "" {
		t.Errorf("SessionURL = %q, want empty (same origin)", res.SessionURL)
	}
}

// TestRedirectPolicy pins the three stops directly: no hop limit run-over,
// no scheme change, no credentials in the target.
func TestRedirectPolicy(t *testing.T) {
	policy := redirectPolicy("https")

	to := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	first := to("https://mail.example.com/.well-known/jmap")

	if err := policy(to("https://session.example.com/jmap"), []*http.Request{first}); err != nil {
		t.Errorf("same-scheme cross-origin redirect refused: %v", err)
	}
	if err := policy(to("http://mail.example.com/x"), []*http.Request{first}); err == nil {
		t.Error("https → http downgrade accepted")
	}
	if err := policy(to("https://user:pass@mail.example.com/x"), []*http.Request{first}); err == nil {
		t.Error("redirect with embedded credentials accepted")
	}
	via := make([]*http.Request, maxProbeRedirects)
	for i := range via {
		via[i] = first
	}
	if err := policy(to("https://mail.example.com/again"), via); err == nil {
		t.Error("redirect loop accepted")
	}
}
