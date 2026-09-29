package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

	mu        sync.Mutex
	mailboxes []mail.Mailbox
	mboxOrder []MailboxNode
	window    *Window
	// sort is the mailbox view's server-side order (FR-D8); thread and
	// search queries keep their own.
	sort []mail.SortCriterion
	// cursorRow is the index into rendered rows (incl. thread members);
	// cursorID is the selected message it tracks by id — row-set changes
	// (extensions, live patches, slide-ins) move rows under a raw index,
	// so snapshotLocked re-anchors cursorRow from cursorID (FR-D5).
	cursorRow int
	cursorID  mail.ID
	// saved holds the mailbox view while a search view is open (FR-F1):
	// Esc restores it with cursor position intact.
	saved *savedView
	// scan is the active fuzzy LIKE scan, if any (FR-F1 auto fallback);
	// scanGen supersedes it (bump = cancel).
	scan        *scanState
	scanGen     uint64
	summaries   map[mail.ID]mail.EmailSummary
	threads     map[mail.ID][]mail.ID // threadID → member ids, oldest first
	threadOrder []mail.ID             // thread cache insertion order (bounded)
	// threadSizes is the member count per thread behind the list's
	// expandable-row chevron (FR-D1): 0 = not yet known (the row renders
	// unmarked and the app asks for a size refresh), >1 = press Enter and
	// something comes out. Bounded with the window it was fetched for.
	threadSizes map[mail.ID]int
	expanded    map[mail.ID]bool
	bodies      *bodyCache
	body        *BodyView
	bodyLoading mail.ID
	version     uint64

	// Live-sync state (M2): state strings are the only sync truth
	// (FR-B5); fresh ids carry the new-mail highlight; overlays are the
	// FR-B7 plumbing; updates is the latest-wins broadcast to the UI.
	emailState   string
	mailboxState string
	status       Status
	fresh        map[mail.ID]time.Time
	overlay      map[mail.ID]*pendingOp
	overlayOrder []mail.ID
	identities   []mail.Identity
	updates      chan Snapshot
	liveOnce     sync.Once
	liveCfg      liveConfig

	// Contacts (M9, FR-L): a parallel in-memory store with its own state
	// strings, version and latest-wins channel — the mail Snapshot path
	// stays untouched. Lazy: populated by
	// LoadContacts, then kept fresh by reconcileContacts only while warm.
	contactsLoaded  bool
	contactsLoading bool
	contactBooks    []mail.AddressBook
	contacts        map[mail.ID]mail.Contact
	contactOrder    []mail.ID
	contactState    string
	bookState       string
	contactVersion  uint64
	contactErr      string
	contactUpdates  chan ContactSnapshot
}

// Config tunes an Engine.
type Config struct {
	// Window bounds the rolling query window (FR-D3).
	Window WindowConfig

	// BodyCache bounds the body LRU; 100 when zero (NFR-2).
	BodyCache int

	// PollInterval is the fallback poll period when push is unavailable or
	// exhausted (FR-B3); 60s when zero.
	PollInterval time.Duration

	// PushRetries is how many failed push attempts are tolerated before
	// falling back to polling (FR-B3); 3 when zero.
	PushRetries int

	// Sort is the mailbox view's server-side order (FR-D8); nil means
	// receivedAt descending. Applied to every mailbox query until
	// Engine.SetSort changes it.
	Sort []mail.SortCriterion
}

