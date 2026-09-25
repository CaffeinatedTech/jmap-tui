package sync

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// flakyProvider fails the first `fails` Connect attempts, then delegates
// to the real client — the shape of a wrong password that gets fixed, or a
// server that is briefly down.
type flakyProvider struct {
	mail.Provider
	attempts atomic.Int32 // read by waitFor while the hub retries
	fails    int
}

func (f *flakyProvider) Connect(ctx context.Context) error {
	n := f.attempts.Add(1)
	if int(n) <= f.fails {
		return errors.New("auth rejected")
	}
	return f.Provider.Connect(ctx)
}

// newHubClient builds an unconnected client against a fresh mockjmap
// server holding the standard mailbox tree (the hub performs Connect).
func newHubClient(t *testing.T) *jmapclient.Client {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 3},
	})
	srv.SetEmails(emailFixtures())
	t.Cleanup(srv.Close)
	return jmapclient.New(jmapclient.Options{
		ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse",
	})
}

// shrinkBackoff pulls the retry pacing down so failure-path tests run in
// milliseconds instead of the production 1s→30s curve.
func shrinkBackoff(e *Engine) {
	e.liveCfg.backoffBase = 5 * time.Millisecond
}

// TestHubFailureIsolation proves one account's connect failure never
// blocks another: account "ok" loads while "flaky" is still retrying, and
// flaky recovers on its own (FR-A4, PLAN §4.3).
func TestHubFailureIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ok := newHubClient(t)
	if err := ok.Connect(ctx); err != nil {
		t.Fatalf("pre-connect: %v", err)
	}
	flaky := &flakyProvider{Provider: newHubClient(t), fails: 1}

	hub := NewHub()
	eOK := hub.Enroll("ok", "Work", ok, true, Config{})
	eFlaky := hub.Enroll("flaky", "Home", flaky, false, Config{})
	shrinkBackoff(eOK)
	shrinkBackoff(eFlaky)

	hub.StartAll(ctx)

	waitFor(t, 3*time.Second, func() bool {
		return len(eOK.Snapshot().Mailboxes) > 0
	})
	waitFor(t, 3*time.Second, func() bool {
		return flaky.attempts.Load() >= 2 && len(eFlaky.Snapshot().Mailboxes) > 0
	})
	if eFlaky.Snapshot().Status.LastError != "" {
		t.Fatalf("flaky recovered but status still errors: %q", eFlaky.Snapshot().Status.LastError)
	}
	if got := len(hub.Accounts()); got != 2 {
		t.Fatalf("accounts = %d, want 2", got)
	}
}

// TestHubConnectErrorSurfacesInStatus: an account that cannot connect
// reports the failure through its snapshot status (the status line and
// switcher read exactly this, FR-I5) while its neighbour stays healthy.
func TestHubConnectErrorSurfacesInStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ok := newHubClient(t)
	if err := ok.Connect(ctx); err != nil {
		t.Fatalf("pre-connect: %v", err)
	}
	dead := &flakyProvider{Provider: newHubClient(t), fails: 1 << 30}

	hub := NewHub()
	eOK := hub.Enroll("ok", "Work", ok, true, Config{})
	eDead := hub.Enroll("dead", "Broken", dead, false, Config{})
	shrinkBackoff(eOK)
	shrinkBackoff(eDead)

	hub.StartAll(ctx)

	waitFor(t, 3*time.Second, func() bool {
		return strings.Contains(eDead.Snapshot().Status.LastError, "connect:")
	})
	waitFor(t, 3*time.Second, func() bool {
		return len(eOK.Snapshot().Mailboxes) > 0
	})
	if eDead.Snapshot().Status.Mode != ModeConnecting {
		t.Fatalf("dead mode = %q, want connecting", eDead.Snapshot().Status.Mode)
	}
}

// TestHubEnrollIdempotentAndOrdered: re-enrolling is a no-op and Accounts
// keeps enrollment order (the switcher lists accounts in config order).
func TestHubEnrollIdempotentAndOrdered(t *testing.T) {
	hub := NewHub()
	a := hub.Enroll("a", "A", nil, true, Config{})
	again := hub.Enroll("a", "A", nil, true, Config{})
	if a != again {
		t.Fatal("re-enroll created a second engine")
	}
	hub.Enroll("b", "B", nil, true, Config{})
	accs := hub.Accounts()
	if len(accs) != 2 || accs[0].ID != "a" || accs[1].ID != "b" {
		t.Fatalf("accounts order wrong: %+v", accs)
	}
	if hub.Engine("nope") != nil {
		t.Fatal("unknown account returned an engine")
	}
}
