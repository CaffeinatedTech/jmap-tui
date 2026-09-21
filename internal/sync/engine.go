package sync

import (
	"context"
	"sync"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
)

// Engine is the per-account orchestrator (PLAN §1): it owns the mailbox
// tree, the rolling window, summaries, thread expansion state, and the body
// LRU, and drives a mail.Provider. Methods are blocking and safe for
// concurrent use — the app layer wraps them in tea.Cmd goroutines so the
// UI thread never waits on the network (NFR-1). For M1 the engine mutex is
// held across provider calls, which serialises Cmds; finer-grained locking
// arrives with push (M2).
type Engine struct {
	p   mail.Provider
	cfg Config

	mu          sync.Mutex
	mailboxes   []mail.Mailbox
	mboxOrder   []MailboxNode
	window      *Window
	cursorRow   int // index into rendered rows (incl. thread members)
	summaries   map[mail.ID]mail.EmailSummary
	threads     map[mail.ID][]mail.ID // threadID → member ids, oldest first
	threadOrder []mail.ID             // thread cache insertion order (bounded)
	expanded    map[mail.ID]bool
	bodies      *bodyCache
	body        *BodyView
	bodyLoading mail.ID
	version     uint64
}

// Config tunes an Engine.
type Config struct {
	// Window bounds the rolling query window (FR-D3).
	Window WindowConfig

	// BodyCache bounds the body LRU; 100 when zero (NFR-2).
	BodyCache int
}

func (c Config) withDefaults() Config {
	if c.BodyCache <= 0 {
		c.BodyCache = 100
	}
	return c
}

// Row is one rendered line of the message list: either a collapsed thread
// representative, or (when ThreadMember) a member of an expanded thread.
type Row struct {
	ID           mail.ID
	Summary      mail.EmailSummary
	ThreadHeader bool // has an expanded thread beneath it
	ThreadMember bool // rendered inside an expanded thread
}

// MailboxNode is one sidebar entry with its rendered tree depth.
type MailboxNode struct {
	Mailbox mail.Mailbox
	Depth   int
}

// Snapshot is the immutable view the UI renders. Everything it contains is
// already in memory, so first render from a new snapshot is local work only
// (FR-D6, NFR-1).
type Snapshot struct {
	Version uint64

	Mailboxes []MailboxNode
	Rows      []Row
	Cursor    int
	Total     int // full result size; -1 before the first query
	Start     int // absolute position of Rows' collapsed origin

	// ActiveMailbox is the id of the open mailbox (empty before the first
	// OpenMailbox).
	ActiveMailbox mail.ID

	// Edge hints: more results exist beyond the materialised window in that
	// direction (the UI may show a loading hint while a fetch runs).
	LoadForward  bool
	LoadBackward bool

	// Body is the rendered body of the cursor message (nil while loading or
	// when nothing is selected). Its ID matches the cursor id.
	Body        *BodyView
	BodyLoading bool
}

// BodyView is the preview-ready content of one message: text already
// converted from HTML when the source was HTML (FR-E2).
type BodyView struct {
	ID          mail.ID
	Text        string
	Attachments []mail.Attachment
}

// NewEngine returns an engine driving p.
func NewEngine(p mail.Provider, cfg Config) *Engine {
	c := cfg.withDefaults()
	return &Engine{
		p:         p,
		cfg:       c,
		summaries: map[mail.ID]mail.EmailSummary{},
		threads:   map[mail.ID][]mail.ID{},
		expanded:  map[mail.ID]bool{},
		bodies:    newBodyCache(c.BodyCache),
	}
}

