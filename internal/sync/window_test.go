package sync

import (
	"errors"
	"fmt"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// idsAt returns n synthetic ids starting at absolute position pos, ordered
// so that id at absolute position p is fmt.Sprintf("e%04d", p).
func idsAt(pos, n int) []mail.ID {
	out := make([]mail.ID, n)
	for i := range out {
		out[i] = mail.ID(fmt.Sprintf("e%04d", pos+i))
	}
	return out
}

// wantIDs renders the expected materialised window as a compact string.
func wantIDs(start, n int) string {
	s := "["
	for i := 0; i < n; i++ {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("e%04d", start+i)
	}
	return s + "]"
}

func TestWindowSeedAndCursor(t *testing.T) {
	w := NewWindow(Query{Filter: FilterSpec{MailboxID: "mb"}}, WindowConfig{Chunk: 50})
	r, pos, limit := w.Seed(0)
	if pos != 0 || limit != 50 {
		t.Fatalf("Seed = (%d,%d), want (0,50)", pos, limit)
	}
	if err := w.Complete(r, pos, idsAt(0, 50), 1000, "qs-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if w.Len() != 50 || w.Total() != 1000 || w.Start() != 0 || w.QueryState() != "qs-1" {
		t.Fatalf("state = len %d total %d start %d state %q", w.Len(), w.Total(), w.Start(), w.QueryState())
	}
	if id := w.CursorID(); id != "e0000" {
		t.Fatalf("CursorID = %q, want e0000", id)
	}

	w.Move(42)
	if _, id := w.Cursor(); id != "e0042" {
		t.Fatalf("after Move(42) cursor = %q", id)
	}
	w.Move(999)
	if _, id := w.Cursor(); id != "e0049" {
		t.Fatalf("Move clamped forward to %q, want e0049", id)
	}
	w.Move(-999)
	if _, id := w.Cursor(); id != "e0000" {
		t.Fatalf("Move clamped backward to %q, want e0000", id)
	}
}

func TestWindowForwardExtension(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 1000, "qs")

	w.Move(45) // within 10 of the forward edge
	r, pos, limit, dir, ok := w.NextNeed()
	if !ok || dir != Forward || pos != 50 || limit != 50 {
		t.Fatalf("NextNeed = (%v,%d,%d,%d), want forward (50,50)", ok, pos, limit, dir)
	}
	// A second NextNeed must not fire while outstanding.
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("NextNeed fired while a request was outstanding")
	}
	if err := w.Complete(r, pos, idsAt(50, 50), 1000, "qs"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if w.Len() != 100 || w.Start() != 0 {
		t.Fatalf("after extension len %d start %d", w.Len(), w.Start())
	}
	if got := wantIDs(w.Start(), w.Len()); got != wantIDs(0, 100) {
		t.Fatalf("window = %s", got)
	}
	// Cursor preserved by id across the merge.
	if _, id := w.Cursor(); id != "e0045" {
		t.Fatalf("cursor moved to %q, want e0045", id)
	}
}

func TestWindowBackwardExtension(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ := w.Seed(250)
	_ = w.Complete(r, pos, idsAt(250, 50), 1000, "qs")

	w.Move(5) // near the backward edge
	r, pos, limit, dir, ok := w.NextNeed()
	if !ok || dir != Backward || pos != 200 || limit != 50 {
		t.Fatalf("NextNeed = (%v,%d,%d,%d), want backward (200,50)", ok, pos, limit, dir)
	}
	_ = w.Complete(r, pos, idsAt(200, 50), 1000, "qs")
	if w.Start() != 200 || w.Len() != 100 {
		t.Fatalf("after backward merge start %d len %d", w.Start(), w.Len())
	}
	if _, id := w.Cursor(); id != "e0255" {
		t.Fatalf("cursor moved to %q, want e0255", id)
	}

	// Backward clamp at the top of the result set: the fetch is sized to
	// exactly the missing prefix, and no request fires once start == 0.
	w2 := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ = w2.Seed(30)
	_ = w2.Complete(r, pos, idsAt(30, 50), 1000, "qs")
	w2.SeekID("e0030") // top row

	r, pos, limit, dir, ok = w2.NextNeed()
	if !ok || dir != Backward || pos != 0 || limit != 30 {
		t.Fatalf("clamp fetch = (%v,%d,%d,%d), want backward (0,30)", ok, pos, limit, dir)
	}
	_ = w2.Complete(r, pos, idsAt(0, 30), 1000, "qs")
	if w2.Start() != 0 || w2.Len() != 80 {
		t.Fatalf("after clamped merge start %d len %d", w2.Start(), w2.Len())
	}
	if _, id := w2.Cursor(); id != "e0030" {
		t.Fatalf("cursor moved to %q, want e0030", id)
	}
	if _, _, _, dir, ok := w2.NextNeed(); ok || dir == Backward {
		t.Fatal("backward fetch requested at the top of the result set")
	}
}

