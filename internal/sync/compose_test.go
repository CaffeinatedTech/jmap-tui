package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// composeMailboxes adds the Drafts and Sent roles compose needs.
func composeMailboxes() []mockjmap.Mailbox {
	return []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 1, UnreadEmails: 1},
		{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 4},
		{ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 5},
	}
}

// newComposeEngine builds an engine over a mock with role mailboxes and
// one inbox message to reply to.
func newComposeEngine(t *testing.T) (*Engine, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", composeMailboxes())
	srv.SetEmails([]mockjmap.Email{{
		ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
		Keywords:   map[string]bool{"$seen": true},
		MessageID:  []string{"<orig@example.test>"},
		From:       []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
		To:         []mockjmap.Address{{Name: "Tester", Email: "tester@example.com"}},
		Subject:    "thread starter",
		ReceivedAt: time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC),
		TextBody:   "The original body.\n",
	}})
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{
		ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse",
	})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{})
	if err := e.LoadMailboxes(context.Background()); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	return e, srv
}

func sampleDraft() mail.Draft {
	return mail.Draft{
		IdentityID: "id-1",
		From:       []mail.Address{{Name: "Tester", Email: "tester@example.com"}},
		To:         []mail.Address{{Email: "alice@example.test"}},
		Subject:    "hello",
		Text:       "body\n",
	}
}

// TestSaveDraftAppearsInOpenDraftsView: an autosave while Drafts is the
// open view lands as a row immediately, without waiting for push.
func TestSaveDraftAppearsInOpenDraftsView(t *testing.T) {
	e, srv := newComposeEngine(t)
	ctx := context.Background()
	if err := e.OpenMailbox(ctx, "mb-drafts"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	id, snap, err := e.SaveDraft(ctx, sampleDraft())
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if id == "" {
		t.Fatal("SaveDraft returned no id")
	}
	found := false
	for _, row := range snap.Rows {
		if row.ID == id {
			found = true
			if row.Summary.Subject != "hello" {
				t.Errorf("row subject = %q", row.Summary.Subject)
			}
		}
	}
	if !found {
		t.Errorf("draft %s not in the open Drafts view (%d rows)", id, len(snap.Rows))
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("server drafts = %d, want 1", srv.CountIn("mb-drafts"))
	}

	// An edit recreates: the store must forget the old id, not show two.
	d := sampleDraft()
	d.ID = id
	d.Subject = "hello again"
	id2, snap2, err := e.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft edit: %v", err)
	}
	if id2 == id {
		t.Error("edit did not recreate")
	}
	if srv.CountIn("mb-drafts") != 1 {
		t.Errorf("server drafts after edit = %d, want 1", srv.CountIn("mb-drafts"))
	}
	rows := map[mail.ID]bool{}
	for _, row := range snap2.Rows {
		rows[row.ID] = true
	}
	if rows[id] {
		t.Error("retired draft still rendered")
	}
}

// TestSaveDraftLeavesOtherViewsAlone: autosaving while the Inbox is open
// must not splice a draft row into it.
func TestSaveDraftLeavesOtherViewsAlone(t *testing.T) {
	e, _ := newComposeEngine(t)
	ctx := context.Background()
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	before := len(e.Snapshot().Rows)

	id, snap, err := e.SaveDraft(ctx, sampleDraft())
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if id == "" {
		t.Fatal("no id")
	}
	if len(snap.Rows) != before {
		t.Errorf("inbox rows %d → %d; a draft must not enter another view", before, len(snap.Rows))
	}
	for _, row := range snap.Rows {
		if row.ID == id {
			t.Error("draft row appeared in the Inbox view")
		}
	}
}

// TestSendFillsSentAndClearsDrafts is the FR-H6 pipeline from the store's
// point of view: with Sent open the sent copy shows up without a manual
// refresh, and the draft row is gone.
func TestSendFillsSentAndClearsDrafts(t *testing.T) {
	e, srv := newComposeEngine(t)
	ctx := context.Background()

	d := sampleDraft()
	id, _, err := e.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	d.ID = id

	if err := e.OpenMailbox(ctx, "mb-sent"); err != nil {
		t.Fatalf("OpenMailbox sent: %v", err)
	}
	rcpt, snap, err := e.Send(ctx, d)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rcpt.EmailID != id {
		t.Errorf("EmailID = %s, want %s", rcpt.EmailID, id)
	}
	found := false
	for _, row := range snap.Rows {
		if row.ID == id {
			found = true
			if row.Summary.Subject != "hello" {
				t.Errorf("sent row subject = %q", row.Summary.Subject)
			}
		}
	}
	if !found {
		t.Errorf("sent message not in the open Sent view (%d rows)", len(snap.Rows))
	}
	if srv.CountIn("mb-drafts") != 0 {
		t.Errorf("draft left behind in Drafts: %d", srv.CountIn("mb-drafts"))
	}
	if srv.CountIn("mb-sent") != 1 {
		t.Errorf("sent copies = %d, want 1", srv.CountIn("mb-sent"))
	}
}