// LoadMailboxes fetches the mailbox tree and publishes it. The UI can
// render the sidebar before any query resolves (NFR-3).
func (e *Engine) LoadMailboxes(ctx context.Context) error {
	mbs, err := e.p.Mailboxes(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mailboxes = mbs
	e.rebuildMailboxTreeLocked()
	e.publishLocked()
	return nil
}

// rebuildMailboxTreeLocked renders the flat mailbox list into depth-ordered
// nodes for the sidebar (FR-C1): server sort order within each parent.
func (e *Engine) rebuildMailboxTreeLocked() {
	children := map[mail.ID][]mail.ID{}
	byID := map[mail.ID]mail.Mailbox{}
	for _, mb := range e.mailboxes {
		children[mb.ParentID] = append(children[mb.ParentID], mb.ID)
		byID[mb.ID] = mb
	}
	// The provider returns mailboxes in server sort order; preserve that
	// order within each sibling group.
	out := make([]MailboxNode, 0, len(e.mailboxes))
	var walk func(parent mail.ID, depth int)
	walk = func(parent mail.ID, depth int) {
		for _, id := range children[parent] {
			mb := byID[id]
			out = append(out, MailboxNode{Mailbox: mb, Depth: depth})
			walk(id, depth+1)
		}
	}
	walk("", 0)
	// Orphans (parent missing from the list) render at the top level in
	// server order after the real roots.
	seen := map[mail.ID]bool{}
	for _, n := range out {
		seen[n.Mailbox.ID] = true
	}
	for _, mb := range e.mailboxes {
		if seen[mb.ID] || mb.ParentID == "" {
			continue
		}
		if _, parentExists := byID[mb.ParentID]; !parentExists {
			out = append(out, MailboxNode{Mailbox: mb, Depth: 0})
		}
	}
	e.mboxOrder = out
}

// OpenMailbox issues a fresh query for the mailbox and resets the list
// window (FR-C2).
func (e *Engine) OpenMailbox(ctx context.Context, id mail.ID) error {
	e.mu.Lock()
	e.window = NewWindow(Query{Filter: FilterSpec{MailboxID: id}}, e.cfg.Window)
	e.expanded = map[mail.ID]bool{}
	e.cursorRow = 0
	e.bodyLoading = ""
	e.body = nil
	r, pos, limit := e.window.Seed(0)
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.absorbSummariesLocked(sums)
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return err
	}
	e.publishLocked()
	return nil
}

// MoveCursor shifts the rendered-row cursor and returns the fresh snapshot.
// The cursor spans expanded thread members too (FR-D2); the underlying
// window cursor follows the owning thread header so window math (edges,
// prefetch) stays anchored. Purely local — no network (NFR-1).
func (e *Engine) MoveCursor(delta int) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := e.renderedRowsLocked()
	if len(rows) > 0 {
		e.cursorRow = min(max(e.cursorRow+delta, 0), len(rows)-1)
		e.syncWindowCursorLocked(rows)
	}
	e.publishLocked()
	return e.snapshotLocked()
}

// syncWindowCursorLocked aligns the window cursor with the thread header
// owning the rendered cursor row.
func (e *Engine) syncWindowCursorLocked(rows []Row) {
	if e.window == nil || len(rows) == 0 {
		return
	}
	header := rows[e.cursorRow].ID
	if rows[e.cursorRow].ThreadMember {
		for i := e.cursorRow; i >= 0; i-- {
			if rows[i].ThreadHeader {
				header = rows[i].ID
				break
			}
		}
	}
	e.window.SeekID(header)
}

// SeekCursor puts the cursor on the given message id if rendered.
func (e *Engine) SeekCursor(id mail.ID) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := e.renderedRowsLocked()
	for i, r := range rows {
		if r.ID == id {
			e.cursorRow = i
			e.syncWindowCursorLocked(rows)
			break
		}
	}
	e.publishLocked()
	return e.snapshotLocked()
}

// CursorID returns the selected message id (empty when nothing selected).
func (e *Engine) CursorID() mail.ID {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := e.renderedRowsLocked()
	if e.cursorRow < 0 || e.cursorRow >= len(rows) {
		return ""
	}
	return rows[e.cursorRow].ID
}

// Prefetch satisfies any pending edge extension the window wants (FR-D3).
// It is a no-op when nothing is needed. The window's outstanding
// bookkeeping coalesces concurrent callers; stale results are discarded.
func (e *Engine) Prefetch(ctx context.Context) error {
	e.mu.Lock()
	if e.window == nil {
		e.mu.Unlock()
		return nil
	}
	r, pos, limit, _, ok := e.window.NextNeed()
	if !ok {
		e.mu.Unlock()
		return nil
	}
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.absorbSummariesLocked(sums)
	// A swapped window (mailbox change mid-flight) never matches r, so the
	// stale result is rejected here (PLAN §4.1).
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		if err == ErrStaleRequest {
			return nil // silently drop
		}
		return err
	}
	e.publishLocked()
	return nil
}

// querySpecLocked maps the window's query into provider terms for a page.
// The caller must hold mu.
func (e *Engine) querySpecLocked(position, limit int) mail.QuerySpec {
	var spec mail.QuerySpec
	if e.window == nil {
		return spec
	}
	q := e.window.query
	spec = mail.QuerySpec{
		CollapseThreads: true,
		Position:        position,
		Limit:           limit,
	}
	switch {
	case q.Filter.ThreadID != "":
		spec.ThreadID = q.Filter.ThreadID
	default:
		spec.MailboxID = q.Filter.MailboxID
	}
	if len(q.Sort) > 0 {
		spec.Sort = append(spec.Sort, q.Sort...)
	}
	return spec
}

