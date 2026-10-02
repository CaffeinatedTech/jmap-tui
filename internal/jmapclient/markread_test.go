package jmapclient

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// TestMarkMailboxReadPagesEveryUnread pins the Bulwark paging rule: the
// sweep must mark every unread message, not every other page. Marked
// messages drop out of the notKeyword:$seen filter, so the query always
// re-reads from position 0; advancing would skip a page each round.
func TestMarkMailboxReadPagesEveryUnread(t *testing.T) {
	c, srv := newTestClient(t, testPassword)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Five unread plus one already-seen message: the filter must skip the
	// read one, and paging must reach all five unread.
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	var emails []mockjmap.Email
	for i, id := range []string{"u1", "u2", "u3", "u4", "u5"} {
		emails = append(emails, mockjmap.Email{
			ID: id, ThreadID: "th-" + id, MailboxIDs: []string{"mb-inbox"},
			From:       []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject:    "unread " + id,
			ReceivedAt: base.Add(time.Duration(i) * time.Minute),
			TextBody:   "body\n",
		})
	}
	emails = append(emails, mockjmap.Email{
		ID: "r1", ThreadID: "th-r1", MailboxIDs: []string{"mb-inbox"},
		Keywords:   map[string]bool{"$seen": true},
		From:       []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
		Subject:    "already read",
		ReceivedAt: base,
		TextBody:   "body\n",
	})
	srv.SetEmails(emails)

	old := markReadChunk
	markReadChunk = 2
	t.Cleanup(func() { markReadChunk = old })

	n, err := c.MarkMailboxRead(ctx, "mb-inbox")
	if err != nil {
		t.Fatalf("MarkMailboxRead: %v", err)
	}
	if n != 5 {
		t.Fatalf("marked = %d, want 5 (every unread, not every other page)", n)
	}
	if calls := srv.SetCalls(); calls != 3 {
		t.Fatalf("Email/set calls = %d, want 3 (ceil(5/2) chunks)", calls)
	}

	// Nothing unread left: a second sweep finds none.
	again, err := c.MarkMailboxRead(ctx, "mb-inbox")
	if err != nil {
		t.Fatalf("second MarkMailboxRead: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep marked = %d, want 0", again)
	}
}

// TestMarkMailboxReadScopedToOneMailbox proves the sweep never touches an
// unread message in another mailbox.
func TestMarkMailboxReadScopedToOneMailbox(t *testing.T) {
	c, srv := newTestClient(t, testPassword)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	srv.SetEmails([]mockjmap.Email{
		{ID: "i1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"}, From: []mockjmap.Address{{Name: "A", Email: "a@x.test"}}, Subject: "inbox", ReceivedAt: base, TextBody: "x\n"},
		{ID: "a1", ThreadID: "t2", MailboxIDs: []string{"mb-archive"}, From: []mockjmap.Address{{Name: "B", Email: "b@x.test"}}, Subject: "archive", ReceivedAt: base, TextBody: "x\n"},
	})

	n, err := c.MarkMailboxRead(ctx, "mb-inbox")
	if err != nil {
		t.Fatalf("MarkMailboxRead: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	// The archive message stays unread: clearing it now returns it.
	left, err := c.MarkMailboxRead(ctx, "mb-archive")
	if err != nil {
		t.Fatalf("MarkMailboxRead archive: %v", err)
	}
	if left != 1 {
		t.Fatalf("archive sweep = %d, want 1 (untouched by the inbox sweep)", left)
	}
}

// TestMarkMailboxReadRefusedAborts guards the loop-forever case: a page
// the server rejects must abort, since the rejected ids stay unread and
// the next query would return them again.
func TestMarkMailboxReadRefusedAborts(t *testing.T) {
	c, srv := newTestClient(t, testPassword)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	srv.SetSyntheticMailbox(mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 3})
	// Synthetic ids are not real fixtures, so Email/set answers notFound
	// for every one — the refusal the guard is built for.
	if _, err := c.MarkMailboxRead(ctx, "mb-big"); err == nil {
		t.Fatal("MarkMailboxRead on a refusing server returned nil error")
	}
}