func (c Config) withDefaults() Config {
	if c.BodyCache <= 0 {
		c.BodyCache = 100
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 60 * time.Second
	}
	if c.PushRetries <= 0 {
		c.PushRetries = 3
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
	// ThreadSize is the thread's member count: 0 while unknown (the row
	// renders without a chevron until a size refresh lands), 1 for a
	// single-message thread, >1 for a thread Enter can expand (FR-D1).
	ThreadSize int
	Fresh      bool // arrived via live sync; highlighted until cleared
	// Account is the owning account id, stamped only on unified-view rows
	// (FR-A5); empty in single-account views. JMAP ids are unique per
	// account, so actions route by (account, id).
	Account string
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
	// OpenMailbox). Search views report their scope mailbox; an
	// all-mailbox search reports empty.
	ActiveMailbox mail.ID

	// ViewKey identifies the open view for app-side change detection:
	// "m:<mailbox>" for mailbox views, "s:<scope>:<filters>" for search
	// views (FR-F1). It changes on mailbox switches and on search
	// re-issues (scope toggle, new terms).
	ViewKey string

	// SearchActive reports that a search view is open over the mailbox
	// view (FR-F1).
	SearchActive bool

	// Scan is the fuzzy LIKE scan's progress (FR-F1 auto fallback):
	// non-nil while a scan is running over an empty server result.
	Scan *ScanProgress

	// Edge hints: more results exist beyond the materialised window in that
	// direction (the UI may show a loading hint while a fetch runs).
	LoadForward  bool
	LoadBackward bool

	// Body is the rendered body of the cursor message (nil while loading or
	// when nothing is selected). Its ID matches the cursor id.
	Body        *BodyView
	BodyLoading bool

	// NewAbove hints that fresh mail arrived above a window scrolled away
	// from position 0 (PLAN §4.1 case 3).
	NewAbove bool

	// Fresh lists ids that arrived via live sync and are still inside the
	// highlight TTL (the slide-in delight moment, PLAN §4.1 case 3).
	Fresh []mail.ID

	// Status is the live-sync state for the status line (FR-I5).
	Status Status
}

// BodyView is the preview-ready content of one message: text already
// converted from HTML when the source was HTML (FR-E2).
//
// Text is markdown when Styled is set — the UI styles it into the frame —
// and plain text otherwise. It is never what a reply quotes: the
// composer's quote comes from EmailBody.Text, which convertBody keeps in
// plain text no matter how the display was rendered.
type BodyView struct {
	ID          mail.ID
	Text        string
	Styled      bool
	Attachments []mail.Attachment
}

// NewEngine returns an engine driving p. Call Start to begin live sync.
func NewEngine(p mail.Provider, cfg Config) *Engine {
	c := cfg.withDefaults()
	return &Engine{
		p:              p,
		cfg:            c,
		sort:           c.Sort,
		summaries:      map[mail.ID]mail.EmailSummary{},
		threads:        map[mail.ID][]mail.ID{},
		threadSizes:    map[mail.ID]int{},
		expanded:       map[mail.ID]bool{},
		bodies:         newBodyCache(c.BodyCache),
		fresh:          map[mail.ID]time.Time{},
		overlay:        map[mail.ID]*pendingOp{},
		updates:        make(chan Snapshot, 1),
		contacts:       map[mail.ID]mail.Contact{},
		contactUpdates: make(chan ContactSnapshot, 1),
		liveCfg: liveConfig{
			pollInterval: c.PollInterval,
			pushRetries:  c.PushRetries,
			backoffBase:  defaultBackoffBase,
		},
		status: Status{Mode: ModeConnecting},
	}
}

// Updates exposes the latest-wins snapshot broadcast: every publish pushes
// the newest snapshot here (buffered 1, drained first). The app reads it
// from a Cmd so live changes repaint without any user action (FR-B2).
func (e *Engine) Updates() <-chan Snapshot { return e.updates }

// LoadMailboxes fetches the mailbox tree and publishes it. The UI can
// render the sidebar before any query resolves (NFR-3). The returned state
// string bootstraps Mailbox/changes reconciliation (FR-B5).
func (e *Engine) LoadMailboxes(ctx context.Context) error {
	list, err := e.p.Mailboxes(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mailboxes = list.Mailboxes
	e.mailboxState = list.State
	e.rebuildMailboxTreeLocked()
	e.publishLocked()
	return nil
}

// Identities returns the account's sendable identities fetched at startup
// (FR-B1); compose (M5) consumes them. Empty when the server lacks the
// submission capability (FR-A6).
func (e *Engine) Identities() []mail.Identity {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]mail.Identity(nil), e.identities...)
}

