package sync

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// newLiveEngine boots a test engine with the sync loop running against the
// mock: fast poll/backoff, bootstrap loads done, inbox open. The loop dies
// with the test context.
func newLiveEngine(t *testing.T) (*Engine, *mockjmap.Server) {
	t.Helper()
	e, srv := newTestEngine(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.liveCfg.pollInterval = 25 * time.Millisecond
	e.liveCfg.backoffBase = 5 * time.Millisecond
	e.Start(ctx)

	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, "mb-inbox"); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	// Wait for the push stream: every live test notifies through it.
	waitFor(t, 2*time.Second, func() bool { return srv.StreamCount() == 1 })
	return e, srv
}

// stateString reads the email state under the engine lock (test races with
// the sync loop otherwise).
func stateString(e *Engine) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.emailState
}

// waitFor polls fn until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// freshArrival is newer than the newest fixture (e3 @ 11:00).
func freshArrival(id string) mockjmap.Email {
	return mockjmap.Email{
		ID: id, ThreadID: "t" + id, MailboxIDs: []string{"mb-inbox"},
		From:       []mockjmap.Address{{Name: "Fresh", Email: "fresh@example.test"}},
		Subject:    "fresh arrival",
		ReceivedAt: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		TextBody:   "Just landed.\n",
	}
}

func TestLiveNewMailSlidesInAtTop(t *testing.T) {
	e, srv := newLiveEngine(t)

	srv.CreateEmails([]mockjmap.Email{freshArrival("e9")})
	srv.Notify()

	waitFor(t, 2*time.Second, func() bool {
		snap := e.Snapshot()
		return len(snap.Rows) > 0 && snap.Rows[0].Summary.ID == "e9"
	})
	snap := e.Snapshot()
	if !snap.Rows[0].Fresh {
		t.Error("slide-in row must be flagged Fresh (the delight moment)")
	}
	if snap.Total != 3 {
		t.Errorf("total = %d, want 3", snap.Total)
	}
	if snap.Status.Mode != ModePush {
		t.Errorf("mode = %q, want push", snap.Status.Mode)
	}
	if snap.Status.LastSync.IsZero() {
		t.Error("last sync never stamped")
	}

	// The fade clears the highlight (FR-B2 status plumbing drives it from
	// the app; the engine method is the mechanism).
	e.ClearFresh()
	if fresh := e.Snapshot().Rows[0].Fresh; fresh {
		t.Error("ClearFresh left the highlight on")
	}
}

func TestLiveFlagChangePatchesSummary(t *testing.T) {
	e, srv := newLiveEngine(t)

	srv.UpdateEmails([]string{"e3"}, func(em *mockjmap.Email) {
		em.Keywords = map[string]bool{"$seen": false, "$flagged": true}
	})
	srv.Notify()

	waitFor(t, 2*time.Second, func() bool {
		snap := e.Snapshot()
		if len(snap.Rows) == 0 {
			return false
		}
		kw := snap.Rows[0].Summary.Keywords
		return !kw.Has("$seen") && kw.Has("$flagged")
	})
}

func TestLiveDestroyEvictsAndRepairsCursor(t *testing.T) {
	e, srv := newLiveEngine(t)

	// Cursor on the head row (e3); destroying it must land on e2 (FR-D5).
	if id := e.CursorID(); id != "e3" {
		t.Fatalf("setup cursor = %q, want e3", id)
	}
	srv.DestroyEmails([]string{"e3"})
	srv.Notify()

	waitFor(t, 2*time.Second, func() bool {
		return e.CursorID() == "e2"
	})
	snap := e.Snapshot()
	if snap.Total != 1 || len(snap.Rows) != 1 {
		t.Fatalf("total %d rows %d, want 1/1", snap.Total, len(snap.Rows))
	}
}

func TestLiveMailboxCountsRefresh(t *testing.T) {
	e, srv := newLiveEngine(t)

	boxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 9, UnreadEmails: 7},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 1},
	}
	srv.SetMailboxes(boxes)
	srv.Notify()

	waitFor(t, 2*time.Second, func() bool {
		for _, node := range e.Snapshot().Mailboxes {
			if node.Mailbox.ID == "mb-inbox" {
				return node.Mailbox.TotalEmails == 9 && node.Mailbox.UnreadEmails == 7
			}
		}
		return false
	})
}

func TestLiveStreamDropReconnects(t *testing.T) {
	e, srv := newLiveEngine(t)

	srv.DropStreams()
	waitFor(t, 2*time.Second, func() bool { return srv.StreamCount() == 1 })

	// The reconnected stream still delivers (FR-B3 recovery).
	srv.CreateEmails([]mockjmap.Email{freshArrival("e9")})
	srv.Notify()
	waitFor(t, 2*time.Second, func() bool {
		snap := e.Snapshot()
		return len(snap.Rows) > 0 && snap.Rows[0].Summary.ID == "e9"
	})
	if mode := e.Snapshot().Status.Mode; mode != ModePush {
		t.Fatalf("mode = %q, want push after reconnect", mode)
	}
}

