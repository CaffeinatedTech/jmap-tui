package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// searchFixtures: subjects/keywords/attachments tuned so each filter
// dimension separates cleanly. Timestamps from a 09:00 UTC base:
// s1 12:00 (inbox, attachment, $seen), s2 11:00 (agent-test),
// s3 10:00 (inbox, $flagged).
func searchFixtures() []mockjmap.Email {
	base := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	return []mockjmap.Email{
		{
			ID: "s1", ThreadID: "st1", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Ann Billing", Email: "billing@example.test"}},
			Subject: "Invoice for September", ReceivedAt: base.Add(3 * time.Hour),
			TextBody: "Your invoice is attached.\n", HasAttachment: true,
			Keywords: map[string]bool{"$seen": true},
		},
		{
			ID: "s2", ThreadID: "st2", MailboxIDs: []string{"mb-agent"},
			From:    []mockjmap.Address{{Name: "Ben Ops", Email: "ops@example.test"}},
			Subject: "Deploy failed", ReceivedAt: base.Add(2 * time.Hour),
			TextBody: "The deploy pipeline reported a failure.\n",
		},
		{
			ID: "s3", ThreadID: "st3", MailboxIDs: []string{"mb-inbox"},
			From:    []mockjmap.Address{{Name: "Ann Billing", Email: "billing@example.test"}},
			Subject: "Receipt of payment", ReceivedAt: base.Add(1 * time.Hour),
			TextBody: "Thanks for the payment.\n",
			Keywords: map[string]bool{"$flagged": true},
		},
	}
}

// newSearchEngine wires an engine to a mockjmap server with a custom
// email set (and optionally a synthetic mailbox).
func newSearchEngine(t *testing.T, emails []mockjmap.Email, syn *mockjmap.SyntheticMailbox) (*Engine, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", "correct-horse", []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 2, UnreadEmails: 1},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
		{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive", SortOrder: 2, TotalEmails: 1},
	})
	srv.SetEmails(emails)
	if syn != nil {
		srv.SetSyntheticMailbox(*syn)
	}
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return NewEngine(c, Config{}), srv
}