// LoadIdentities fetches identities over the network (FR-B1). Absence of
// the capability is not an error (FR-A6).
func (e *Engine) LoadIdentities(ctx context.Context) error {
	ids, err := e.p.Identities(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.identities = ids
	e.mu.Unlock()
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
// window (FR-C2). The page's Email/get state bootstraps Email/changes
// reconciliation (FR-B5). Opening a mailbox while a search view is open
// discards the search (choosing a mailbox is an explicit exit).
func (e *Engine) OpenMailbox(ctx context.Context, id mail.ID) error {
	e.mu.Lock()
	sort := e.sort
	e.mu.Unlock()
	return e.openView(ctx, Query{Filter: FilterSpec{MailboxID: id}, Sort: sort})
}

// SetSort re-queries the open mailbox view under a new server-side order
// (FR-D8): same filter, fresh window, cursor at the top — the same
// one-outstanding-query path OpenMailbox takes. The order sticks for
// every later mailbox open on this account. It is a no-op while no view
// is open; the app keeps it out of search views (their sort stays
// receivedAt descending).
func (e *Engine) SetSort(ctx context.Context, sort []mail.SortCriterion) error {
	e.mu.Lock()
	e.sort = sort
	if e.window == nil {
		e.mu.Unlock()
		return nil
	}
	q := e.window.query
	e.mu.Unlock()
	q.Sort = sort
	return e.openView(ctx, q)
}

// openView (re)opens a mailbox-shaped query: fresh window, reset cursor
// and transient state, first page fetched, snapshot published.
func (e *Engine) openView(ctx context.Context, q Query) error {
	e.mu.Lock()
	e.saved = nil
	e.cancelScanLocked()
	e.window = NewWindow(q, e.cfg.Window)
	e.expanded = map[mail.ID]bool{}
	e.cursorRow = 0
	e.cursorID = ""
	e.bodyLoading = ""
	e.body = nil
	e.fresh = map[mail.ID]time.Time{}
	r, pos, limit := e.window.Seed(0)
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return err
	}
	if st := handle.EmailState(); st != "" {
		e.emailState = st
	}
	e.absorbSummariesLocked(sums)
	e.publishLocked()
	return nil
}

// SearchSpec describes one search view (FR-F1..F3): content filters with
// AND semantics plus a scope mailbox. Zero time values are ignored; an
// empty ScopeMailbox means all mailboxes.
type SearchSpec struct {
	Text          string
	From          string
	To            string
	Subject       string
	After         time.Time
	Before        time.Time
	HasKeyword    string
	HasAttachment *bool
	ScopeMailbox  mail.ID
}

// savedView is the mailbox window parked while a search view is open.
type savedView struct {
	win       *Window
	cursorRow int
	cursorID  mail.ID
}

// Equal reports whether two specs describe the same query. The app uses
// it to skip re-issuing an unchanged search on Enter — an identical
// re-issue would restart an in-flight fuzzy scan from scratch.
func (s SearchSpec) Equal(o SearchSpec) bool {
	if s.Text != o.Text || s.From != o.From || s.To != o.To ||
		s.Subject != o.Subject || s.HasKeyword != o.HasKeyword ||
		s.ScopeMailbox != o.ScopeMailbox {
		return false
	}
	if !s.After.Equal(o.After) || !s.Before.Equal(o.Before) {
		return false
	}
	if (s.HasAttachment == nil) != (o.HasAttachment == nil) {
		return false
	}
	return s.HasAttachment == nil || *s.HasAttachment == *o.HasAttachment
}

// SearchOpen enters the search view (FR-F1): the mailbox window is parked
// with its cursor row, and a fresh window pages a new query built from the
// search spec. Re-issuing while already searching (new terms, scope
// toggle) replaces the search window without re-parking. The caller
// debounce-coalesces keystrokes; the engine coalesces anything that slips
// through via superseded-request bookkeeping.
func (e *Engine) SearchOpen(ctx context.Context, s SearchSpec) error {
	fs := FilterSpec{MailboxID: s.ScopeMailbox}
	fs.Search = &mail.SearchFilter{
		Text:          s.Text,
		From:          s.From,
		To:            s.To,
		Subject:       s.Subject,
		After:         s.After,
		Before:        s.Before,
		HasKeyword:    s.HasKeyword,
		HasAttachment: s.HasAttachment,
	}

	e.mu.Lock()
	if e.saved == nil {
		e.saved = &savedView{win: e.window, cursorRow: e.cursorRow, cursorID: e.cursorID}
	}
	e.cancelScanLocked()
	e.window = NewWindow(Query{Filter: fs}, e.cfg.Window)
	e.expanded = map[mail.ID]bool{}
	e.cursorRow = 0
	e.cursorID = ""
	e.bodyLoading = ""
	e.body = nil
	e.fresh = map[mail.ID]time.Time{}
	r, pos, limit := e.window.Seed(0)
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return err
	}
	if st := handle.EmailState(); st != "" {
		e.emailState = st
	}
	e.absorbSummariesLocked(sums)
	// Fuzzy fallback (FR-F1): the server matched whole tokens only and
	// found nothing — a partial word in any text-ish field — so scan the
	// scope's headers client-side and stream matches in. Non-zero results
	// take the fast path and never scan; exact-only searches (keyword,
	// attachment, dates) have no substring semantics to fall back to.
	if e.window.Total() == 0 && s.scannable() {
		e.startScanLocked(ctx, s)
	}
	e.publishLocked()
	return nil
}

