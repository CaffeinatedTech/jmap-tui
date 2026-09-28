package jmapclient

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	jmap "git.sr.ht/~rockorager/go-jmap"
)

// TestAuthTransportSchemes pins FR-A2's two schemes at the transport:
// HTTP Basic by default, Bearer when configured — and neither ever
// leaves the trusted origins (the F-1/F-2 origin gate covers both).
// Fastmail's JMAP API accepts Bearer only (verified live 2026-09-27).
func TestAuthTransportSchemes(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	key, ok := originKey(srv.URL)
	if !ok {
		t.Fatalf("originKey(%q)", srv.URL)
	}
	trusted := map[string]bool{key: true}
	req := func() *http.Request {
		r, err := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Default: HTTP Basic over username/password.
	if _, err := (authTransport{username: "u", password: "p", trusted: trusted}).RoundTrip(req()); err != nil {
		t.Fatalf("basic roundtrip: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	if got != want {
		t.Errorf("default scheme header = %q, want %q", got, want)
	}

	// Bearer: the password travels as a raw token, username unused.
	if _, err := (authTransport{password: "tok", bearer: true, trusted: trusted}).RoundTrip(req()); err != nil {
		t.Fatalf("bearer roundtrip: %v", err)
	}
	if got != "Bearer tok" {
		t.Errorf("bearer header = %q, want %q", got, "Bearer tok")
	}

	// Off-origin: no Authorization at all, either scheme (F-1/F-2).
	for _, bearer := range []bool{false, true} {
		if _, err := (authTransport{username: "u", password: "tok", bearer: bearer, trusted: map[string]bool{}}).RoundTrip(req()); err != nil {
			t.Fatalf("off-origin roundtrip: %v", err)
		}
		if got != "" {
			t.Errorf("off-origin request (bearer=%v) carried Authorization %q — must be stripped", bearer, got)
		}
	}
}

// TestTrustSessionOriginsEligibility pins which session-advertised
// endpoints join the credential trust anchor (F-2 revision): absolute
// https with a hostname — yes; cleartext, IP literals, unparseable — no.
func TestTrustSessionOriginsEligibility(t *testing.T) {
	c := New(Options{ServerURL: "https://mail.example.com", Username: "u", Password: "p"})
	c.trustSessionOrigins(&jmap.Session{
		APIURL:         "https://ams.example.com/jmap/api",
		UploadURL:      "https://ams.example.com/jmap/upload",
		DownloadURL:    "https://cdn.exampleusercontent.com/blob/{accountId}/{blobId}/{name}",
		EventSourceURL: "https://ams.example.com/jmap/event/{types}/{closeafter}/{ping}",
	})
	for _, k := range []string{"https://ams.example.com", "https://cdn.exampleusercontent.com"} {
		if !c.trusted[k] {
			t.Errorf("session-advertised %s not trusted", k)
		}
	}

	c2 := New(Options{ServerURL: "https://mail.example.com", Username: "u", Password: "p"})
	c2.trustSessionOrigins(&jmap.Session{
		APIURL:         "http://insecure.example.com/api", // cleartext
		UploadURL:      "https://10.0.0.8/upload",         // IP literal (SSRF shape)
		DownloadURL:    "https://[::1]:8443/download",     // loopback literal
		EventSourceURL: "not a url at all",                // unparseable
	})
	for _, k := range []string{"http://insecure.example.com", "https://10.0.0.8", "https://[::1]:8443"} {
		if c2.trusted[k] {
			t.Errorf("ineligible session endpoint %s joined the trust anchor", k)
		}
	}
	if k, ok := originKey("not a url at all"); ok && c2.trusted[k] {
		t.Error("unparseable session URL joined the trust anchor")
	}
}
