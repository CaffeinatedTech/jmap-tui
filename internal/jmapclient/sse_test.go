package jmapclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

func TestExpandEventSourceURL(t *testing.T) {
	cases := []struct {
		name, tmpl, want string
	}{
		{
			name: "path placeholders (RFC 8620 §7.3, Stalwart shape)",
			tmpl: "https://srv/jmap/event/{types}/{closeafter}/{ping}",
			want: "https://srv/jmap/event/*/no/30",
		},
		{
			name: "percent-encoded placeholders",
			tmpl: "https://srv/jmap/event/%7Btypes%7D/%7Bcloseafter%7D/%7Bping%7D",
			want: "https://srv/jmap/event/*/no/30",
		},
		{
			name: "no placeholders falls back to query parameters",
			tmpl: "https://srv/jmap/event",
			want: "https://srv/jmap/event?types=*&closeafter=no&ping=30",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandEventSourceURL(tc.tmpl, 30); got != tc.want {
				t.Fatalf("expand = %q, want %q", got, tc.want)
			}
		})
	}
}

// newStreamClient connects a client to the mock with the session fetched.
func newStreamClient(t *testing.T) (*Client, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
	})
	t.Cleanup(srv.Close)
	c := New(Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, srv
}

func TestSubscribeReceivesStateChange(t *testing.T) {
	c, srv := newStreamClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, stop := c.Subscribe(ctx)
	if ch == nil {
		t.Fatal("mock advertises eventSourceUrl; Subscribe returned nil channel")
	}
	defer func() { _ = stop() }()

	srv.Notify()
	select {
	case change, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before any event")
		}
		states, ok := change.Changed["acc1"]
		if !ok {
			t.Fatalf("change missing acc1: %+v", change.Changed)
		}
		if states["Email"] == "" || states["Mailbox"] == "" {
			t.Fatalf("state strings missing: %+v", states)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no state change within 2s")
	}
}

func TestSubscribeChannelClosesOnDrop(t *testing.T) {
	c, srv := newStreamClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := c.Subscribe(ctx)
	srv.Notify() // ensure the stream is live
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never delivered")
	}

	srv.DropStreams()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel close, got a value")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after stream drop")
	}
}

func TestSubscribeStopsWithContext(t *testing.T) {
	c, _ := newStreamClient(t)
	ctx, cancel := context.WithCancel(context.Background())

	ch, _ := c.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel close on ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after cancel")
	}
}

func TestSubscribeConnectFailureClosesChannel(t *testing.T) {
	c, srv := newStreamClient(t)
	srv.FailStreams(1)

	ch, _ := c.Subscribe(context.Background())
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("failed connect must close the channel empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed connect did not close the channel")
	}
}

func TestEmailChangesReplaysJournal(t *testing.T) {
	c, srv := newStreamClient(t)
	ctx := context.Background()

	before, err := c.EmailChanges(ctx, "e-1")
	if err != nil {
		t.Fatalf("EmailChanges(initial): %v", err)
	}
	if len(before.Updated) != 0 || len(before.Destroyed) != 0 {
		t.Fatalf("fresh journal not empty: %+v", before)
	}

	srv.CreateEmails([]mockjmap.Email{{
		ID: "e9", ThreadID: "t9", MailboxIDs: []string{"mb-inbox"},
		Subject: "new arrival",
	}})
	srv.UpdateEmails([]string{"e9"}, func(e *mockjmap.Email) { e.Subject = "edited" })

	set, err := c.EmailChanges(ctx, before.NewState)
	if err != nil {
		t.Fatalf("EmailChanges: %v", err)
	}
	if len(set.Updated) != 1 || set.Updated[0] != "e9" {
		t.Fatalf("updated = %v, want [e9]", set.Updated)
	}
	if set.NewState == before.NewState {
		t.Fatal("newState did not advance")
	}

	srv.DestroyEmails([]string{"e9"})
	destroyed, err := c.EmailChanges(ctx, set.NewState)
	if err != nil {
		t.Fatalf("EmailChanges(destroy): %v", err)
	}
	if len(destroyed.Destroyed) != 1 || destroyed.Destroyed[0] != "e9" {
		t.Fatalf("destroyed = %v, want [e9]", destroyed.Destroyed)
	}
}

func TestMailboxChangesReplaysJournal(t *testing.T) {
	c, srv := newStreamClient(t)
	ctx := context.Background()

	before, err := c.MailboxChanges(ctx, "m-1")
	if err != nil {
		t.Fatalf("MailboxChanges: %v", err)
	}
	srv.SetMailboxes([]mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, UnreadEmails: 4},
	})

	set, err := c.MailboxChanges(ctx, before.NewState)
	if err != nil {
		t.Fatalf("MailboxChanges: %v", err)
	}
	found := false
	for _, id := range set.Updated {
		if id == "mb-inbox" {
			found = true
		}
	}
	if !found {
		t.Fatalf("updated = %v, want mb-inbox", set.Updated)
	}
}

func TestChangesCannotCalculateIsSentinel(t *testing.T) {
	c, srv := newStreamClient(t)
	srv.SetCannotCalculateChanges(true)

	_, err := c.EmailChanges(context.Background(), "e-1")
	if !errors.Is(err, mail.ErrCannotCalculateChanges) {
		t.Fatalf("EmailChanges err = %v, want ErrCannotCalculateChanges", err)
	}
	_, err = c.MailboxChanges(context.Background(), "m-1")
	if !errors.Is(err, mail.ErrCannotCalculateChanges) {
		t.Fatalf("MailboxChanges err = %v, want ErrCannotCalculateChanges", err)
	}
}
