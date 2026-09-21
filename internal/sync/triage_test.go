package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// triageFixtures: e3 (read, newest), e2 (unread), e1 (unread, oldest).
func triageFixtures() []mockjmap.Email {
	base := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	return []mockjmap.Email{
		{
			ID: "e3", ThreadID: "t3", MailboxIDs: []string{"mb-inbox"},
			Keywords: map[string]bool{"$seen": true},
			From:     []mockjmap.Address{{Name: "Carol", Email: "carol@example.test"}},
			Subject:  "hello three", ReceivedAt: base.Add(2 * time.Hour),
			Preview:  "third",
			TextBody: "third\n",
		},
		{
			ID: "e2", ThreadID: "t2", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			Subject: "hello two", ReceivedAt: base.Add(1 * time.Hour),
			Preview:  "second",
			TextBody: "second\n",
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject: "hello one", ReceivedAt: base,
			Preview:  "first",
			TextBody: "first\n",
		},
	}
}

// newTriageEngine opens the standard triage fixtures in mb-inbox; the mock
// carries distinct trash/archive/agent mailboxes for move flows.
func newTriageEngine(t *testing.T) (*Engine, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 1},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 2},
		{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive", SortOrder: 3},
	})
	srv.SetEmails(triageFixtures())
	t.Cleanup(srv.Close)

	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{})
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	return e, srv
}

// rowByID finds the rendered row for id (zero Row when absent).
func rowByID(e *Engine, id mail.ID) Row {
	for _, r := range e.Snapshot().Rows {
		if r.ID == id {
			return r
		}
	}
	return Row{}
}

func TestTriageMarkReadConfirmsAndUndoes(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	// e1 is unread; e3 is already read (no-op).
	rcpt, err := e.Triage(ctx, TriageSpec{Kind: TriageRead, IDs: []mail.ID{"e1", "e3"}})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(rcpt.Applied) != 1 || rcpt.Applied[0] != "e1" {
		t.Fatalf("Applied = %v, want [e1] (already-read id is a no-op)", rcpt.Applied)
	}
	if rcpt.Undo == nil || rcpt.Undo.Kind != TriageUnread || len(rcpt.Undo.IDs) != 1 || rcpt.Undo.IDs[0] != "e1" {
		t.Fatalf("Undo = %+v", rcpt.Undo)
	}
	if !rowByID(e, "e1").Summary.Keywords.Has("$seen") {
		t.Fatal("e1 not marked read")
	}
	if _, pending := e.overlay["e1"]; pending {
		t.Fatal("overlay not confirmed after /set")
	}

	// Undo restores unread.
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(undo): %v", err)
	}
	if rowByID(e, "e1").Summary.Keywords.Has("$seen") {
		t.Fatal("e1 still read after undo")
	}
}

func TestTriageStar(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	if _, err := e.Triage(ctx, TriageSpec{Kind: TriageStar, IDs: []mail.ID{"e2"}}); err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if !rowByID(e, "e2").Summary.Keywords.Has("$flagged") {
		t.Fatal("e2 not starred")
	}
}

func TestTriageMoveHidesRowAndUndoRestoresIt(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	if got := e.Snapshot().Total; got != 3 {
		t.Fatalf("setup total = %d", got)
	}

	rcpt, err := e.Triage(ctx, TriageSpec{Kind: TriageMove, IDs: []mail.ID{"e1"}, Mailbox: "mb-agent"})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(rcpt.Applied) != 1 {
		t.Fatalf("Applied = %v", rcpt.Applied)
	}
	if rowByID(e, "e1").ID != "" {
		t.Fatal("moved row still visible")
	}
	if got := e.Snapshot().Total; got != 2 {
		t.Fatalf("total after move = %d, want 2", got)
	}

	if rcpt.Undo == nil || rcpt.Undo.Kind != TriageMove || rcpt.Undo.Mailbox != "mb-agent" {
		t.Fatalf("Undo = %+v", rcpt.Undo)
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(undo): %v", err)
	}
	if rowByID(e, "e1").ID == "" {
		t.Fatal("undo did not restore the row")
	}
	if got := e.Snapshot().Total; got != 3 {
		t.Fatalf("total after undo = %d, want 3", got)
	}
	if !inMailbox(rowByID(e, "e1").Summary, "mb-inbox") {
		t.Fatalf("memberships after undo = %v", rowByID(e, "e1").Summary.MailboxIDs)
	}
}

