package sync

import (
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// newSeededWindow returns a window holding the chunk at absolute position
// pos, total-size result, queryState qs-1.
func newSeededWindow(pos, n, total int) *Window {
	w := NewWindow(Query{Filter: FilterSpec{MailboxID: "mb"}}, WindowConfig{Chunk: 50, Cap: 200})
	r, _, _ := w.Seed(pos)
	if err := w.Complete(r, pos, idsAt(pos, n), total, "qs-1"); err != nil {
		panic(err)
	}
	return w
}

func TestWindowRemoveIDsKeepsCursorByID(t *testing.T) {
	w := newSeededWindow(0, 10, 10)
	w.Move(3) // on e0003

	if got := w.RemoveIDs([]mail.ID{"e0001"}); got != 1 {
		t.Fatalf("RemoveIDs = %d, want 1", got)
	}
	if w.Len() != 9 || w.Total() != 9 {
		t.Fatalf("len %d total %d, want 9/9", w.Len(), w.Total())
	}
	if _, id := w.Cursor(); id != "e0003" {
		t.Fatalf("cursor = %q, want e0003 (id preservation, FR-D5)", id)
	}
	if !w.Dirty() {
		t.Fatal("live destroy must mark the window dirty")
	}
	// NextNeed refuses to extend a dirty window; re-anchor is the path.
	if _, _, _, _, ok := w.NextNeed(); ok {
		t.Fatal("NextNeed must refuse a dirty window")
	}
}

func TestWindowRemoveIDsLandsCursorOnNeighbour(t *testing.T) {
	w := newSeededWindow(0, 5, 5)
	w.Move(2) // on e0002
	if got := w.RemoveIDs([]mail.ID{"e0002"}); got != 1 {
		t.Fatalf("RemoveIDs = %d, want 1", got)
	}
	// The destroyed cursor id lands on the row that took its neighbourhood.
	if _, id := w.Cursor(); id != "e0003" {
		t.Fatalf("cursor = %q, want e0003 (neighbour, PLAN §4.1)", id)
	}
}

func TestWindowInsertTopSlidesIn(t *testing.T) {
	w := newSeededWindow(0, 5, 5)
	w.Move(1) // on e0001
	if !w.InsertTop("e-new") {
		t.Fatal("InsertTop refused at start 0")
	}
	if w.Len() != 6 || w.Total() != 6 {
		t.Fatalf("len %d total %d, want 6/6", w.Len(), w.Total())
	}
	if w.IDs()[0] != "e-new" {
		t.Fatalf("head = %q, want e-new", w.IDs()[0])
	}
	// The user stays on their message, not the new row (FR-D5).
	if _, id := w.Cursor(); id != "e0001" {
		t.Fatalf("cursor = %q, want e0001", id)
	}
	if !w.Dirty() {
		t.Fatal("insert must mark the window dirty")
	}
}

func TestWindowInsertTopScrolledShowsHint(t *testing.T) {
	w := newSeededWindow(50, 50, 200)
	if w.InsertTop("e-new") {
		t.Fatal("InsertTop must refuse when scrolled away from position 0")
	}
	if !w.NewAbove() {
		t.Fatal("scrolled insert must set the new-above hint")
	}
	if w.Len() != 50 {
		t.Fatalf("rows changed: %d", w.Len())
	}
}

func TestWindowReanchorNeedAnchorsCursor(t *testing.T) {
	w := newSeededWindow(40, 50, 200)
	w.Move(10) // on e0050

	r, anchor, offset, limit, ok := w.ReanchorNeed()
	if !ok {
		t.Fatal("ReanchorNeed refused a clean dirty window")
	}
	if anchor != "e0050" {
		t.Fatalf("anchor = %q, want e0050", anchor)
	}
	if offset != -10 {
		t.Fatalf("offset = %d, want -10 (chunk starts at the old window head)", offset)
	}
	if limit != 50 {
		t.Fatalf("limit = %d, want chunk 50", limit)
	}
	// The anchored replace completes with a server-computed position; the
	// cursor lands back on its id.
	if err := w.Complete(r, 40, idsAt(40, 50), 200, "qs-2"); err != nil {
		t.Fatalf("Complete(reanchor): %v", err)
	}
	if _, id := w.Cursor(); id != "e0050" {
		t.Fatalf("cursor after re-anchor = %q, want e0050", id)
	}
	if w.Dirty() {
		t.Fatal("re-anchor complete must clear dirty")
	}
	if w.QueryState() != "qs-2" {
		t.Fatalf("queryState = %q, want qs-2", w.QueryState())
	}
	// Extensions work again.
	w.Move(45)
	r2, pos, _, _, ok := w.NextNeed()
	if !ok || pos != 90 {
		t.Fatalf("NextNeed after re-anchor = (ok %v pos %d)", ok, pos)
	}
	if err := w.Complete(r2, 90, idsAt(90, 50), 200, "qs-2"); err != nil {
		t.Fatalf("Complete(extend): %v", err)
	}
	if w.Len() != 100 {
		t.Fatalf("len after extension = %d, want 100", w.Len())
	}
}

func TestWindowAnchorNeedForcesReplace(t *testing.T) {
	w := newSeededWindow(0, 10, 10)
	w.Move(7) // fullResync anchors at the window's own cursor id
	r := w.AnchorNeed("e0007")
	if err := w.Complete(r, 7, idsAt(7, 3), 10, "qs-9"); err != nil {
		t.Fatalf("Complete(anchor): %v", err)
	}
	if w.Start() != 7 || w.Len() != 3 {
		t.Fatalf("start %d len %d, want 7/3", w.Start(), w.Len())
	}
	if _, id := w.Cursor(); id != "e0007" {
		t.Fatalf("cursor = %q, want e0007 (anchored re-query, FR-B5)", id)
	}
}

func TestWindowQueryStateMismatchMarksDirty(t *testing.T) {
	w := newSeededWindow(0, 10, 100)
	w.Move(5)
	r, pos, _, _, ok := w.NextNeed()
	if !ok || pos != 10 {
		t.Fatalf("NextNeed = (ok %v pos %d)", ok, pos)
	}
	// The result returns a queryState the window has never seen: the
	// server's result set shifted under us (FR-B5).
	if err := w.Complete(r, 10, idsAt(10, 50), 100, "qs-CHANGED"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !w.Dirty() {
		t.Fatal("queryState mismatch on extension must mark the window dirty")
	}
}
