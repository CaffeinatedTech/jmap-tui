package app

import "testing"

// TestScrollCoalescesBodyHydrationUnified: holding the movement key in the
// merged view passes many rows but must hydrate only the one it settles
// on. Intermediate ticks are never pumped (the key is "held"), which is
// exactly what a burst of auto-repeat looks like to the model (FR-D4,
// FR-K4).
func TestScrollCoalescesBodyHydrationUnified(t *testing.T) {
	m, srvWork, srvPersonal := newTwoAccountModel(t)
	loadAll(t, m)

	_, cmd := m.handleKey(key("i"))
	pump(t, m, cmd)
	if !m.unified {
		t.Fatal("unified view did not open")
	}
	// Row 0 hydrates on entry; that is the baseline the scroll must not
	// add to until it settles.
	if m.vpBodyID != m.cursorKey() {
		t.Fatalf("row 0 body not installed: %q, want %q", m.vpBodyID, m.cursorKey())
	}
	base := srvWork.BodyGetCalls() + srvPersonal.BodyGetCalls()

	// Three rows passed, tick ignored: nothing may reach the server.
	for i := 0; i < 3; i++ {
		_, _ = m.handleKey(key("j"))
	}
	if got := srvWork.BodyGetCalls() + srvPersonal.BodyGetCalls(); got != base {
		t.Fatalf("held scroll issued %d body fetches, want 0", got-base)
	}
	acct, row, ok := m.cursorRef()
	if !ok {
		t.Fatal("no cursor row")
	}
	wantKey := m.rowKey(acct, row.ID)
	if m.bodyPending != wantKey {
		t.Fatalf("pending hydration = %q, want %q", m.bodyPending, wantKey)
	}

	// A superseded tick must not issue even though it names this row.
	if _, c := m.Update(bodyDebounceMsg{seq: m.bodySeq - 1, acct: acct, id: row.ID}); c != nil {
		t.Fatal("stale debounce tick issued a fetch")
	}
	if got := srvWork.BodyGetCalls() + srvPersonal.BodyGetCalls(); got != base {
		t.Fatalf("stale tick issued a fetch: %d", got-base)
	}

	// The cursor rested: one tick, one fetch.
	_, cmd = m.Update(bodyDebounceMsg{seq: m.bodySeq, acct: acct, id: row.ID})
	pump(t, m, cmd)
	if got := srvWork.BodyGetCalls() + srvPersonal.BodyGetCalls(); got != base+1 {
		t.Fatalf("settled scroll issued %d body fetches, want 1", got-base)
	}
	if m.vpBodyID != wantKey {
		t.Fatalf("resting row body not installed: %q, want %q", m.vpBodyID, wantKey)
	}
	if m.snap.BodyLoading {
		t.Fatal("preview still claims to be loading after the settle")
	}
}

// TestScrollCoalescesBodyHydrationSingle: the same discipline on the
// single-account path, where the body rides the engine's cursor-scoped
// snapshot. A cached revisit still paints immediately.
func TestScrollCoalescesBodyHydrationSingle(t *testing.T) {
	m, srvWork, _ := newTwoAccountModel(t)
	loadAll(t, m)
	if m.unified {
		t.Fatal("view unexpectedly unified")
	}
	if m.vpBodyID != "e2" {
		t.Fatalf("initial body = %q, want e2", m.vpBodyID)
	}
	base := srvWork.BodyGetCalls()

	// Move to the older row and hold.
	_, _ = m.moveCursor(1)
	if got := srvWork.BodyGetCalls(); got != base {
		t.Fatalf("held scroll issued %d body fetches, want 0", got-base)
	}
	if m.bodyPending != "e1" {
		t.Fatalf("pending hydration = %q, want e1", m.bodyPending)
	}

	// Settle: one fetch, body installed.
	_, cmd := m.Update(bodyDebounceMsg{seq: m.bodySeq, acct: m.activeID, id: "e1"})
	pump(t, m, cmd)
	if got := srvWork.BodyGetCalls(); got != base+1 {
		t.Fatalf("settled scroll issued %d body fetches, want 1", got-base)
	}
	if m.vpBodyID != "e1" {
		t.Fatalf("resting row body = %q, want e1", m.vpBodyID)
	}

	// Revisiting the first row is an LRU hit: it paints with no fetch.
	_, cmd = m.moveCursor(-1)
	pump(t, m, cmd)
	if got := srvWork.BodyGetCalls(); got != base+1 {
		t.Fatalf("cached revisit fetched again: %d extra", got-(base+1))
	}
	if m.vpBodyID != "e2" {
		t.Fatalf("cached revisit body = %q, want e2", m.vpBodyID)
	}
}