func TestLivePollFallbackAndPushUpgrade(t *testing.T) {
	e, srv := newLiveEngine(t)

	// Kill the healthy stream, then make every reconnect fail: the engine
	// burns its push budget and falls back to polling (FR-B3), reconciling
	// from state strings alone.
	srv.DropStreams()
	srv.FailStreams(10)
	waitFor(t, 2*time.Second, func() bool {
		return e.Snapshot().Status.Mode == ModePoll
	})

	srv.CreateEmails([]mockjmap.Email{freshArrival("e9")}) // no Notify: poll only
	waitFor(t, 2*time.Second, func() bool {
		snap := e.Snapshot()
		return len(snap.Rows) > 0 && snap.Rows[0].Summary.ID == "e9"
	})

	// Once the failure budget on the server is exhausted, the next upgrade
	// attempt succeeds and the engine returns to push (auto-upgrade).
	waitFor(t, 3*time.Second, func() bool {
		return e.Snapshot().Status.Mode == ModePush && srv.StreamCount() == 1
	})
}

func TestLiveCannotCalculateChangesResyncsAtCursor(t *testing.T) {
	e, srv := newLiveEngine(t)

	e.MoveCursor(1) // on e2
	before := stateString(e)

	srv.SetCannotCalculateChanges(true)
	srv.UpdateEmails([]string{"e3"}, func(em *mockjmap.Email) {
		em.Keywords = map[string]bool{"$seen": false}
	})
	srv.Notify()

	// The engine answers cannotCalculateChanges with a cursor-anchored
	// re-query: position survives by id and the email state advances. The
	// fresh chunk begins at the cursor's position, so e2 leads it.
	waitFor(t, 2*time.Second, func() bool { return stateString(e) != before })
	snap := e.Snapshot()
	if id := e.CursorID(); id != "e2" {
		t.Fatalf("cursor = %q, want e2 after full resync", id)
	}
	if len(snap.Rows) != 1 || snap.Start != 1 || snap.Total != 2 {
		t.Fatalf("resync window: rows %d start %d total %d, want 1/1/2", len(snap.Rows), snap.Start, snap.Total)
	}
}

func TestLiveOverlaySurvivesServerUpdateAndConfirms(t *testing.T) {
	e, srv := newLiveEngine(t)

	// Optimistic flag with no server action: summary patches immediately.
	e.ApplyOverlay("e3", Overlay{KeywordsAdd: []string{"$flagged"}})
	if kw := e.Snapshot().Rows[0].Summary.Keywords; !kw.Has("$flagged") {
		t.Fatal("overlay did not patch the summary (FR-B7 plumbing)")
	}

	// A server update to the same message mid-flight must not wipe the
	// unconfirmed local op.
	srv.UpdateEmails([]string{"e3"}, func(em *mockjmap.Email) {
		em.Subject = "renamed by server"
	})
	srv.Notify()
	waitFor(t, 2*time.Second, func() bool {
		return e.Snapshot().Rows[0].Summary.Subject == "renamed by server"
	})
	if kw := e.Snapshot().Rows[0].Summary.Keywords; !kw.Has("$flagged") {
		t.Fatal("server update clobbered the pending overlay")
	}

	// Confirm drops the overlay: future server fetches stand alone.
	e.OverlayConfirmed("e3")

	// Revert drops a fresh overlay and refetches server truth: the flag
	// was never confirmed server-side, so it disappears with the overlay.
	e.ApplyOverlay("e3", Overlay{KeywordsRemove: []string{"$flagged"}})
	if err := e.RevertOverlay(context.Background(), "e3"); err != nil {
		t.Fatalf("RevertOverlay: %v", err)
	}
	snap := e.Snapshot()
	kw := snap.Rows[0].Summary.Keywords
	if kw.Has("$flagged") || !kw.Has("$seen") {
		t.Fatalf("revert did not restore server truth: %v", kw)
	}
	if snap.Rows[0].Summary.Subject != "renamed by server" {
		t.Fatalf("revert lost the server subject: %q", snap.Rows[0].Summary.Subject)
	}
}

func TestLiveOverlayDestroyHidesRow(t *testing.T) {
	e, _ := newLiveEngine(t)

	e.ApplyOverlay("e3", Overlay{Destroy: true})
	snap := e.Snapshot()
	if snap.Total != 1 || len(snap.Rows) != 1 {
		t.Fatalf("optimistic destroy left total %d rows %d", snap.Total, len(snap.Rows))
	}
	if id := e.CursorID(); id != "e2" {
		t.Fatalf("cursor = %q, want e2 after optimistic destroy", id)
	}
}
