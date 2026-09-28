package discover

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
)

// liveTargets pairs each configured live account's username with the
// server it is expected to be discovered on. Discovery itself never uses
// the password: it is DNS plus one unauthenticated GET (FR-A7).
func liveTargets(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, key := range []string{"JMAP_TUI_TEST_USER", "JMAP_TUI_TEST_USER_2"} {
		if u := os.Getenv(key); u != "" {
			out = append(out, u)
		}
	}
	// The configured server hosts exercise the domain-fallback path:
	// api.fastmail.com / mail.geekify.me publish no _jmap._tcp record of
	// their own, so only the bare-domain probe can find them.
	for _, key := range []string{"JMAP_TUI_TEST_URL", "JMAP_TUI_TEST_URL_2"} {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		out = append(out, "probe@"+strings.ToLower(parsed.Hostname()))
	}
	if len(out) == 0 {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER …)")
	}
	return out
}

// TestDiscoverLive is the M10 live gate for the probe chain: every
// configured account's address must resolve to a verified server URL, and
// a .invalid domain must fail with the actionable both-attempts error.
// Read-only — DNS lookups and unauthenticated GETs (AGENTS.md rules).
func TestDiscoverLive(t *testing.T) {
	d := New(MaxTimeout)
	for _, email := range liveTargets(t) {
		t.Run("discover "+domainOf(t, email), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), MaxTimeout+2*time.Second)
			defer cancel()
			res, err := d.Discover(ctx, email)
			if err != nil {
				t.Fatalf("Discover(%q): %v", domainOf(t, email), err)
			}
			if err := config.ValidateServerURL(res.URL); err != nil {
				t.Errorf("discovered URL %q: %v", res.URL, err)
			}
			if res.SessionURL != "" {
				if err := config.ValidateServerURL(res.SessionURL); err != nil {
					t.Errorf("discovered session URL %q: %v", res.SessionURL, err)
				}
			}
			t.Logf("source=%s url=%s session_url=%q", res.Source, res.URL, res.SessionURL)
		})
	}

	t.Run("unreachable domain names both attempts", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), MaxTimeout+2*time.Second)
		defer cancel()
		// .invalid is reserved (RFC 2606) and can never resolve, so this
		// stays green however the public DNS landscape moves.
		_, err := d.Discover(ctx, "nobody@example.invalid")
		if err == nil {
			t.Fatal("a .invalid domain discovered a server")
		}
		for _, want := range []string{"couldn't find a JMAP server for example.invalid", "SRV _jmap._tcp.example.invalid", "https://example.invalid"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

// domainOf is the test-only domain readout for log lines.
func domainOf(t *testing.T, email string) string {
	t.Helper()
	d, err := Domain(email)
	if err != nil {
		t.Fatalf("Domain(%q): %v", email, err)
	}
	return d
}