// SearchClose leaves the search view and restores the parked mailbox
// window with its cursor position intact (FR-F1). Purely local (NFR-1).
// The restored window is marked dirty so the next prefetch re-anchors it
// around the cursor id: live changes that landed while the search was open
// fold in there instead of guessing positions (FR-B5).
func (e *Engine) SearchClose() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.saved == nil {
		return e.snapshotLocked()
	}
	e.window = e.saved.win
	e.cursorRow = e.saved.cursorRow
	e.cursorID = e.saved.cursorID
	e.saved = nil
	e.cancelScanLocked()
	e.expanded = map[mail.ID]bool{}
	e.body, e.bodyLoading = nil, ""
	e.fresh = map[mail.ID]time.Time{}
	if e.window != nil {
		e.window.invalidate()
	}
	e.publishLocked()
	return e.snapshotLocked()
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
		e.cursorID = rows[e.cursorRow].ID
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
			e.cursorID = id
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

// ErrNoUnread reports that a jump-unread scan reached the end of what it
// can load without finding a message (FR-D7).
var ErrNoUnread = errors.New("no more unread")

// maxUnreadScanHops bounds one jump-unread press: chunk fetches inside a
// single call (25 × 50 = 1,250 rows), so a fully-read mailbox makes a
// bounded, sequential number of round trips (FR-K4).
const maxUnreadScanHops = 25

// FindUnread returns the index of the first unread row scanning from
// `from` inclusive in dir (+1/−1); ok is false when the range holds none.
// Shared by the engine scan and the app's merged (unified) view.
func FindUnread(rows []Row, from, dir int) (int, bool) {
	if dir > 0 {
		for i := max(from, 0); i < len(rows); i++ {
			if !rows[i].Summary.Keywords.Has("$seen") {
				return i, true
			}
		}
		return 0, false
	}
	for i := min(from, len(rows)-1); i >= 0; i-- {
		if !rows[i].Summary.Keywords.Has("$seen") {
			return i, true
		}
	}
	return 0, false
}

// JumpUnread moves the cursor to the next/prev unread message (FR-D7).
// The scan runs engine-side so it can extend the window while it looks:
// unlike Prefetch it ignores the cursor-proximity threshold (the cursor
// deliberately stays put until something unread is found) via
// NextNeedForce. Returns ErrNoUnread when the reachable window holds
// nothing — the app surfaces that as the non-fatal notice.
func (e *Engine) JumpUnread(ctx context.Context, dir int) error {
	if dir == 0 {
		return nil
	}
	scanDir := 1
	if dir < 0 {
		scanDir = -1
	}
	for hop := 0; ; hop++ {
		e.mu.Lock()
		if e.window == nil {
			e.mu.Unlock()
			return nil
		}
		rows := e.renderedRowsLocked()
		if i, ok := FindUnread(rows, e.cursorRow+dir, dir); ok {
			e.cursorRow = i
			e.cursorID = rows[i].ID
			e.syncWindowCursorLocked(rows)
			e.publishLocked()
			e.mu.Unlock()
			return nil
		}
		if e.scan != nil || hop >= maxUnreadScanHops {
			e.mu.Unlock()
			return ErrNoUnread
		}
		r, pos, limit, ok := e.window.NextNeedForce(scanDir)
		if !ok {
			e.mu.Unlock()
			return ErrNoUnread // no more results in that direction
		}
		spec := e.querySpecLocked(pos, limit)
		e.mu.Unlock()

		handle, sums, err := e.p.OpenQuery(ctx, spec)
		if err != nil {
			return err
		}

		e.mu.Lock()
		if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
			e.mu.Unlock()
			if err == ErrStaleRequest {
				continue // live change mid-scan: rescan the merged rows
			}
			return err
		}
		e.absorbSummariesLocked(sums)
		// The merge may have prepended rows under the cursor index —
		// re-anchor before the next scan pass.
		e.repairCursorLocked(e.renderedRowsLocked())
		e.publishLocked()
		e.mu.Unlock()
	}
}

// Prefetch satisfies any pending edge extension the window wants (FR-D3).
// A dirty window (live changes, triage mutations) re-anchors around the
// cursor id first (FR-B5): absolute positions are stale, so extending
// before re-anchoring would merge into the wrong shape. The window's
// outstanding bookkeeping coalesces concurrent callers; stale results are
// discarded.
func (e *Engine) Prefetch(ctx context.Context) error {
	e.mu.Lock()
	if e.window == nil {
		e.mu.Unlock()
		return nil
	}
	if e.window.Dirty() {
		if r, anchor, offset, limit, ok := e.window.ReanchorNeed(); ok {
			spec := e.querySpecLocked(0, limit)
			spec.AnchorID = anchor
			spec.AnchorOffset = offset
			e.mu.Unlock()
			return e.completeWindowFetch(ctx, r, spec)
		}
	}
	r, pos, limit, _, ok := e.window.NextNeed()
	if !ok {
		e.mu.Unlock()
		return nil
	}
	spec := e.querySpecLocked(pos, limit)
	e.mu.Unlock()
	return e.completeWindowFetch(ctx, r, spec)
}

