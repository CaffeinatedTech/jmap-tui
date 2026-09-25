// Package sync keeps the in-memory mail state for one account: the mailbox
// tree, rolling query windows (PLAN §4.1), message summaries, and a body
// LRU. It is the only layer above mail.Provider that decides what to fetch;
// the UI consumes published snapshots and never waits on the network (NFR-1).
package sync

import (
	"errors"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// ErrStaleRequest reports a Complete for a request the window has already
// superseded. The engine discards it silently (PLAN §4.1).
var ErrStaleRequest = errors.New("sync: stale window request result")

// FilterSpec scopes a server-side query. Mailbox browsing sets MailboxID
// (or ThreadID for thread expansion); search views set Search (FR-F1),
// with MailboxID acting as the scope — empty meaning all mailboxes.
type FilterSpec struct {
	MailboxID mail.ID
	ThreadID  mail.ID

	// Search carries the content filters of a search view; nil in
	// mailbox-browsing views. ThreadID is unused while set.
	Search *mail.SearchFilter
}

// Query is the server-side query a window pages through.
type Query struct {
	Filter FilterSpec
	Sort   []mail.SortCriterion
}

// WindowConfig bounds a rolling window (FR-D3). Zero fields fall back to
// defaults: 50-row chunks, a 2,000-row cap, prefetch when the cursor is
// within 10 rows of a materialised edge. (A 0 PrefetchAt cannot express
// "never prefetch"; set 1 to prefetch only from the edge row itself.)
type WindowConfig struct {
	Chunk      int
	Cap        int
	PrefetchAt int
}

// Defaults for WindowConfig (FR-D3, PLAN §4.1).
const (
	DefaultChunk      = 50
	DefaultCap        = 2000
	DefaultPrefetchAt = 10
)

func (c WindowConfig) withDefaults() WindowConfig {
	if c.Chunk <= 0 {
		c.Chunk = DefaultChunk
	}
	if c.Cap <= 0 {
		c.Cap = DefaultCap
	}
	if c.PrefetchAt <= 0 {
		c.PrefetchAt = DefaultPrefetchAt
	}
	return c
}

// Request is a monotonic handle for one outstanding server query. Only the
// most recent request issued for a window is allowed to complete; superseded
// results are discarded by the engine (PLAN §4.1 debounce rule).
type Request uint64

// Direction of a needed fetch relative to the materialised window.
const (
	Backward = -1
	Forward  = 1
)

// JumpTarget selects a re-anchor destination (FR-D3 deep jump).
type JumpTarget int

// Re-anchor destinations.
const (
	JumpStart JumpTarget = iota
	JumpEnd
)

// Window is the rolling view over one server-side query result: an ordered
// slice of ids materialised from absolute position Start, plus cursor math
// that tracks ids rather than row indexes. It is pure state — no network,
// no clocks — so every rule of PLAN §4.1 is table-testable.
//
// All methods are single-goroutine; the engine serialises access.
type Window struct {
	query Query
	cfg   WindowConfig

	ids        []mail.ID
	start      int // absolute position of ids[0] in the full result
	total      int // size of the full result set; -1 until first seed
	queryState string

	cursorID mail.ID // id-based cursor (PLAN §4.1 position preservation)
	cursor   int     // index into ids; re-derived after every mutation

	next        Request // monotonic request counter
	outstanding Request // most recent issued request; 0 = none
	pending     pendingKind

	// dirty records that live changes mutated the window (or the server
	// reported an unseen queryState), so the materialised positions are
	// stale relative to the server's result set. The next extension is
	// replaced by a cursor-anchored re-query (FR-B5, FR-D5).
	dirty bool

	// newAbove records that fresh mail arrived above a window that is
	// scrolled away from position 0: it cannot slide in, so the UI shows a
	// hint until the next replace brings the window honest again.
	newAbove bool
}

// pendingKind records what the outstanding request will do on Complete:
// replace the window (seed, re-anchor) or extend it (edge prefetch, with a
// direction so trimming drops the edge opposite to the user's travel).
type pendingKind struct {
	replace bool
	dir     int
}

// NewWindow returns an empty window for the query. Total is unknown (-1)
// until the first Complete.
func NewWindow(q Query, cfg WindowConfig) *Window {
	return &Window{
		query: q,
		cfg:   cfg.withDefaults(),
		total: -1,
	}
}

// Seed issues the request for the initial chunk at absolute position:
// position 0 for a fresh mailbox view; JumpStart/JumpEnd re-anchors use the
// positions JumpNeed computes.
func (w *Window) Seed(position int) (Request, int, int) {
	return w.issue(position, w.cfg.Chunk, true, 0)
}

// JumpNeed computes the re-anchor query for a deep jump (FR-D3): a fresh
// chunk at the top or bottom of the result that replaces the window content
// on Complete. ok=false when total is still unknown.
func (w *Window) JumpNeed(t JumpTarget) (r Request, position, limit int, ok bool) {
	if w.total < 0 {
		return 0, 0, 0, false
	}
	switch t {
	case JumpStart:
		r, position, limit = w.issue(0, w.cfg.Chunk, true, 0)
	case JumpEnd:
		pos := w.total - w.cfg.Chunk
		if pos < 0 {
			pos = 0
		}
		r, position, limit = w.issue(pos, w.cfg.Chunk, true, 0)
	default:
		return 0, 0, 0, false
	}
	return r, position, limit, true
}

// SettleJump places the cursor after a completed re-anchor: first row for
// JumpStart, last row for JumpEnd.
func (w *Window) SettleJump(t JumpTarget) {
	if len(w.ids) == 0 {
		return
	}
	switch t {
	case JumpStart:
		w.cursor = 0
	case JumpEnd:
		w.cursor = len(w.ids) - 1
	}
	w.cursorID = w.ids[w.cursor]
}

// NextNeed reports the next edge extension the window wants, honouring the
// prefetch threshold: the cursor within PrefetchAt rows of an edge that has
// more results on the far side (FR-D3). When both edges qualify, the edge
// nearer the cursor is fetched first. ok=false when nothing is needed —
// including when a fetch is already outstanding (one outstanding query per
// window; the engine coalesces and debounces).
func (w *Window) NextNeed() (r Request, position, limit, dir int, ok bool) {
	// A dirty window re-anchors before it extends: the engine drives that
	// via ReanchorNeed (FR-B5).
	if w.outstanding != 0 || w.dirty || len(w.ids) == 0 || w.total < 0 {
		return 0, 0, 0, 0, false
	}
	moreForward := w.start+len(w.ids) < w.total
	moreBackward := w.start > 0
	nearForward := w.cfg.PrefetchAt - (len(w.ids) - 1 - w.cursor)
	nearBackward := w.cfg.PrefetchAt - w.cursor

	forward := moreForward && nearForward >= 0 && (nearForward >= nearBackward || !moreBackward)
	backward := moreBackward && nearBackward >= 0 && !forward

	switch {
	case forward:
		r, position, limit = w.issue(w.start+len(w.ids), w.cfg.Chunk, false, Forward)
		return r, position, limit, Forward, true
	case backward:
		// Clamp at the top of the result set: fetch exactly the missing
		// prefix so the chunk never overlaps the window.
		pos := max(0, w.start-w.cfg.Chunk)
		r, position, limit = w.issue(pos, w.start-pos, false, Backward)
		return r, position, limit, Backward, true
	default:
		return 0, 0, 0, 0, false
	}
}

// NextNeedForce is NextNeed without the cursor-proximity threshold: the
// unread-jump scan (FR-D7) deliberately extends rows the cursor has not
// approached yet. dir > 0 requests the forward side, dir < 0 the backward
// side; ok=false when that side is exhausted or a request is already
// outstanding. Ordinary scrolling keeps using NextNeed (FR-D3).
func (w *Window) NextNeedForce(dir int) (r Request, position, limit int, ok bool) {
	if w.outstanding != 0 || w.dirty || len(w.ids) == 0 || w.total < 0 {
		return 0, 0, 0, false
	}
	if dir > 0 {
		if w.start+len(w.ids) >= w.total {
			return 0, 0, 0, false
		}
		r, position, limit = w.issue(w.start+len(w.ids), w.cfg.Chunk, false, Forward)
		return r, position, limit, true
	}
	if w.start <= 0 {
		return 0, 0, 0, false
	}
	pos := max(0, w.start-w.cfg.Chunk)
	r, position, limit = w.issue(pos, w.start-pos, false, Backward)
	return r, position, limit, true
}

// issue allocates a request id and marks it outstanding. Any previously
// outstanding request is superseded and its result will be discarded.
func (w *Window) issue(position, limit int, replace bool, dir int) (Request, int, int) {
	w.next++
	w.outstanding = w.next
	w.pending = pendingKind{replace: replace, dir: dir}
	return w.next, position, limit
}

// Complete installs a query result. Results from superseded requests are
// rejected with ErrStaleRequest so the engine can drop them silently.
// position is the absolute offset of ids within the full result.
func (w *Window) Complete(r Request, position int, ids []mail.ID, total int, queryState string) error {
	if r != w.outstanding {
		return ErrStaleRequest
	}
	w.outstanding = 0
	if total < 0 {
		total = w.total
	}
	if w.queryState != "" && queryState != w.queryState && !w.pending.replace {
		// The server's result set shifted under an extension (FR-B5):
		// the ids still merge, but positions are suspect — re-anchor on
		// the next need instead of guessing. Set after the reset below so
		// the flag survives this Complete.
		defer func() { w.dirty = true }()
	}
	w.total = total
	w.queryState = queryState
	w.dirty = false
	w.newAbove = false

	if len(ids) == 0 {
		return nil
	}
	if w.pending.replace {
		w.ids = append(w.ids[:0], ids...)
		w.start = position
	} else {
		w.extend(position, ids)
	}
	// The cursor index must be valid on the merged slice before trim math
	// runs; a second rederive afterwards is a destroyed-id safety net.
	w.rederiveCursor()
	w.trim()
	w.rederiveCursor()
	return nil
}

// extend folds a contiguous chunk into the window: appended at the end for
// forward chunks, prepended for backward chunks. Any other shape (overlap,
// gap — possible only if the server result set shifted mid-flight) falls
// back to replacing with the new chunk rather than corrupting absolute
// positions.
func (w *Window) extend(position int, ids []mail.ID) {
	switch {
	case position == w.start+len(w.ids): // forward, contiguous
		w.ids = append(w.ids, ids...)
	case position < w.start && position+len(ids) == w.start: // backward, contiguous
		w.ids = append(append([]mail.ID{}, ids...), w.ids...)
		w.start = position
	default:
		w.ids = append(w.ids[:0], ids...)
		w.start = position
	}
}

// trim drops rows beyond the cap, silently (PLAN §4.1). The edge opposite
// to the last extension direction goes first — content the user is moving
// away from — then, if the cursor row blocks that side, the other edge.
// The cursor's own row is never dropped; the cap is otherwise always
// enforced so memory stays bounded (FR-D3, NFR-2).
func (w *Window) trim() {
	excess := len(w.ids) - w.cfg.Cap
	if excess <= 0 {
		return
	}
	first, second := w.trimOrder()
	excess -= w.dropRows(first, excess)
	if excess > 0 {
		w.dropRows(second, excess)
	}
}

// trimOrder picks the edges to shed: opposite the travel direction when one
// is known, otherwise the edge farther from the cursor.
func (w *Window) trimOrder() (first, second int) {
	const dropFront, dropBack = 0, 1
	switch {
	case w.pending.dir == Forward:
		return dropFront, dropBack
	case w.pending.dir == Backward:
		return dropBack, dropFront
	case w.cursor > len(w.ids)/2:
		return dropFront, dropBack
	default:
		return dropBack, dropFront
	}
}

// dropRows sheds up to want rows from one edge, never past the cursor row,
// and returns how many were dropped.
func (w *Window) dropRows(edge, want int) int {
	if want <= 0 {
		return 0
	}
	if edge == 0 { // front
		drop := min(want, w.cursor)
		w.ids = w.ids[drop:]
		w.start += drop
		w.cursor -= drop
		return drop
	}
	keep := max(w.cursor+1, len(w.ids)-want)
	dropped := len(w.ids) - keep
	w.ids = w.ids[:keep]
	return dropped
}

// rederiveCursor re-derives the cursor index from cursorID; a destroyed id
// lands on the row that now occupies its neighbourhood (clamped index).
func (w *Window) rederiveCursor() {
	if len(w.ids) == 0 {
		w.cursor, w.cursorID = 0, ""
		return
	}
	for i, id := range w.ids {
		if id == w.cursorID {
			w.cursor = i
			return
		}
	}
	w.cursor = min(max(w.cursor, 0), len(w.ids)-1)
	w.cursorID = w.ids[w.cursor]
}

// Move shifts the cursor by delta, clamped to the materialised range.
func (w *Window) Move(delta int) {
	if len(w.ids) == 0 {
		return
	}
	w.cursor = min(max(w.cursor+delta, 0), len(w.ids)-1)
	w.cursorID = w.ids[w.cursor]
}

// SeekID puts the cursor on the row with the given id. It reports false when
// the id is not materialised (the cursor is left untouched).
func (w *Window) SeekID(id mail.ID) bool {
	for i, cand := range w.ids {
		if cand == id {
			w.cursor = i
			w.cursorID = id
			return true
		}
	}
	return false
}

// Cursor returns the selected row's index and id.
func (w *Window) Cursor() (int, mail.ID) {
	if len(w.ids) == 0 {
		return 0, ""
	}
	return w.cursor, w.ids[w.cursor]
}

// CursorID returns the id under the cursor (empty when the window is empty).
func (w *Window) CursorID() mail.ID {
	if len(w.ids) == 0 {
		return ""
	}
	return w.ids[w.cursor]
}

// IDs returns the materialised id slice in order. Callers must not mutate it.
func (w *Window) IDs() []mail.ID { return w.ids }

// Start is the absolute position of IDs()[0] in the full result set.
func (w *Window) Start() int { return w.start }

// Len is the number of materialised rows.
func (w *Window) Len() int { return len(w.ids) }

// Total is the full result size, or -1 before the first Complete.
func (w *Window) Total() int { return w.total }

// QueryState is the queryState of the last installed result (FR-B5).
func (w *Window) QueryState() string { return w.queryState }

// AtEdge reports whether the cursor is within PrefetchAt rows of a
// materialised edge with more results beyond (a loading hint for the UI).
func (w *Window) AtEdge() (forward, backward bool) {
	if len(w.ids) == 0 || w.total < 0 {
		return false, false
	}
	forward = len(w.ids)-1-w.cursor < w.cfg.PrefetchAt && w.start+len(w.ids) < w.total
	backward = w.cursor < w.cfg.PrefetchAt && w.start > 0
	return forward, backward
}

// --- live-change patching (M2, PLAN §4.1 case 1/3) ---

// invalidate marks the window dirty after a live patch and discards any
// in-flight fetch: its absolute positions predate the mutation, so its
// result would merge into the wrong shape. The next extension is a
// cursor-anchored re-query instead.
func (w *Window) invalidate() {
	w.dirty = true
	w.next++
	w.outstanding = 0
}

// Dirty reports whether live changes have mutated the window since the last
// server-complete result (the engine re-anchors before extending).
func (w *Window) Dirty() bool { return w.dirty }

// NewAbove reports that fresh mail arrived above a window scrolled away
// from position 0 (a UI hint until the next re-anchor).
func (w *Window) NewAbove() bool { return w.newAbove }

// RemoveIDs drops the given ids from the materialised window (live
// destroys, PLAN §4.1 case 1) and returns how many rows went. Total shrinks
// by that count; the cursor re-derives onto its neighbour when its own id
// was destroyed, preserving the reading position by id (FR-D5).
func (w *Window) RemoveIDs(ids []mail.ID) int {
	if len(ids) == 0 || len(w.ids) == 0 {
		return 0
	}
	gone := make(map[mail.ID]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}
	kept := w.ids[:0]
	removed := 0
	for _, id := range w.ids {
		if gone[id] {
			removed++
			continue
		}
		kept = append(kept, id)
	}
	if removed == 0 {
		return 0
	}
	w.ids = kept
	if w.total >= removed {
		w.total -= removed
	}
	w.invalidate()
	w.rederiveCursor()
	return removed
}

// InsertTop slides a fresh id in at the top of the result when the window
// sits at position 0 — the inbox-like, sorted-by-date-desc case (PLAN §4.1
// case 3). It reports false when the window is scrolled deeper and the id
// cannot slide in (the caller falls back to the new-above hint).
func (w *Window) InsertTop(id mail.ID) bool {
	if w.start != 0 {
		w.newAbove = true
		w.invalidate()
		return false
	}
	w.ids = append([]mail.ID{id}, w.ids...)
	if w.total >= 0 {
		w.total++
	}
	// The cursor id now sits one row deeper; rederive keeps the user on
	// the same message (FR-D5) or clamps safely on an empty window.
	w.invalidate()
	w.trim()
	w.rederiveCursor()
	return true
}

// MarkNewAbove records the new-above hint without inserting (window scrolled
// away from the top when live mail arrived).
func (w *Window) MarkNewAbove() {
	if !w.newAbove {
		w.newAbove = true
		w.invalidate()
	}
}

// ReanchorNeed issues the replace request that re-syncs a dirty window
// around the cursor id (FR-B5, PLAN §4.1 case 2): the engine sends
// Email/query with anchor=cursorID and anchorOffset=offset, so the fresh
// chunk begins where the old window began and the cursor lands on the same
// message regardless of how absolute positions shifted. ok=false when a
// fetch is already outstanding or nothing is materialised.
func (w *Window) ReanchorNeed() (r Request, anchor mail.ID, offset, limit int, ok bool) {
	if w.outstanding != 0 || len(w.ids) == 0 || w.cursorID == "" {
		return 0, "", 0, 0, false
	}
	limit = w.cfg.Chunk
	offset = -w.cursor
	r, _, _ = w.issue(0, limit, true, 0)
	return r, w.cursorID, offset, limit, true
}

// AnchorNeed issues a replace request anchored at an arbitrary id — the
// full re-query path for cannotCalculateChanges (PLAN §4.1 case 2), where
// the cursor id is the only honest position left.
func (w *Window) AnchorNeed(id mail.ID) Request {
	w.next++
	w.outstanding = w.next
	w.pending = pendingKind{replace: true}
	return w.next
}