// TestSendRemovesDraftFromOpenDrafts: with Drafts open, sending retires
// the row the user is looking at.
func TestSendRemovesDraftFromOpenDrafts(t *testing.T) {
	e, _ := newComposeEngine(t)
	ctx := context.Background()

	d := sampleDraft()
	id, _, err := e.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	d.ID = id
	if err := e.OpenMailbox(ctx, "mb-drafts"); err != nil {
		t.Fatalf("OpenMailbox drafts: %v", err)
	}
	if _, _, err := e.Send(ctx, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
	snap := e.Snapshot()
	for _, row := range snap.Rows {
		if row.ID == id {
			t.Error("draft row still rendered after send")
		}
	}
	if snap.Total != 0 {
		t.Errorf("total = %d, want 0", snap.Total)
	}
}

// TestReplyContextUsesBodyCache: once the preview has loaded a message,
// opening a reply costs no extra fetch (FR-D4 lazy load reuse).
func TestReplyContextUsesBodyCache(t *testing.T) {
	e, _ := newComposeEngine(t)
	ctx := context.Background()

	counted := &countingProvider{Provider: e.p}
	e.p = counted

	if err := e.LoadBody(ctx, "e1"); err != nil {
		t.Fatalf("LoadBody: %v", err)
	}
	if got := counted.fetches; got != 1 {
		t.Fatalf("FetchBody calls after LoadBody = %d, want 1", got)
	}
	body, err := e.ReplyContext(ctx, "e1")
	if err != nil {
		t.Fatalf("ReplyContext: %v", err)
	}
	if counted.fetches != 1 {
		t.Errorf("ReplyContext refetched: %d calls, want 1 (cache hit)", counted.fetches)
	}
	if len(body.MessageID) != 1 || body.MessageID[0] != "<orig@example.test>" {
		t.Errorf("MessageID = %v", body.MessageID)
	}
	if body.Subject != "thread starter" {
		t.Errorf("Subject = %q", body.Subject)
	}
	if body.Text == "" {
		t.Error("no display text to quote")
	}

	// A message that was never previewed still fetches — and a missing one
	// fails loudly rather than yielding an empty reply.
	if _, err := e.ReplyContext(ctx, "e2-missing"); err == nil {
		t.Error("ReplyContext for an unknown id must fail")
	}
	if counted.fetches != 2 {
		t.Errorf("uncached lookup fetched %d times, want 2", counted.fetches)
	}
}

// TestReplyContextRejectsNoSelection pins the guard so a reply with no
// cursor fails with a sentence instead of an empty draft.
func TestReplyContextRejectsNoSelection(t *testing.T) {
	e, _ := newComposeEngine(t)
	if _, err := e.ReplyContext(context.Background(), ""); err == nil {
		t.Error("empty id must be rejected")
	}
}

// TestSendRequiresDraftsMailbox: an account with no Drafts role cannot
// autosave, and says so before touching the network.
func TestSendRequiresDraftsMailbox(t *testing.T) {
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
	})
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{
		ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse",
	})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	e := NewEngine(c, Config{})
	if err := e.LoadMailboxes(context.Background()); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	_, _, err := e.SaveDraft(context.Background(), sampleDraft())
	if err == nil {
		t.Fatal("SaveDraft without a Drafts mailbox must fail")
	}
}

// countingProvider tallies FetchBody and Thread/get calls so cache and
// refresh behaviour is observable.
type countingProvider struct {
	mail.Provider
	fetches int
	threads int
}

func (p *countingProvider) FetchBody(ctx context.Context, id mail.ID) (mail.EmailBody, error) {
	p.fetches++
	return p.Provider.FetchBody(ctx, id)
}

func (p *countingProvider) Threads(ctx context.Context, threadIDs []mail.ID) (map[mail.ID][]mail.ID, error) {
	p.threads++
	return p.Provider.Threads(ctx, threadIDs)
}
