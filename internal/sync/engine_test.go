package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// emailFixtures: a 2-member thread, an HTML-only message, an attachment.
func emailFixtures() []mockjmap.Email {
	base := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	return []mockjmap.Email{
		{
			ID: "e3", ThreadID: "t2", MailboxIDs: []string{"mb-inbox"},
			Keywords: map[string]bool{"$seen": true},
			From:     []mockjmap.Address{{Name: "Carol", Email: "carol@example.test"}},
			Subject:  "HTML only", ReceivedAt: base.Add(2 * time.Hour),
			Size: 2048, Preview: "Rich text preview",
			HTMLBody: "<html><body><p>Rich <b>text</b></p></body></html>",
		},
		{
			ID: "e2", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Bob", Email: "bob@example.test"}},
			Subject: "Re: thread starter", ReceivedAt: base.Add(1 * time.Hour),
			Size: 1024, Preview: "The reply body",
			TextBody: "The reply body.\n",
		},
		{
			ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
			Subject: "thread starter", ReceivedAt: base,
			Size: 4096, HasAttachment: true, Preview: "The original body",
			TextBody: "The original body.\n",
		},
	}
}

func newTestEngine(t *testing.T, syn *mockjmap.SyntheticMailbox) (*Engine, *mockjmap.Server) {
	return newTestEngineWithProvider(t, syn, func(p mail.Provider) mail.Provider { return p })
}

// newTestEngineWithProvider builds the usual fixture engine with the
// provider wrapped — the seam tests use to stall a fetch mid-flight.
func newTestEngineWithProvider(t *testing.T, syn *mockjmap.SyntheticMailbox, wrap func(mail.Provider) mail.Provider) (*Engine, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 3, UnreadEmails: 2},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive", SortOrder: 2, TotalEmails: 3, UnreadEmails: 1},
	})
	srv.SetEmails(emailFixtures())
	if syn != nil {
		srv.SetSyntheticMailbox(*syn)
	}
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return NewEngine(wrap(c), Config{}), srv
}

func TestEngineMailboxTreeAndOpen(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()

	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	snap := e.Snapshot()
	if len(snap.Mailboxes) != 3 {
		t.Fatalf("mailboxes = %d", len(snap.Mailboxes))
	}
	if snap.Mailboxes[0].Mailbox.Name != "Inbox" || snap.Mailboxes[0].Depth != 0 {
		t.Fatalf("root order wrong: %+v", snap.Mailboxes[0])
	}
	if snap.Mailboxes[2].Mailbox.Name != "agent-test" || snap.Mailboxes[2].Depth != 1 {
		t.Fatalf("tree depth wrong: %+v", snap.Mailboxes[2])
	}

	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	snap = e.Snapshot()
	if snap.Total != 2 || len(snap.Rows) != 2 {
		t.Fatalf("rows = %d total %d, want 2/2 (thread collapsed)", len(snap.Rows), snap.Total)
	}
	if snap.Cursor != 0 || snap.Rows[0].Summary.Subject != "HTML only" {
		t.Fatalf("cursor/subject = %d/%q", snap.Cursor, snap.Rows[0].Summary.Subject)
	}
	if snap.ActiveMailbox != "mb-inbox" {
		t.Fatalf("active mailbox = %q", snap.ActiveMailbox)
	}
}

func TestEngineMoveCursorIsLocal(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	snap := e.MoveCursor(1)
	if snap.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", snap.Cursor)
	}
	snap = e.MoveCursor(-5)
	if snap.Cursor != 0 {
		t.Fatalf("cursor clamped = %d, want 0", snap.Cursor)
	}
}

func TestEngineBodyLRUAndHTMLConversion(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	// Cursor on the HTML-only message: text comes from the converter.
	id := e.CursorID()
	if err := e.LoadBody(ctx, id); err != nil {
		t.Fatalf("LoadBody: %v", err)
	}
	snap := e.Snapshot()
	if snap.Body == nil || snap.Body.ID != id {
		t.Fatalf("body not installed: %+v", snap.Body)
	}
	if snap.Body.Text != "Rich text" {
		t.Fatalf("converted text = %q, want %q", snap.Body.Text, "Rich text")
	}

	// Moving the cursor away drops the body view; loading the other message
	// installs its text body.
	snap = e.MoveCursor(1)
	if snap.Body != nil {
		t.Fatalf("body followed cursor: %+v", snap.Body)
	}
	other := e.CursorID()
	if err := e.LoadBody(ctx, other); err != nil {
		t.Fatalf("LoadBody(other): %v", err)
	}
	if snap := e.Snapshot(); snap.Body == nil || snap.Body.ID != other {
		t.Fatalf("body for %q missing: %+v", other, snap.Body)
	}

	// Returning to the first message hits the LRU (no network).
	e.MoveCursor(-1)
	if err := e.LoadBody(ctx, id); err != nil {
		t.Fatalf("LoadBody(cache): %v", err)
	}
	if snap := e.Snapshot(); snap.Body == nil || snap.Body.ID != id || snap.Body.Text != "Rich text" {
		t.Fatalf("cached body wrong: %+v", snap.Body)
	}
}