// Jump re-anchors the window at the top or bottom of the result (FR-D3).
func (e *Engine) Jump(ctx context.Context, t JumpTarget) error {
	e.mu.Lock()
	if e.window == nil {
		e.mu.Unlock()
		return nil
	}
	r, pos, limit, ok := e.window.JumpNeed(t)
	if !ok {
		e.mu.Unlock()
		return nil
	}
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.absorbSummariesLocked(sums)
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return err
	}
	e.window.SettleJump(t)
	e.alignCursorWithWindowLocked()
	e.publishLocked()
	return nil
}

// alignCursorWithWindowLocked moves the rendered cursor to the row of the
// window cursor id (after jumps/re-anchors). Caller holds mu.
func (e *Engine) alignCursorWithWindowLocked() {
	id := e.window.CursorID()
	if id == "" {
		return
	}
	rows := e.renderedRowsLocked()
	for i, r := range rows {
		if r.ID == id {
			e.cursorRow = i
			return
		}
	}
}

// ToggleThread expands or collapses the cursor message's thread in place
// (FR-D2). Expansion is a sub-list — window math is untouched (PLAN §4.1).
func (e *Engine) ToggleThread(ctx context.Context) error {
	e.mu.Lock()
	if e.window == nil {
		e.mu.Unlock()
		return nil
	}
	rows := e.renderedRowsLocked()
	if e.cursorRow < 0 || e.cursorRow >= len(rows) {
		e.mu.Unlock()
		return nil
	}
	id := rows[e.cursorRow].ID
	sum, ok := e.summaries[id]
	if !ok || id == "" {
		e.mu.Unlock()
		return nil
	}
	if e.expanded[sum.ThreadID] {
		delete(e.expanded, sum.ThreadID)
		e.publishLocked()
		e.mu.Unlock()
		return nil
	}
	if _, cached := e.threads[sum.ThreadID]; !cached {
		// Fetch thread members oldest-first for natural reading order.
		// The mutex is released for the network hop and re-taken after.
		spec := mail.QuerySpec{
			ThreadID: sum.ThreadID,
			Sort:     []mail.SortCriterion{{Property: "receivedAt"}},
			Limit:    1000,
		}
		e.mu.Unlock()
		handle, sums, err := e.p.OpenQuery(ctx, spec)
		if err != nil {
			return err
		}
		e.mu.Lock()
		e.absorbSummariesLocked(sums)
		e.rememberThreadLocked(sum.ThreadID, append([]mail.ID(nil), handle.IDs()...))
	}
	e.expanded[sum.ThreadID] = true
	e.publishLocked()
	e.mu.Unlock()
	return nil
}

// LoadBody fetches (or reuses) the body for id and converts HTML to text
// (FR-E2, FR-D4). The result lands in the snapshot only if id is still the
// cursor message when it arrives.
func (e *Engine) LoadBody(ctx context.Context, id mail.ID) error {
	if id == "" {
		return nil
	}
	if text, _, ok := e.bodies.get(id); ok {
		e.mu.Lock()
		e.bodyLoading = ""
		e.setBodyLocked(id, text, nil)
		e.mu.Unlock()
		return nil
	}

	e.mu.Lock()
	e.bodyLoading = id
	e.publishLocked()
	e.mu.Unlock()

	body, err := e.p.FetchBody(ctx, id)
	if err != nil {
		e.mu.Lock()
		e.bodyLoading = ""
		e.publishLocked()
		e.mu.Unlock()
		return err
	}

	text := body.Text
	if text == "" && body.HTML != "" {
		text = mailtext.HTMLToText(body.HTML)
	}
	e.bodies.put(id, text, body.HTML)

	e.mu.Lock()
	defer e.mu.Unlock()
	e.bodyLoading = ""
	e.setBodyLocked(id, text, body.Attachments)
	return nil
}

// setBodyLocked installs the body view when id still matches the cursor.
// The caller must hold mu.
func (e *Engine) setBodyLocked(id mail.ID, text string, atts []mail.Attachment) {
	if e.window == nil || e.cursorIDLocked() != id {
		return
	}
	e.body = &BodyView{ID: id, Text: text, Attachments: atts}
	e.publishLocked()
}