// completeWindowFetch runs one window query and installs the result,
// dropping stale responses silently (mailbox switch mid-flight).
func (e *Engine) completeWindowFetch(ctx context.Context, r Request, spec mail.QuerySpec) error {
	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		if err == ErrStaleRequest {
			return nil // silently drop
		}
		return err
	}
	e.absorbSummariesLocked(sums)
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
	case q.Filter.Search != nil:
		// Search view: content filters ride along; MailboxID is the
		// scope, empty meaning all mailboxes.
		spec.Search = q.Filter.Search
		if q.Filter.MailboxID != "" {
			spec.MailboxID = q.Filter.MailboxID
		}
	default:
		spec.MailboxID = q.Filter.MailboxID
	}
	if len(q.Sort) > 0 {
		spec.Sort = append(spec.Sort, q.Sort...)
	}
	return spec
}

// Jump re-anchors the window at the top or bottom of the result (FR-D3).
// It is a no-op while a fuzzy scan owns the view — the scan result list
// has no server-side positions to jump within.
func (e *Engine) Jump(ctx context.Context, t JumpTarget) error {
	e.mu.Lock()
	if e.window == nil || e.scan != nil {
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
	if err := e.window.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return err
	}
	e.absorbSummariesLocked(sums)
	e.window.SettleJump(t)
	e.alignCursorWithWindowLocked()
	e.publishLocked()
	return nil
}

// repairCursorLocked re-anchors cursorRow from the tracked message id
// after the row set changed (FR-D5). When the tracked message is gone
// (destroyed), the window's own neighbour choice takes over; as a last
// resort the raw index clamps. Caller holds mu.
func (e *Engine) repairCursorLocked(rows []Row) {
	if len(rows) == 0 {
		e.cursorRow = 0
		return
	}
	find := func(id mail.ID) bool {
		for i, r := range rows {
			if r.ID == id {
				e.cursorRow = i
				return true
			}
		}
		return false
	}
	if e.cursorID != "" && find(e.cursorID) {
		return
	}
	if id := e.window.CursorID(); id != "" && find(id) {
		e.cursorID = id
		return
	}
	e.cursorRow = min(max(e.cursorRow, 0), len(rows)-1)
}

// alignCursorWithWindowLocked moves the rendered cursor to the row of the
// window cursor id (after jumps/re-anchors) and tracks it by id. Caller
// holds mu.
func (e *Engine) alignCursorWithWindowLocked() {
	id := e.window.CursorID()
	if id == "" {
		return
	}
	rows := e.renderedRowsLocked()
	for i, r := range rows {
		if r.ID == id {
			e.cursorRow = i
			e.cursorID = id
			return
		}
	}
}