func TestEngineThreadExpandCollapse(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	// Move to the thread representative (e2, oldest collapsed row).
	e.MoveCursor(1)
	if err := e.ToggleThread(ctx, "e2"); err != nil {
		t.Fatalf("ToggleThread: %v", err)
	}
	snap := e.Snapshot()
	// e3 precedes the thread; then header + remaining member (oldest first).
	if len(snap.Rows) != 3 {
		t.Fatalf("expanded rows = %d: %+v", len(snap.Rows), snap.Rows)
	}
	if !snap.Rows[1].ThreadHeader || snap.Rows[1].Summary.ID != "e2" {
		t.Fatalf("header row wrong: %+v", snap.Rows[1])
	}
	if !snap.Rows[2].ThreadMember || snap.Rows[2].Summary.ID != "e1" {
		t.Fatalf("member row wrong: %+v", snap.Rows[2])
	}

	// Cursor stays on the header through the toggle.
	if id := e.CursorID(); id != "e2" {
		t.Fatalf("cursor moved to %q", id)
	}

	if err := e.ToggleThread(ctx, "e2"); err != nil {
		t.Fatalf("ToggleThread(collapse): %v", err)
	}
	if snap := e.Snapshot(); len(snap.Rows) != 2 {
		t.Fatalf("collapsed rows = %d", len(snap.Rows))
	}
}

// TestEngineToggleThreadTargetsPressedRow pins the contract the app relies
// on: the toggle expands the row the user pressed, whatever the engine's
// own cursor points at. The unified view never moves an engine cursor
// (PLAN §4.3), so a cursor-relative toggle expands the wrong message —
// the chevron landing on a different subject.
func TestEngineToggleThreadTargetsPressedRow(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	// Cursor sits on row 0 (e3); press Enter on row 1 (e2) instead.
	if id := e.CursorID(); id != "e3" {
		t.Fatalf("cursor = %q, want e3", id)
	}
	if err := e.ToggleThread(ctx, "e2"); err != nil {
		t.Fatalf("ToggleThread(e2): %v", err)
	}
	rows := e.Snapshot().Rows
	if len(rows) != 3 {
		t.Fatalf("rows = %d: %+v", len(rows), rows)
	}
	if rows[0].ThreadHeader {
		t.Fatalf("chevron landed on the unpressed row: %+v", rows[0])
	}
	if !rows[1].ThreadHeader || rows[1].ID != "e2" {
		t.Fatalf("pressed row must carry the chevron: %+v", rows[1])
	}

	// A target that is not rendered expands nothing — never a fallback
	// onto whatever the engine cursor happens to select.
	if err := e.ToggleThread(ctx, "ghost"); err != nil {
		t.Fatalf("ToggleThread(ghost): %v", err)
	}
	if got := e.Snapshot().Rows; len(got) != 3 || !got[1].ThreadHeader {
		t.Fatalf("unknown target changed the view: %+v", got)
	}
}

// TestEngineSingleMessageThreadHasNoChevron: a thread of one has nothing
// to put beneath its row, so Enter must leave it plain. Every synthetic
// message is its own thread, which is exactly the shape that used to
// render a down-carrot over empty space.
func TestEngineSingleMessageThreadHasNoChevron(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 20})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	if len(e.Snapshot().Rows) == 0 {
		t.Fatal("no rows")
	}

	if err := e.ToggleThread(ctx, e.CursorID()); err != nil {
		t.Fatalf("ToggleThread: %v", err)
	}
	rows := e.Snapshot().Rows
	if len(rows) != 20 {
		t.Fatalf("rows = %d, want 20 (nothing expanded)", len(rows))
	}
	for i, r := range rows {
		if r.ThreadHeader || r.ThreadMember {
			t.Fatalf("row %d wore thread chrome on a one-message thread: %+v", i, r)
		}
	}
}