func TestWindowNoPrefetchFarFromEdge(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 1000, "qs")

	w.Move(25)
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("prefetch triggered far from both edges")
	}
	fw, bw := w.AtEdge()
	if fw || bw {
		t.Fatalf("AtEdge = (%v,%v), want false,false", fw, bw)
	}
}

func TestWindowNoPrefetchAtResultEnd(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ := w.Seed(950)
	_ = w.Complete(r, pos, idsAt(950, 50), 1000, "qs")

	w.Move(49) // last row of the result set
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("prefetch triggered past the end of the result set")
	}
	if fw, _ := w.AtEdge(); fw {
		t.Fatal("AtEdge forward true past result end")
	}
}

func TestWindowSmallMailbox(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 12), 12, "qs")
	if w.Total() != 12 || w.Len() != 12 {
		t.Fatalf("small mailbox: total %d len %d", w.Total(), w.Len())
	}
	w.Move(11)
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("prefetch triggered on a fully-materialised mailbox")
	}
}

func TestWindowStaleRequestDiscarded(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r1, pos, _ := w.Seed(0)
	_ = w.Complete(r1, pos, idsAt(0, 50), 1000, "qs")
	w.Move(45)

	r2, pos2, _, _, _ := w.NextNeed() // forward extension
	_ = r2
	r3, pos3, _, dir3, _ := w.NextNeed() // must be blocked
	_ = pos3
	_ = dir3
	if r3 != 0 {
		t.Fatal("NextNeed issued a second request while one was outstanding")
	}

	// Superseding is explicit: issue (simulating a re-anchor), then the
	// older result must be rejected.
	r4, pos4, _ := w.Seed(100)
	if err := w.Complete(r2, pos2, idsAt(50, 50), 1000, "qs"); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("Complete(superseded) = %v, want ErrStaleRequest", err)
	}
	if err := w.Complete(r4, pos4, idsAt(100, 50), 1000, "qs"); err != nil {
		t.Fatalf("Complete(latest) = %v", err)
	}
	if w.Start() != 100 || w.Len() != 50 {
		t.Fatalf("after replace start %d len %d", w.Start(), w.Len())
	}
}

func TestWindowJumpEndAndStart(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, PrefetchAt: 10})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 10000, "qs")
	w.Move(3)

	if _, _, _, ok := w.JumpNeed(JumpEnd); !ok {
		t.Fatal("JumpNeed(JumpEnd) refused with total known")
	}
	if _, _, _, ok := NewWindow(Query{}, WindowConfig{}).JumpNeed(JumpEnd); ok {
		t.Fatal("JumpNeed allowed before any seed (total unknown)")
	}

	r, pos, limit, ok := w.JumpNeed(JumpEnd)
	if !ok || pos != 9950 || limit != 50 {
		t.Fatalf("JumpEnd = (%d,%d), want (9950,50)", pos, limit)
	}
	_ = w.Complete(r, pos, idsAt(9950, 50), 10000, "qs")
	w.SettleJump(JumpEnd)
	if _, id := w.Cursor(); id != "e9999" {
		t.Fatalf("after JumpEnd cursor = %q, want e9999", id)
	}
	if w.Start() != 9950 || w.Len() != 50 {
		t.Fatalf("after JumpEnd start %d len %d", w.Start(), w.Len())
	}

	r, pos, _, ok = w.JumpNeed(JumpStart)
	if !ok || pos != 0 {
		t.Fatalf("JumpStart = %d, want 0", pos)
	}
	_ = w.Complete(r, pos, idsAt(0, 50), 10000, "qs")
	w.SettleJump(JumpStart)
	if _, id := w.Cursor(); id != "e0000" {
		t.Fatalf("after JumpStart cursor = %q, want e0000", id)
	}
}