func mustOpen(t *testing.T, e *Engine, mailbox mail.ID) {
	t.Helper()
	if err := e.OpenMailbox(context.Background(), mailbox); err != nil {
		t.Fatalf("OpenMailbox(%s): %v", mailbox, err)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestEngineSearchOpenAndCloseRestoresCursor(t *testing.T) {
	e, _ := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")

	e.MoveCursor(1)
	preSnap := e.Snapshot()
	preID := preSnap.Rows[preSnap.Cursor].ID

	ctx := context.Background()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "invoice"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	snap := e.Snapshot()
	if !snap.SearchActive {
		t.Fatal("SearchActive = false after SearchOpen")
	}
	if snap.ViewKey == "m:mb-inbox" || snap.ViewKey == "" {
		t.Fatalf("ViewKey = %q, want a search key", snap.ViewKey)
	}
	if len(snap.Rows) != 1 || snap.Rows[0].ID != "s1" {
		t.Fatalf("search rows = %v, want [s1]", snap.Rows)
	}

	snap = e.SearchClose()
	if snap.SearchActive {
		t.Fatal("SearchActive = true after SearchClose")
	}
	if snap.ViewKey != "m:mb-inbox" {
		t.Fatalf("ViewKey = %q, want m:mb-inbox", snap.ViewKey)
	}
	if snap.Cursor >= len(snap.Rows) || snap.Rows[snap.Cursor].ID != preID {
		t.Fatalf("restored cursor = %d of %d, want %s (position preserved, FR-F1)",
			snap.Cursor, len(snap.Rows), preID)
	}
}

func TestEngineSearchScopeAllMailboxes(t *testing.T) {
	e, _ := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")

	ctx := context.Background()
	// Scoped to the open mailbox: the agent-test hit stays out (FR-F3).
	if err := e.SearchOpen(ctx, SearchSpec{Text: "deploy", ScopeMailbox: "mb-inbox"}); err != nil {
		t.Fatalf("SearchOpen scoped: %v", err)
	}
	if got := e.Snapshot().Total; got != 0 {
		t.Fatalf("scoped search total = %d, want 0", got)
	}
	// All-mailbox scope: the hit joins.
	if err := e.SearchOpen(ctx, SearchSpec{Text: "deploy"}); err != nil {
		t.Fatalf("SearchOpen all: %v", err)
	}
	snap := e.Snapshot()
	if snap.Total != 1 || len(snap.Rows) != 1 || snap.Rows[0].ID != "s2" {
		t.Fatalf("all-mailbox search = total %d rows %v, want [s2]", snap.Total, snap.Rows)
	}
}

func TestEngineSearchReissueDoesNotDoublePark(t *testing.T) {
	e, _ := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")
	e.MoveCursor(1) // cursor on the second row
	preSnap := e.Snapshot()
	preID := preSnap.Rows[preSnap.Cursor].ID

	ctx := context.Background()
	for _, text := range []string{"invoice", "payment", "deploy"} {
		if err := e.SearchOpen(ctx, SearchSpec{Text: text}); err != nil {
			t.Fatalf("SearchOpen(%q): %v", text, err)
		}
	}
	snap := e.SearchClose()
	// The parked view is the original mailbox view — re-issues replace
	// the search window, never re-park a search as the mailbox view.
	if snap.ViewKey != "m:mb-inbox" {
		t.Fatalf("ViewKey = %q, want m:mb-inbox", snap.ViewKey)
	}
	if snap.Cursor >= len(snap.Rows) || snap.Rows[snap.Cursor].ID != preID {
		t.Fatalf("restored cursor lost after re-issues (row %d of %d)", snap.Cursor, len(snap.Rows))
	}
}

func TestEngineSearchAdvancedFields(t *testing.T) {
	e, _ := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")
	ctx := context.Background()

	cases := []struct {
		name string
		spec SearchSpec
		want int
	}{
		{"from", SearchSpec{From: "ops@example"}, 1},
		{"subject", SearchSpec{Subject: "invoice"}, 1},
		{"keyword", SearchSpec{HasKeyword: "$flagged"}, 1},
		{"attachment", SearchSpec{HasAttachment: boolPtr(true)}, 1},
		{"after", SearchSpec{After: time.Date(2026, 9, 10, 10, 30, 0, 0, time.UTC)}, 2},
		{"before", SearchSpec{Before: time.Date(2026, 9, 10, 11, 30, 0, 0, time.UTC)}, 2},
		{"combined", SearchSpec{From: "billing@example.test", HasKeyword: "$flagged"}, 1},
		{"no-match", SearchSpec{Text: "zebra"}, 0},
	}
	for _, tc := range cases {
		if err := e.SearchOpen(ctx, tc.spec); err != nil {
			t.Fatalf("%s: SearchOpen: %v", tc.name, err)
		}
		if got := e.Snapshot().Total; got != tc.want {
			t.Errorf("%s: total = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestEngineOpenMailboxDiscardsSearch(t *testing.T) {
	e, _ := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")
	ctx := context.Background()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "invoice"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	mustOpen(t, e, "mb-agent")
	snap := e.Snapshot()
	if snap.SearchActive {
		t.Fatal("SearchActive survived OpenMailbox")
	}
	if snap.ViewKey != "m:mb-agent" {
		t.Fatalf("ViewKey = %q, want m:mb-agent", snap.ViewKey)
	}
	snap = e.SearchClose()
	if snap.SearchActive || snap.ViewKey != "m:mb-agent" {
		t.Fatalf("SearchClose after OpenMailbox = active %v view %q, want no-op on m:mb-agent",
			snap.SearchActive, snap.ViewKey)
	}
}

func TestEngineSearchWindowExtendsAndParksSummaries(t *testing.T) {
	// 120 single-thread messages; the search hits them all; the parked
	// mailbox view must survive eviction passes intact.
	syn := &mockjmap.SyntheticMailbox{MailboxID: "mb-agent", Prefix: "syn", Count: 120}
	e, _ := newSearchEngine(t, searchFixtures(), syn)
	mustOpen(t, e, "mb-inbox")
	e.MoveCursor(1)
	preSnap := e.Snapshot()
	preID := preSnap.Rows[preSnap.Cursor].ID

	ctx := context.Background()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "Synthetic"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	// Scroll deep enough to force extension + eviction passes.
	for i := 0; i < 60; i++ {
		e.MoveCursor(1)
		if err := e.Prefetch(ctx); err != nil {
			t.Fatalf("Prefetch: %v", err)
		}
	}
	snap := e.SearchClose()
	if len(snap.Rows) == 0 {
		t.Fatal("parked mailbox view lost its rows")
	}
	// Every restored row must still have a summary — otherwise Esc
	// renders blanks until a refetch that never comes (FR-F1).
	for i, r := range snap.Rows {
		if r.Summary.ID == "" {
			t.Fatalf("restored row %d has no summary", i)
		}
	}
	if snap.Cursor >= len(snap.Rows) || snap.Rows[snap.Cursor].ID != preID {
		t.Fatalf("restored cursor = %d of %d, want %s", snap.Cursor, len(snap.Rows), preID)
	}
}

func TestEngineSearchSuppressesSlideIn(t *testing.T) {
	e, srv := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")
	ctx := context.Background()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "invoice"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	before := e.Snapshot().Total

	// A brand-new message in the scope mailbox but matching no filter
	// must not slide into the search results.
	srv.CreateEmails([]mockjmap.Email{{
		ID: "s9", ThreadID: "st9", MailboxIDs: []string{"mb-inbox"},
		From:    []mockjmap.Address{{Name: "New", Email: "new@example.test"}},
		Subject: "totally unrelated", ReceivedAt: time.Now().UTC(),
		TextBody: "nothing to do with the query\n",
	}})
	e.reconcileEmail(ctx)
	if got := e.Snapshot().Total; got != before {
		t.Fatalf("search total changed on unrelated new mail: %d → %d", before, got)
	}

	// An update to an in-window message still patches through.
	srv.UpdateEmails([]string{"s1"}, func(em *mockjmap.Email) { em.Keywords["$flagged"] = true })
	e.reconcileEmail(ctx)
	snap := e.Snapshot()
	if !snap.Rows[snap.Cursor].Summary.Keywords.Has("$flagged") {
		t.Fatal("in-window summary patch did not land during search")
	}
}

// TestEngineSearch50kFirstPageUnder1s verifies the M4 acceptance gate:
// the first page of a search across 50,000 messages returns in under one
// second (server-bound — the mock scans the full candidate set the way a
// real server would, and the client path must not add meaningful cost).
func TestEngineSearch50kFirstPageUnder1s(t *testing.T) {
	syn := &mockjmap.SyntheticMailbox{MailboxID: "mb-agent", Prefix: "syn", Count: 50_000}
	e, _ := newSearchEngine(t, searchFixtures(), syn)
	mustOpen(t, e, "mb-inbox")

	ctx := context.Background()
	start := time.Now()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "Synthetic"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}
	elapsed := time.Since(start)

	snap := e.Snapshot()
	if snap.Total != 50_000 {
		t.Fatalf("total = %d, want 50000", snap.Total)
	}
	if len(snap.Rows) == 0 {
		t.Fatal("no rows on the first page")
	}
	if elapsed > time.Second {
		t.Fatalf("first page took %v, want < 1s", elapsed)
	}
	t.Logf("50k-message search first page: %v (limit=%d)", elapsed, len(snap.Rows))
}

func TestEngineMailboxViewReanchorsAfterSearch(t *testing.T) {
	e, srv := newSearchEngine(t, searchFixtures(), nil)
	mustOpen(t, e, "mb-inbox")
	ctx := context.Background()
	if err := e.SearchOpen(ctx, SearchSpec{Text: "invoice"}); err != nil {
		t.Fatalf("SearchOpen: %v", err)
	}

	// While the search is open, new mail lands in the mailbox view.
	srv.CreateEmails([]mockjmap.Email{{
		ID: "s8", ThreadID: "st8", MailboxIDs: []string{"mb-inbox"},
		From:    []mockjmap.Address{{Name: "Fresh", Email: "fresh@example.test"}},
		Subject: "brand new", ReceivedAt: time.Now().UTC(),
		TextBody: "arrived while searching\n",
	}})
	e.reconcileEmail(ctx)

	e.SearchClose()
	if e.window == nil || !e.window.Dirty() {
		t.Fatal("restored window not marked dirty after search close")
	}
	if err := e.Prefetch(ctx); err != nil {
		t.Fatalf("Prefetch (re-anchor): %v", err)
	}
	// The anchored re-query saw the arrival (cursor position is
	// preserved by id, so the new row sits above the viewport — FR-D5).
	if got := e.Snapshot().Total; got != 3 {
		t.Fatalf("total after re-anchor = %d, want 3", got)
	}
	e.MoveCursor(-1)
	if err := e.Prefetch(ctx); err != nil {
		t.Fatalf("Prefetch (backward): %v", err)
	}
	for _, r := range e.Snapshot().Rows {
		if r.ID == "s8" {
			return // the live arrival is visible above the cursor
		}
	}
	t.Fatalf("new mail missing after re-anchor + scroll up; rows=%v", e.Snapshot().Rows)
}