// TestEngineThreadSizesMarkExpandableRows (FR-D1): the list has to know
// which collapsed rows have replies beneath them — Enter on those expands,
// Enter on the rest does nothing — and the count is fetched once per view,
// never re-asked while the view is already sized.
func TestEngineThreadSizesMarkExpandableRows(t *testing.T) {
	counter := &countingProvider{}
	e, _ := newTestEngineWithProvider(t, nil, func(p mail.Provider) mail.Provider {
		counter.Provider = p
		return counter
	})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	sizes := func() map[mail.ID]int {
		out := map[mail.ID]int{}
		for _, r := range e.Snapshot().Rows {
			out[r.ID] = r.ThreadSize
		}
		return out
	}
	for id, s := range sizes() {
		if s != 0 {
			t.Fatalf("row %s sized %d before the refresh", id, s)
		}
	}

	e.RefreshThreadSizes(ctx)
	got := sizes()
	if got["e2"] != 2 {
		t.Errorf("thread row e2 size = %d, want 2 (e1 sits beneath it)", got["e2"])
	}
	if got["e3"] != 1 {
		t.Errorf("single-message row e3 size = %d, want 1", got["e3"])
	}
	if counter.threads != 1 {
		t.Errorf("Thread/get calls = %d, want 1", counter.threads)
	}

	e.RefreshThreadSizes(ctx)
	if counter.threads != 1 {
		t.Errorf("Thread/get calls = %d, want 1 (a sized view is not re-asked)", counter.threads)
	}
}

// gateProvider stalls the member fetch so a test can swap the view while
// it is in flight — the window is released for the network hop (NFR-1),
// which is exactly when a mailbox switch can race it.
type gateProvider struct {
	mail.Provider
	entered chan struct{}
	release chan struct{}
}

func (p *gateProvider) Threads(ctx context.Context, threadIDs []mail.ID) (map[mail.ID][]mail.ID, error) {
	p.entered <- struct{}{}
	<-p.release
	return p.Provider.Threads(ctx, threadIDs)
}

// TestEngineToggleThreadAbandonsOnViewSwitch: a mailbox switch mid-fetch
// swaps the window wholesale, so the in-flight result belongs to a view
// nobody is looking at any more. Installing it would mark whichever row of
// the new view happens to share that threadId — a chevron on a message the
// user never pressed.
func TestEngineToggleThreadAbandonsOnViewSwitch(t *testing.T) {
	gate := &gateProvider{entered: make(chan struct{}), release: make(chan struct{})}
	e, _ := newTestEngineWithProvider(t, nil, func(p mail.Provider) mail.Provider {
		gate.Provider = p
		return gate
	})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- e.ToggleThread(ctx, "e2") }()

	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("member fetch never started")
	}
	// The user opens another mailbox while the fetch is in flight.
	if err := e.OpenMailbox(ctx, "mb-archive"); err != nil {
		t.Fatalf("OpenMailbox(archive): %v", err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("ToggleThread: %v", err)
	}

	e.mu.Lock()
	expanded, cached := e.expanded["t1"], e.threads["t1"]
	e.mu.Unlock()
	if expanded {
		t.Error("expansion from the old view survived the mailbox switch")
	}
	if len(cached) > 0 {
		t.Error("thread cache from the old view survived the mailbox switch")
	}
}

func TestEnginePrefetchExtendsWindow(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 200})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}

	// Scroll near the forward edge; the window wants the next chunk.
	e.MoveCursor(45)
	if err := e.Prefetch(ctx); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	snap := e.Snapshot()
	if snap.Start != 0 || len(snap.Rows) != 100 {
		t.Fatalf("after prefetch start %d rows %d", snap.Start, len(snap.Rows))
	}

	// Jump to the end re-anchors (FR-D3).
	if err := e.Jump(ctx, JumpEnd); err != nil {
		t.Fatalf("Jump: %v", err)
	}
	snap = e.Snapshot()
	if snap.Start != 150 {
		t.Fatalf("after JumpEnd start = %d, want 150", snap.Start)
	}
	if id := e.CursorID(); id != "syn-000199" {
		t.Fatalf("cursor = %q, want syn-000199", id)
	}
}