func TestWindowTrimForwardGrowthDropsFarEdge(t *testing.T) {
	// Cap 100: scrolling deep into a large result must keep memory bounded
	// (FR-D3) and preserve the cursor id (PLAN §4.1).
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, Cap: 100, PrefetchAt: 10})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 100000, "qs")

	for round := 0; round < 5; round++ {
		w.Move(w.Len() - 1 - 5) // near the forward edge
		r, pos, _, dir, ok := w.NextNeed()
		if !ok || dir != Forward {
			t.Fatalf("round %d: NextNeed = (%v,%d)", round, ok, dir)
		}
		if err := w.Complete(r, pos, idsAt(pos, 50), 100000, "qs"); err != nil {
			t.Fatalf("round %d: Complete: %v", round, err)
		}
	}
	if w.Len() > 100 {
		t.Fatalf("window grew to %d, cap 100", w.Len())
	}
	_, id := w.Cursor()
	if id == "" {
		t.Fatal("cursor lost during trimming")
	}
	// The window must still extend forward from its true absolute position.
	w.Move(w.Len() - 1)
	r, pos, _, dir, ok := w.NextNeed()
	if !ok || dir != Forward {
		t.Fatal("window stopped extending forward after trims")
	}
	_ = w.Complete(r, pos, idsAt(pos, 50), 100000, "qs")
	if w.Len() > 100 {
		t.Fatalf("post-trim len %d exceeds cap", w.Len())
	}
}

func TestWindowTrimNeverDropsCursorRow(t *testing.T) {
	// PrefetchAt 49 makes the cursor-on-row-0 window still want a forward
	// extension; the cap then trims the back edge because the cursor row
	// blocks the front.
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, Cap: 60, PrefetchAt: 49})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 1000, "qs")
	w.Move(0) // cursor parked on the first row

	r, pos, _, dir, ok := w.NextNeed() // forward extension to 100 rows
	if !ok || dir != Forward {
		t.Fatal("expected forward fetch")
	}
	_ = w.Complete(r, pos, idsAt(50, 50), 1000, "qs")

	if _, id := w.Cursor(); id != "e0000" {
		t.Fatalf("cursor row dropped by trim: %q", id)
	}
	// Back edge trimmed, cursor side kept.
	if w.Len() > 60 {
		t.Fatalf("len %d exceeds cap 60", w.Len())
	}
	if got := wantIDs(w.Start(), w.Len()); got != wantIDs(0, 60) {
		t.Fatalf("trimmed window = %s", got)
	}
}

func TestWindowTrimBackwardGrowthDropsFarEdge(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50, Cap: 60, PrefetchAt: 10})
	r, pos, _ := w.Seed(500)
	_ = w.Complete(r, pos, idsAt(500, 50), 1000, "qs")
	w.Move(2) // near the backward edge

	r, pos, _, dir, _ := w.NextNeed()
	if dir != Backward {
		t.Fatalf("dir = %d, want backward", dir)
	}
	_ = w.Complete(r, pos, idsAt(450, 50), 1000, "qs")

	if _, id := w.Cursor(); id != "e0502" {
		t.Fatalf("cursor moved to %q", id)
	}
	if w.Len() > 60 {
		t.Fatalf("len %d exceeds cap 60", w.Len())
	}
	// Travel was backward, so the back edge (rows the user is moving away
	// from) is shed and the fetched chunk is kept in full.
	if w.Start() != 450 {
		t.Fatalf("start = %d, want 450", w.Start())
	}
	if got := wantIDs(w.Start(), w.Len()); got != wantIDs(450, 60) {
		t.Fatalf("trimmed window = %s", got)
	}
}

