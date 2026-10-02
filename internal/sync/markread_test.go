package sync

import (
	"context"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// TestEngineMarkMailboxReadRefreshesTruth drives the whole path: the
// provider sweep marks the inbox's unread fixtures, then the engine pulls
// fresh server truth — the mailbox badge drops to zero, the open window's
// rows carry $seen, and the cursor keeps its place (FR-C7, PLAN §4.1).
func TestEngineMarkMailboxReadRefreshesTruth(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	// Park the cursor on the second row so position preservation is
	// actually exercised by the in-place resync.
	e.MoveCursor(1)
	before := e.Snapshot()
	if before.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", before.Cursor)
	}
	wantID := before.Rows[before.Cursor].ID

	n, err := e.MarkMailboxRead(ctx, "mb-inbox")
	if err != nil {
		t.Fatalf("MarkMailboxRead: %v", err)
	}
	// The fixtures hold two unread inbox messages (e2, e1); e3 is read.
	if n != 2 {
		t.Fatalf("marked = %d, want 2", n)
	}

	snap := e.Snapshot()
	for _, r := range snap.Rows {
		if !r.Summary.Keywords.Has("$seen") {
			t.Errorf("row %s still unread after sweep", r.ID)
		}
	}
	if snap.Cursor != 1 || snap.Rows[snap.Cursor].ID != wantID {
		t.Errorf("cursor moved: %d (%s) → %d (%s)", before.Cursor, wantID, snap.Cursor, snap.Rows[snap.Cursor].ID)
	}
	if got := unreadIn(snap.Mailboxes, "mb-inbox"); got != 0 {
		t.Errorf("mb-inbox unread = %d, want 0", got)
	}
}

// TestEngineMarkMailboxReadOtherMailboxLeavesCounts proves the badge
// refresh is server truth, not a blanket local zero: an untouched mailbox
// keeps its unread count.
func TestEngineMarkMailboxReadOtherMailboxLeavesCounts(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if _, err := e.MarkMailboxRead(ctx, "mb-inbox"); err != nil {
		t.Fatalf("MarkMailboxRead: %v", err)
	}
	if got := unreadIn(e.Snapshot().Mailboxes, "mb-agent"); got != 1 {
		t.Errorf("mb-agent unread = %d, want 1 (untouched)", got)
	}
}

func unreadIn(nodes []MailboxNode, id mail.ID) int {
	for _, n := range nodes {
		if n.Mailbox.ID == id {
			return n.Mailbox.UnreadEmails
		}
	}
	return -1
}