func TestEngineWindowCapHeld(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 5000})
	e2 := NewEngine(e.p, Config{Window: WindowConfig{Chunk: 50, Cap: 150, PrefetchAt: 10}})
	ctx := context.Background()
	_ = e2.LoadMailboxes(ctx)
	if err := e2.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	if err := e2.Jump(ctx, JumpEnd); err != nil {
		t.Fatalf("Jump: %v", err)
	}
	// Walk upward in steps, prefetching as the UI would.
	for i := 0; i < 30; i++ {
		e2.MoveCursor(-40)
		if err := e2.Prefetch(ctx); err != nil {
			t.Fatalf("round %d Prefetch: %v", i, err)
		}
		if snap := e2.Snapshot(); len(snap.Rows) > 150 {
			t.Fatalf("round %d: rows %d exceeds cap", i, len(snap.Rows))
		}
	}
}

func TestEngineMailboxSwapDiscardsStaleFetch(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 300})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	e.MoveCursor(45)

	// Open another mailbox while a prefetch would be pending; the engine
	// must not surface foreign rows. (Serialised engine: the swap happens
	// between Cmds; the invariant under test is that rows always belong to
	// the active mailbox.)
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox(inbox): %v", err)
	}
	snap := e.Snapshot()
	if snap.ActiveMailbox != "mb-inbox" || snap.Total != 2 {
		t.Fatalf("swap failed: active %q total %d", snap.ActiveMailbox, snap.Total)
	}
	for _, r := range snap.Rows {
		if r.Summary.ID == "" {
			t.Fatalf("empty row id: %+v", r)
		}
	}
}

func TestEngineUnknownMailboxIsEmpty(t *testing.T) {
	e, _ := newTestEngine(t, nil)
	// Server-side semantics: an unknown mailbox is an empty result, not an
	// error (the id came from the server's own tree in real flows).
	if err := e.OpenMailbox(context.Background(), "mb-missing"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	snap := e.Snapshot()
	if snap.Total != 0 || len(snap.Rows) != 0 {
		t.Fatalf("total %d rows %d, want empty", snap.Total, len(snap.Rows))
	}
}

// TestEngineBackwardPrefetchKeepsCursor: a backward extension prepends
// rows under the raw cursor index — the selection must stay on the same
// message (FR-D5), which the id repair in snapshotLocked guarantees.
func TestEngineBackwardPrefetchKeepsCursor(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 200})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	_ = e.Jump(ctx, JumpEnd)
	_ = e.MoveCursor(-45) // near the top of the anchored chunk
	want := e.CursorID()
	if err := e.Prefetch(ctx); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	if got := e.CursorID(); got != want {
		t.Fatalf("cursor drifted %s → %s after backward extension", want, got)
	}
	if id := e.Snapshot().Rows[e.Snapshot().Cursor].Summary.ID; id != want {
		t.Fatalf("snapshot cursor row = %q, want %q", id, want)
	}
}

// TestEngineJumpUnread: the scan force-extends forward and backward past
// the loaded window (FR-D7) and reports ErrNoUnread when nothing is left.
func TestEngineJumpUnread(t *testing.T) {
	e, _ := newTestEngine(t, &mockjmap.SyntheticMailbox{MailboxID: "mb-big", Prefix: "syn", Count: 200})
	ctx := context.Background()
	_ = e.LoadMailboxes(ctx)
	if err := e.OpenMailbox(ctx, "mb-big"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	// Forward across the chunk boundary: cursor at the last loaded row
	// (idx 49, unread itself), the jump looks beyond the window.
	_ = e.MoveCursor(49)
	if err := e.JumpUnread(ctx, 1); err != nil {
		t.Fatalf("JumpUnread(forward): %v", err)
	}
	if got := e.CursorID(); got != "syn-000050" {
		t.Fatalf("forward jump landed on %q, want syn-000050", got)
	}

	// Backward from the end: rows above the window chunk must load.
	if err := e.Jump(ctx, JumpEnd); err != nil {
		t.Fatalf("JumpEnd: %v", err)
	}
	_ = e.MoveCursor(-49) // top of the anchored chunk (syn-000150)
	if err := e.JumpUnread(ctx, -1); err != nil {
		t.Fatalf("JumpUnread(backward): %v", err)
	}
	if got := e.CursorID(); got != "syn-000149" {
		t.Fatalf("backward jump landed on %q, want syn-000149", got)
	}
}