// ToggleThread expands or collapses the thread of target — the message the
// user pressed (FR-D2). Expansion is a sub-list — window math is untouched
// (PLAN §4.1) — and an empty target falls back to the engine cursor, which
// single-account views track exactly. The app passes the row it rendered:
// in the unified view no engine cursor ever moves (PLAN §4.3), so a
// cursor-relative toggle would expand a different account's row 0 and put
// the chevron on a message nobody pressed. Scan results are flat rows;
// threads do not expand there.
func (e *Engine) ToggleThread(ctx context.Context, target mail.ID) error {
	e.mu.Lock()
	if e.window == nil || e.scan != nil {
		e.mu.Unlock()
		return nil
	}
	rows := e.renderedRowsLocked()
	idx := -1
	if target == "" {
		if e.cursorRow >= 0 && e.cursorRow < len(rows) {
			idx = e.cursorRow
		}
	} else {
		for i, r := range rows {
			if r.ID == target {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		e.mu.Unlock()
		return nil
	}
	id := rows[idx].ID
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
	members, cached := e.threads[sum.ThreadID]
	if cached && len(members) <= 1 {
		// Everything this thread could show is the row itself (its other
		// members were destroyed): no chevron over nothing — and the count
		// the list is acting on was stale, so correct it (FR-D1).
		e.rememberThreadSizeLocked(sum.ThreadID, len(members))
		e.mu.Unlock()
		return nil
	}
	win := e.window
	e.mu.Unlock()

	var sums []mail.EmailSummary
	if !cached {
		var err error
		if members, sums, err = e.loadThread(ctx, sum.ThreadID); err != nil {
			return err
		}
		if len(members) <= 1 {
			// The server says this thread is the row itself. Record it:
			// a chevron that promised a reply has to go (FR-D1), whatever
			// view the fetch outlived.
			e.mu.Lock()
			e.rememberThreadSizeLocked(sum.ThreadID, len(members))
			e.mu.Unlock()
			return nil
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	// The window is swapped wholesale by a mailbox switch or a search
	// opening/closing while the fetch was in flight; planting an
	// expansion from the old view would mark whichever row of the new
	// one happens to share this threadId — the chevron on a message the
	// user never pressed.
	if e.window != win || e.scan != nil {
		return nil
	}
	if !cached {
		// Remember before absorbing: a member that lives outside the
		// window survives eviction only because the thread cache keeps it
		// (PLAN §3), so the cache must exist first.
		e.rememberThreadLocked(sum.ThreadID, members)
		e.absorbSummariesLocked(sums)
	}
	if len(e.threads[sum.ThreadID]) <= 1 {
		// Re-checked under the lock: the cache can shrink while the fetch
		// is out — a concurrent destroy drops members, the LRU evicts the
		// entry — and an expansion with nothing beneath it is a chevron
		// over empty space.
		return nil
	}
	e.expanded[sum.ThreadID] = true
	e.publishLocked()
	return nil
}

// loadThread fetches a thread's member ids and their summaries from the
// provider (FR-D2), oldest-first. It runs without mu held; the caller
// re-checks the view, caches the ids, then absorbs the summaries.
func (e *Engine) loadThread(ctx context.Context, threadID mail.ID) ([]mail.ID, []mail.EmailSummary, error) {
	m, err := e.p.Threads(ctx, []mail.ID{threadID})
	if err != nil {
		return nil, nil, err
	}
	ids := m[threadID]
	if len(ids) <= 1 {
		return ids, nil, nil // nothing beneath the pressed row
	}
	sums, err := e.p.FetchSummaries(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	return ids, sums, nil
}

// LoadBody fetches (or reuses) the body for id and converts HTML to text
// (FR-E2, FR-D4). The result lands in the snapshot only if id is still the
// cursor message when it arrives.
func (e *Engine) LoadBody(ctx context.Context, id mail.ID) error {
	if id == "" {
		return nil
	}
	if bv, ok := e.cachedBody(id); ok {
		e.mu.Lock()
		e.bodyLoading = ""
		e.setBodyLocked(id, bv)
		e.mu.Unlock()
		return nil
	}

	e.mu.Lock()
	e.bodyLoading = id
	e.publishLocked()
	e.mu.Unlock()

	bv, err := e.fetchBody(ctx, id)
	if err != nil {
		e.mu.Lock()
		e.bodyLoading = ""
		e.publishLocked()
		e.mu.Unlock()
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.bodyLoading = ""
	e.setBodyLocked(id, bv)
	return nil
}

// BodyFor returns the body for id from the LRU or the server, with no
// cursor gate and no publish: the unified view's cursor is app-side (PLAN
// §4.3), so the engine has no idea which row the caller wants — the
// caller keys the request itself and tracks its own in-flight state. The
// fetch still lands in the LRU (NFR-2), so a revisit costs no round trip.
func (e *Engine) BodyFor(ctx context.Context, id mail.ID) (*BodyView, error) {
	if id == "" {
		return nil, nil
	}
	if bv, ok := e.cachedBody(id); ok {
		return bv, nil
	}
	return e.fetchBody(ctx, id)
}

// cachedBody returns the body view for id out of the LRU, marking it
// recently used. The cache has no lock of its own: every get/put rides
// the engine mutex, so overlapping loads on one engine can't collide on
// it (and no lock is held across a render).
func (e *Engine) cachedBody(id mail.ID) (*BodyView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	text, styled, body, ok := e.bodies.get(id)
	if !ok {
		return nil, false
	}
	return &BodyView{ID: id, Text: text, Styled: styled, Attachments: body.Attachments}, true
}

// convertBody renders a fetched body once for each of its two readers.
// The preview wants markdown when the source was HTML; the composer wants
// plain text to quote. body.Text is rewritten to the plain form, so
// everything downstream that quotes a body gets text and never markup.
// A body that already carries text/plain is displayed as-is (FR-E2's
// preference) and is not styled.
func convertBody(body *mail.EmailBody) (display string, styled bool) {
	if body.Text != "" {
		return body.Text, false
	}
	if body.HTML == "" {
		return "", false
	}
	body.Text = mailtext.HTMLToText(body.HTML)
	return mailtext.HTMLToMarkdown(body.HTML), true
}

// fetchBody fetches id from the server, converts HTML for display (FR-E2)
// and caches the result (NFR-2). No lock is held across the network call.
func (e *Engine) fetchBody(ctx context.Context, id mail.ID) (*BodyView, error) {
	body, err := e.p.FetchBody(ctx, id)
	if err != nil {
		return nil, err
	}
	display, styled := convertBody(&body)

	e.mu.Lock()
	e.bodies.put(id, display, styled, body)
	e.mu.Unlock()
	return &BodyView{ID: id, Text: display, Styled: styled, Attachments: body.Attachments}, nil
}

// setBodyLocked installs the body view when id still matches the cursor.
// The caller must hold mu.
func (e *Engine) setBodyLocked(id mail.ID, bv *BodyView) {
	if e.window == nil || e.cursorIDLocked() != id {
		return
	}
	e.body = bv
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
// beneath their header. During a fuzzy scan the rows are the scan's flat
// match list instead. Caller holds mu.
func (e *Engine) renderedRowsLocked() []Row {
	if e.scan != nil {
		rows := make([]Row, 0, len(e.scan.matches))
		for _, id := range e.scan.matches {
			sum, ok := e.summaries[id]
			if !ok {
				continue // match summary not yet stored; lands next publish
			}
			if op, pending := e.overlay[id]; pending && op.ov.Destroy {
				continue // pending delayed destroy: hidden until commit/cancel
			}
			// Flat view: no thread chrome to wear and nothing to expand
			// (FR-F1), so -1 tells the list and the size refresh alike
			// that this row is not "unknown", it is "never".
			rows = append(rows, Row{ID: id, Summary: sum, ThreadSize: -1})
		}
		return rows
	}
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
		if op, pending := e.overlay[id]; pending && op.ov.Destroy {
			continue // pending delayed destroy: hidden until commit/cancel
		}
		r := Row{ID: id, Summary: sum, ThreadSize: e.threadSizes[sum.ThreadID]}
		if _, fresh := e.fresh[id]; fresh {
			r.Fresh = true
		}
		if e.expanded[sum.ThreadID] {
			members := e.threadMembersLocked(sum.ThreadID, id)
			if len(members) == 0 {
				// Nothing renders beneath: a single-message thread, or
				// every other member is gone. A chevron over empty space
				// is a lie, so the row stays plain (FR-D2).
				rows = append(rows, r)
				continue
			}
			// The header (the thread's collapsed representative — its
			// newest member) renders first; remaining members follow,
			// oldest-first, so the header is never duplicated.
			r.ThreadHeader = true
			rows = append(rows, r)
			rows = append(rows, members...)
			continue
		}
		rows = append(rows, r)
	}
	return rows
}

// threadMembersLocked builds the rows rendered beneath an expanded
// thread's header: every cached member except the header itself, in cache
// order (oldest first), skipping ids whose summary has not arrived and
// rows already carrying a pending destroy. Caller holds mu.
func (e *Engine) threadMembersLocked(threadID, header mail.ID) []Row {
	memberIDs := e.threads[threadID]
	out := make([]Row, 0, max(len(memberIDs)-1, 0))
	for _, mid := range memberIDs {
		if mid == header {
			continue
		}
		ms, ok := e.summaries[mid]
		if !ok {
			continue
		}
		if op, pending := e.overlay[mid]; pending && op.ov.Destroy {
			continue
		}
		out = append(out, Row{ID: mid, Summary: ms, ThreadMember: true, ThreadSize: e.threadSizes[threadID]})
	}
	return out
}

// absorbSummariesLocked stores page summaries (id-keyed, last wins), then
// evicts everything the visible state no longer needs (PLAN §3: summaries
// are evicted when a window trims, so memory stays bounded no matter how
// far the user scrolls — NFR-2). Bodies have their own LRU and are not
// touched here. Pending overlays re-apply on top of server truth (FR-B7).
func (e *Engine) absorbSummariesLocked(sums []mail.EmailSummary) {
	for _, s := range sums {
		e.summaries[s.ID] = e.reapplyOverlayLocked(s)
	}
	e.evictSummariesLocked()
}

// evictSummariesLocked drops summaries outside the window and outside any
// cached thread (so re-expanding a recently viewed thread needs no
// refetch) and outside any pending overlay (an optimistic row must not
// lose its patch mid-flight). While a search view is open, the parked
// mailbox window's summaries stay resident too — Esc restores that view
// instantly, so its rows must render without a refetch (FR-F1, NFR-1).
// Caller holds mu.
func (e *Engine) evictSummariesLocked() {
	keep := map[mail.ID]bool{}
	keepThreads := map[mail.ID]bool{}
	noteThread := func(id mail.ID) {
		keep[id] = true
		if s, ok := e.summaries[id]; ok && s.ThreadID != "" {
			keepThreads[s.ThreadID] = true
		}
	}
	if e.window != nil {
		for _, id := range e.window.IDs() {
			noteThread(id)
		}
	}
	if e.saved != nil && e.saved.win != nil {
		for _, id := range e.saved.win.IDs() {
			noteThread(id)
		}
	}
	if e.scan != nil {
		for _, id := range e.scan.matches {
			keep[id] = true
		}
	}
	for tid, members := range e.threads {
		keepThreads[tid] = true
		for _, mid := range members {
			keep[mid] = true
		}
	}
	for id := range e.overlay {
		noteThread(id)
	}
	// Thread sizes ride with the rows that showed them (FR-D1): a thread
	// no live row belongs to has nothing left to mark, so its count goes
	// too — the next refresh re-learns it if the row comes back.
	for tid := range e.threadSizes {
		if !keepThreads[tid] {
			delete(e.threadSizes, tid)
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
	e.threadSizes[threadID] = len(members)
	e.threadOrder = append(e.threadOrder, threadID)
	for len(e.threadOrder) > maxThreadCache {
		oldest := e.threadOrder[0]
		e.threadOrder = e.threadOrder[1:]
		delete(e.threads, oldest)
		delete(e.expanded, oldest)
	}
}

// publishLocked bumps the snapshot version and broadcasts the newest
// snapshot to the UI (latest wins: any unread stale snapshot is dropped
// first). Cheap enough to call on every state change; the app repaints
// from live updates without user action (FR-B2).
func (e *Engine) publishLocked() {
	e.version++
	snap := e.snapshotLocked()
	select {
	case <-e.updates:
	default:
	}
	select {
	case e.updates <- snap:
	default:
	}
}

// viewKeyLocked identifies the open view for app-side change detection
// (FR-F1): "m:<mailbox>" for mailbox views, "s:<scope>:<fields>" for
// search views. Caller holds mu.
func (e *Engine) viewKeyLocked() string {
	if e.window == nil {
		return ""
	}
	q := e.window.query
	if q.Filter.Search != nil {
		s := q.Filter.Search
		attach := s.HasAttachment != nil && *s.HasAttachment
		return fmt.Sprintf("s:%s:%s|%s|%s|%s|%s|%s|%s|%t",
			q.Filter.MailboxID, s.Text, s.From, s.To, s.Subject,
			s.After.UTC().Format(time.RFC3339), s.Before.UTC().Format(time.RFC3339),
			s.HasKeyword, attach)
	}
	return "m:" + string(q.Filter.MailboxID)
}

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
		ViewKey:       e.viewKeyLocked(),
		SearchActive:  e.saved != nil,
		Status:        e.status,
	}
	e.expireFreshLocked()
	if e.window != nil {
		snap.NewAbove = e.window.NewAbove()
	}
	if len(e.fresh) > 0 {
		for id := range e.fresh {
			snap.Fresh = append(snap.Fresh, id)
		}
	}
	if e.window == nil {
		return snap
	}
	if e.scan != nil {
		// Fuzzy scan owns the view: matches stream in newest-first and
		// the total is progressive until the scan completes (FR-F1).
		snap.Total = len(e.scan.matches)
		snap.Start = 0
		snap.Scan = &ScanProgress{
			Active:  e.scan.scanned < e.scan.total || e.scan.total < 0,
			Scanned: e.scan.scanned,
			Total:   e.scan.total,
		}
		rows := e.renderedRowsLocked()
		snap.Cursor = min(max(e.cursorRow, 0), max(len(rows)-1, 0))
		snap.Rows = rows
		cursor := e.cursorIDLocked()
		if e.body != nil && cursor == e.body.ID {
			snap.Body = e.body
		}
		snap.BodyLoading = e.bodyLoading != "" && e.bodyLoading == cursor
		return snap
	}
	snap.Total = e.window.Total()
	snap.Start = e.window.Start()

	rows := e.renderedRowsLocked()
	// FR-D5 id repair: extensions, live patches, and slide-ins move rows
	// under a raw index — re-anchor the selection on its tracked message.
	e.repairCursorLocked(rows)
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

// FreshTTL bounds how long a live-arrival highlight lives; the app also
// clears it on a timer and on cursor movement.
const FreshTTL = 5 * time.Second

// expireFreshLocked drops highlight entries past their TTL. Caller holds mu.
func (e *Engine) expireFreshLocked() {
	now := time.Now()
	for id, at := range e.fresh {
		if now.Sub(at) > FreshTTL {
			delete(e.fresh, id)
		}
	}
}

// activeMailboxLocked reads the open mailbox from the window's query.
func (e *Engine) activeMailboxLocked() mail.ID {
	if e.window == nil {
		return ""
	}
	return e.window.query.Filter.MailboxID
}