func TestWindowDestroyedCursorLandsOnNeighbour(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 50, "qs")
	w.Move(7)
	if _, id := w.Cursor(); id != "e0007" {
		t.Fatalf("cursor = %q", id)
	}

	// A re-seeded result no longer contains e0007 (destroyed server-side):
	// the cursor lands on the row now occupying its neighbourhood.
	r, pos, _ = w.Seed(0)
	survivors := append(idsAt(0, 7), idsAt(8, 42)...)
	_ = w.Complete(r, pos, survivors, 49, "qs2")
	if _, id := w.Cursor(); id != "e0008" {
		t.Fatalf("destroyed cursor landed on %q, want neighbour e0008", id)
	}
	if w.Total() != 49 || w.QueryState() != "qs2" {
		t.Fatalf("total/state = %d/%q", w.Total(), w.QueryState())
	}
}

func TestWindowSeekID(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 50, "qs")

	if !w.SeekID("e0031") {
		t.Fatal("SeekID missed a present id")
	}
	if _, id := w.Cursor(); id != "e0031" {
		t.Fatalf("cursor = %q after seek", id)
	}
	if w.SeekID("e9999") {
		t.Fatal("SeekID claimed an absent id")
	}
	if _, id := w.Cursor(); id != "e0031" {
		t.Fatalf("failed seek moved cursor to %q", id)
	}
}

func TestWindowEmptyResult(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 50})
	r, pos, _ := w.Seed(0)
	if err := w.Complete(r, pos, nil, 0, "qs"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if w.Len() != 0 || w.Total() != 0 {
		t.Fatalf("len %d total %d, want 0/0", w.Len(), w.Total())
	}
	if _, id := w.Cursor(); id != "" {
		t.Fatalf("cursor = %q on empty window", id)
	}
	w.Move(3) // must not panic
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("NextNeed fired on an empty window")
	}
}

func TestWindowGapFallbackReplaces(t *testing.T) {
	// A server result that neither appends nor prepends (state shifted
	// mid-flight) must replace the window, never corrupt positions.
	w := NewWindow(Query{}, WindowConfig{Chunk: 50})
	r, pos, _ := w.Seed(0)
	_ = w.Complete(r, pos, idsAt(0, 50), 1000, "qs")

	w.Move(49) // near the forward edge
	r, pos, _, _, ok := w.NextNeed()
	if !ok {
		t.Fatal("expected a forward fetch request")
	}
	// Simulate the server having shifted under us: return a chunk that
	// overlaps instead of continuing at pos.
	overlap := idsAt(pos-10, 50)
	if err := w.Complete(r, pos-10, overlap, 1000, "qs"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if w.Start() != pos-10 {
		t.Fatalf("start = %d, want %d (replaced)", w.Start(), pos-10)
	}
	if w.Len() != 50 {
		t.Fatalf("len = %d, want 50", w.Len())
	}
}

func TestWindowRequestIDsMonotonic(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{Chunk: 10})
	seen := map[Request]bool{}
	last := Request(0)
	check := func(r Request) {
		if r <= last || seen[r] {
			t.Fatalf("request id %d not monotonic/unique (last %d)", r, last)
		}
		seen[r] = true
		last = r
	}
	r, pos, _ := w.Seed(0)
	check(r)
	_ = w.Complete(r, pos, idsAt(0, 10), 100, "qs")
	w.Move(9)
	r, _, _, _, ok := w.NextNeed()
	if !ok {
		t.Fatal("expected fetch")
	}
	check(r)
}

func TestWindowEmptyMoveOnUnseededWindow(t *testing.T) {
	w := NewWindow(Query{}, WindowConfig{})
	w.Move(5) // must not panic before any result arrives
	if w.Len() != 0 || w.Total() != -1 {
		t.Fatalf("unseeded window mutated: len %d total %d", w.Len(), w.Total())
	}
}
