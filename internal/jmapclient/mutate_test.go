package jmapclient

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// newMutateClient connects a client to a server seeded with one fixture
// email (read, in mb-agent) plus a trash/archive pair to move into.
func newMutateClient(t *testing.T) (*Client, *mockjmap.Server) {
	t.Helper()
	c, srv := newTestClient(t, testPassword)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	received := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	srv.SetEmails([]mockjmap.Email{{
		ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-agent"},
		Keywords:    map[string]bool{"$seen": true},
		From:        []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
		Subject:     "fixture",
		ReceivedAt:  received,
		TextBody:    "hello\n",
		Attachments: []mockjmap.Attachment{{BlobID: "b1", Name: "notes.txt", Type: "text/plain", Size: 5}},
	}})
	srv.SetBlob("b1", []byte("hello"))
	return c, srv
}

func TestMutateKeywordsAndMove(t *testing.T) {
	c, _ := newMutateClient(t)
	ctx := context.Background()

	res, err := c.Mutate(ctx, mail.Mutation{Emails: map[mail.ID]mail.EmailPatch{
		"e1": {
			SetKeywords:     map[string]bool{"$flagged": true, "$seen": false},
			RemoveMailboxes: []mail.ID{"mb-agent"},
			AddMailboxes:    []mail.ID{"mb-archive"},
		},
	}})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "e1" {
		t.Fatalf("Updated = %v, want [e1]", res.Updated)
	}
	if len(res.NotUpdated) != 0 {
		t.Fatalf("NotUpdated = %v", res.NotUpdated)
	}
	if res.NewState == "" || res.NewState == res.OldState {
		t.Fatalf("state did not advance: %q → %q", res.OldState, res.NewState)
	}

	sums, err := c.FetchSummaries(ctx, []mail.ID{"e1"})
	if err != nil {
		t.Fatalf("FetchSummaries: %v", err)
	}
	s := sums[0]
	if !s.Keywords.Has("$flagged") || s.Keywords.Has("$seen") {
		t.Fatalf("keywords = %v", s.Keywords)
	}
	if len(s.MailboxIDs) != 1 || s.MailboxIDs[0] != "mb-archive" {
		t.Fatalf("mailboxes = %v, want [mb-archive]", s.MailboxIDs)
	}

	// Fixture counts followed the move (FR-B6 realism): archive gained the
	// message, agent-test lost it.
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	for _, mb := range mbs.Mailboxes {
		switch mb.ID {
		case "mb-archive":
			if mb.TotalEmails != 1 || mb.UnreadEmails != 1 {
				t.Errorf("archive counts = %d/%d, want 1/1", mb.UnreadEmails, mb.TotalEmails)
			}
		case "mb-agent":
			if mb.TotalEmails != 2 || mb.UnreadEmails != 1 {
				t.Errorf("agent-test counts = %d unread/%d total, want 1/2", mb.UnreadEmails, mb.TotalEmails)
			}
		}
	}
}

func TestMutateDestroy(t *testing.T) {
	c, _ := newMutateClient(t)
	ctx := context.Background()

	res, err := c.Mutate(ctx, mail.Mutation{Destroy: []mail.ID{"e1"}})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0] != "e1" {
		t.Fatalf("Destroyed = %v, want [e1]", res.Destroyed)
	}
	// A destroyed id is reported notFound by Email/get: the caller sees an
	// empty result set (missing ids are a normal reconcile outcome).
	sums, err := c.FetchSummaries(ctx, []mail.ID{"e1"})
	if err != nil {
		t.Fatalf("FetchSummaries after destroy: %v", err)
	}
	if len(sums) != 0 {
		t.Fatalf("summaries after destroy = %v, want none", sums)
	}
}

func TestMutatePartialFailure(t *testing.T) {
	c, _ := newMutateClient(t)
	ctx := context.Background()

	res, err := c.Mutate(ctx, mail.Mutation{Emails: map[mail.ID]mail.EmailPatch{
		"e1":    {SetKeywords: map[string]bool{"$flagged": true}},
		"gone1": {SetKeywords: map[string]bool{"$flagged": true}},
	}})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "e1" {
		t.Fatalf("Updated = %v, want [e1]", res.Updated)
	}
	if _, ok := res.NotUpdated["gone1"]; !ok {
		t.Fatalf("NotUpdated = %v, want gone1", res.NotUpdated)
	}
}

func TestDownloadBlob(t *testing.T) {
	c, _ := newMutateClient(t)
	ctx := context.Background()

	rc, err := c.DownloadBlob(ctx, "b1", "notes.txt", "text/plain")
	if err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("blob = %q, want hello", data)
	}
}

func TestDownloadBlobNotFound(t *testing.T) {
	c, _ := newMutateClient(t)
	ctx := context.Background()
	if _, err := c.DownloadBlob(ctx, "nope", "x.txt", ""); err == nil {
		t.Fatal("unknown blob succeeded; want error")
	}
}