// cursorIDLocked returns the rendered-row cursor id. Caller holds mu.
func (e *Engine) cursorIDLocked() mail.ID {
	rows := e.renderedRowsLocked()
	if e.cursorRow < 0 || e.cursorRow >= len(rows) {
		return ""
	}
	return rows[e.cursorRow].ID
}

// renderedRowsLocked builds the rendered row list: collapsed window ids,
// with expanded threads splicing their remaining members (oldest-first)
// beneath their header. Caller holds mu.
func (e *Engine) renderedRowsLocked() []Row {
	if e.window == nil {
		return nil
	}
	ids := e.window.IDs()
	rows := make([]Row, 0, len(ids))
	for _, id := range ids {
		sum, ok := e.summaries[id]
		if !ok {
			continue // summary not yet arrived; row appears when it does
		}
		r := Row{ID: id, Summary: sum}
		if e.expanded[sum.ThreadID] {
			// The header (the thread's collapsed representative — its
			// newest member) renders first; remaining members follow,
			// oldest-first, so the header is never duplicated.
			r.ThreadHeader = true
			rows = append(rows, r)
			for _, mid := range e.threads[sum.ThreadID] {
				if mid == id {
					continue
				}
				ms, ok := e.summaries[mid]
				if !ok {
					continue
				}
				rows = append(rows, Row{ID: mid, Summary: ms, ThreadMember: true})
			}
			continue
		}
		rows = append(rows, r)
	}
	return rows
}

// absorbSummariesLocked stores page summaries (id-keyed, last wins), then
// evicts everything the visible state no longer needs (PLAN §3: summaries
// are evicted when a window trims, so memory stays bounded no matter how
// far the user scrolls — NFR-2). Bodies have their own LRU and are not
// touched here.
func (e *Engine) absorbSummariesLocked(sums []mail.EmailSummary) {
	for _, s := range sums {
		e.summaries[s.ID] = s
	}
	e.evictSummariesLocked()
}

// evictSummariesLocked drops summaries outside the window and outside any
// cached thread (so re-expanding a recently viewed thread needs no
// refetch). Caller holds mu.
func (e *Engine) evictSummariesLocked() {
	keep := map[mail.ID]bool{}
	if e.window != nil {
		for _, id := range e.window.IDs() {
			keep[id] = true
		}
	}
	for _, members := range e.threads {
		for _, mid := range members {
			keep[mid] = true
		}
	}
	for id := range e.summaries {
		if !keep[id] {
			delete(e.summaries, id)
		}
	}
}

// maxThreadCache bounds the remembered thread expansions (NFR-2); the
// oldest entry is dropped when exceeded.
const maxThreadCache = 64

// rememberThreadLocked caches a thread's member ids with LRU-style
// eviction. Caller holds mu.
func (e *Engine) rememberThreadLocked(threadID mail.ID, members []mail.ID) {
	e.threads[threadID] = members
	e.threadOrder = append(e.threadOrder, threadID)
	for len(e.threadOrder) > maxThreadCache {
		oldest := e.threadOrder[0]
		e.threadOrder = e.threadOrder[1:]
		delete(e.threads, oldest)
		delete(e.expanded, oldest)
	}
}

// publishLocked bumps the snapshot version.
func (e *Engine) publishLocked() { e.version++ }

// Snapshot returns the current immutable view.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// snapshotLocked builds the immutable view. The caller must hold mu.
func (e *Engine) snapshotLocked() Snapshot {
	snap := Snapshot{
		Version:       e.version,
		Mailboxes:     e.mboxOrder,
		Total:         -1,
		ActiveMailbox: e.activeMailboxLocked(),
	}
	if e.window == nil {
		return snap
	}
	snap.Total = e.window.Total()
	snap.Start = e.window.Start()

	rows := e.renderedRowsLocked()
	snap.Cursor = min(max(e.cursorRow, 0), max(len(rows)-1, 0))
	snap.Rows = rows

	fwd, bwd := e.window.AtEdge()
	snap.LoadForward = fwd
	snap.LoadBackward = bwd

	cursor := e.cursorIDLocked()
	if e.body != nil && cursor == e.body.ID {
		snap.Body = e.body
	}
	snap.BodyLoading = e.bodyLoading != "" && e.bodyLoading == cursor
	return snap
}

// activeMailboxLocked reads the open mailbox from the window's query.
func (e *Engine) activeMailboxLocked() mail.ID {
	if e.window == nil {
		return ""
	}
	return e.window.query.Filter.MailboxID
}
