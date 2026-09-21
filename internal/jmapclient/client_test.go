package jmapclient

import (
	"context"
	"errors"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

func fixtures() []mockjmap.Mailbox {
	return []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 1234, UnreadEmails: 5},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive", SortOrder: 2, TotalEmails: 3, UnreadEmails: 1},
	}
}

const testPassword = "correct-horse"

func newTestClient(t *testing.T, password string) (*Client, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", testPassword, fixtures())
	t.Cleanup(srv.Close)
	c := New(Options{
		ServerURL: srv.URL(),
		Username:  "tester@example.com",
		Password:  password,
	})
	return c, srv
}

func TestConnectAndSessionInfo(t *testing.T) {
	c, _ := newTestClient(t, testPassword)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := c.SessionInfo()
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	if info.PrimaryMailAccount != "acc1" {
		t.Errorf("PrimaryMailAccount = %q, want acc1", info.PrimaryMailAccount)
	}
	if len(info.Accounts) != 1 || info.Accounts[0].Name != "tester@example.com" {
		t.Errorf("Accounts = %+v", info.Accounts)
	}
	if !containsString(info.Capabilities, "urn:ietf:params:jmap:mail") {
		t.Errorf("Capabilities = %v, want mail capability", info.Capabilities)
	}
	if info.APIURL == "" || info.EventSourceURL == "" {
		t.Errorf("URLs not populated: %+v", info)
	}
}

func TestConnectBadPassword(t *testing.T) {
	c, _ := newTestClient(t, "wrong-password")
	err := c.Connect(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestConnectRequiresServerURL(t *testing.T) {
	c := New(Options{})
	err := c.Connect(context.Background())
	if err == nil || !errors.Is(err, ErrNoServerURL) {
		t.Fatalf("err = %v, want errNoServerURL", err)
	}
}

func TestSessionInfoBeforeConnect(t *testing.T) {
	c := New(Options{})
	if _, err := c.SessionInfo(); err == nil {
		t.Fatal("want error before Connect")
	}
}

func TestMailboxes(t *testing.T) {
	c, _ := newTestClient(t, testPassword)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if len(mbs) != 3 {
		t.Fatalf("got %d mailboxes, want 3", len(mbs))
	}
	// Server sort order (sortOrder asc) must survive conversion.
	if mbs[0].ID != "mb-inbox" || mbs[1].ID != "mb-archive" || mbs[2].ID != "mb-agent" {
		t.Errorf("order = %v, %v, %v", mbs[0].ID, mbs[1].ID, mbs[2].ID)
	}
	inbox := mbs[0]
	if inbox.Role != "inbox" || inbox.UnreadEmails != 5 || inbox.TotalEmails != 1234 {
		t.Errorf("inbox = %+v", inbox)
	}
	if mbs[2].ParentID != "mb-archive" {
		t.Errorf("agent-test ParentID = %q, want mb-archive", mbs[2].ParentID)
	}
}

func TestMailboxesBeforeConnect(t *testing.T) {
	c := New(Options{})
	if _, err := c.Mailboxes(context.Background()); err == nil {
		t.Fatal("want error before Connect")
	}
}

func TestUnimplementedMethodsAreTyped(t *testing.T) {
	c, _ := newTestClient(t, testPassword)
	ctx := context.Background()
	if err := c.Mutate(ctx, mail.Mutation{}); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("Mutate err = %v", err)
	}
	if _, err := c.Send(ctx, mail.Draft{}); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("Send err = %v", err)
	}
	if _, stop := c.Subscribe(ctx); stop == nil {
		t.Error("Subscribe stop func = nil")
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