func TestTriageCopyKeepsRowAndUndoDropsMembership(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	rcpt, err := e.Triage(ctx, TriageSpec{Kind: TriageCopy, IDs: []mail.ID{"e2"}, Mailbox: "mb-archive"})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if rowByID(e, "e2").ID == "" {
		t.Fatal("copy removed the row from the view")
	}
	sum := rowByID(e, "e2").Summary
	if !inMailbox(sum, "mb-archive") || !inMailbox(sum, "mb-inbox") {
		t.Fatalf("memberships after copy = %v", sum.MailboxIDs)
	}

	if rcpt.Undo == nil || rcpt.Undo.Kind != TriageRemoveMailbox {
		t.Fatalf("Undo = %+v", rcpt.Undo)
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(undo): %v", err)
	}
	if inMailbox(rowByID(e, "e2").Summary, "mb-archive") {
		t.Fatalf("undo did not remove the copy membership")
	}
}

func TestTriageBatchIsOneSet(t *testing.T) {
	e, srv := newTriageEngine(t)
	ctx := context.Background()

	before := srv.SetCalls()
	if _, err := e.Triage(ctx, TriageSpec{Kind: TriageRead, IDs: []mail.ID{"e1", "e2"}}); err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if got := srv.SetCalls() - before; got != 1 {
		t.Fatalf("batch triage issued %d Email/set calls, want 1 (FR-K4)", got)
	}
}

func TestTriagePrepareDestroyHidesAndCancels(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	snap := e.PrepareDestroy([]mail.ID{"e1"})
	if snap.Version == 0 {
		t.Fatal("PrepareDestroy published nothing")
	}
	if rowByID(e, "e1").ID != "" {
		t.Fatal("prepared destroy left the row visible")
	}
	if got := e.Snapshot().Total; got != 2 {
		t.Fatalf("total after prepare = %d, want 2", got)
	}

	// Cancel restores the row via the anchored re-query.
	e.CancelDestroy(ctx, []mail.ID{"e1"})
	if rowByID(e, "e1").ID == "" {
		t.Fatal("CancelDestroy did not restore the row")
	}
	if got := e.Snapshot().Total; got != 3 {
		t.Fatalf("total after cancel = %d, want 3", got)
	}
}

func TestTriageDestroyCommitIsPermanent(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	if _, err := e.Triage(ctx, TriageSpec{Kind: TriageDestroy, IDs: []mail.ID{"e1"}}); err != nil {
		t.Fatalf("Triage(destroy): %v", err)
	}
	if rowByID(e, "e1").ID != "" {
		t.Fatal("destroyed row still visible")
	}
	if rcpt := e.Snapshot(); rcpt.Total != 2 {
		t.Fatalf("total after destroy = %d, want 2", rcpt.Total)
	}
}

func TestTriageUnknownIdIsSkipped(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	rcpt, err := e.Triage(ctx, TriageSpec{Kind: TriageRead, IDs: []mail.ID{"nope", "e1"}})
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(rcpt.Applied) != 1 || rcpt.Applied[0] != "e1" {
		t.Fatalf("Applied = %v, want [e1]", rcpt.Applied)
	}
}

func TestTriagePendingOverlaySurvivesRefetch(t *testing.T) {
	e, _ := newTriageEngine(t)
	ctx := context.Background()

	// A pending overlay (e.g. delayed-destroy window or unconfirmed flag)
	// must survive a fresh server fetch that lacks the delta (FR-B7).
	e.ApplyOverlay("e1", Overlay{KeywordsAdd: []string{"$flagged"}})
	if err := e.Prefetch(ctx); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	if !rowByID(e, "e1").Summary.Keywords.Has("$flagged") {
		t.Fatal("pending overlay lost on refetch")
	}
}
