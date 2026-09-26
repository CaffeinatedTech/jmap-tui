package sync

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// Mode is the live-sync connection state shown in the status line
// (FR-I5).
type Mode string

// Sync states: connecting (initial), push (EventSource live, FR-B2), and
// poll (60s /changes fallback, FR-B3).
const (
	ModeConnecting Mode = "connecting"
	ModePush       Mode = "push"
	ModePoll       Mode = "poll"
)

// Status is the per-account sync state for the status line (FR-I5).
type Status struct {
	Mode      Mode
	LastSync  time.Time
	LastError string
	Attempts  int // consecutive push failures since the last success
}

// Live-sync policy knobs (FR-B3): reconnect attempts before the poll
// fallback engages, exponential backoff with a 30s cap (FR-A3), and a
// bounded /changes drain per reconcile pass.
const (
	defaultBackoffBase = 1 * time.Second
	backoffMax         = 30 * time.Second
	maxChangeLoops     = 20
	// probeCap bounds how many out-of-window updated ids are fetched to
	// detect new mail for the slide-in (PLAN §4.1 case 3); bulk external
	// changes beyond it wait for the next re-anchor.
	probeCap = 32
)

// liveConfig is the resolved live-sync policy. Exported Config fields feed
// the first two; tests shrink the backoff to keep suites fast.
type liveConfig struct {
	pollInterval time.Duration
	pushRetries  int
	backoffBase  time.Duration
}

// Start launches the per-account sync loop (PLAN §4.2): one EventSource
// stream with reconnect/backoff, falling back to 60s /changes polling after
// PushRetries failed attempts, and auto-upgrading back to push whenever a
// reconnect succeeds. Idempotent; the loop ends when ctx is cancelled.
//
// State-string bootstrapping happens through the normal load path
// (LoadMailboxes, OpenMailbox); the loop skips reconciliation for types
// whose state string has not been learned yet.
func (e *Engine) Start(ctx context.Context) {
	e.liveOnce.Do(func() {
		go e.run(ctx)
	})
}

// run is the sync loop: push → backoff → push …, or poll forever when push
// is unsupported or the retry budget is exhausted (FR-B3).
func (e *Engine) run(ctx context.Context) {
	fails := 0
	pushSupported := true
	for {
		if ctx.Err() != nil {
			return
		}
		if pushSupported && fails < e.liveCfg.pushRetries {
			ch, stop := e.p.Subscribe(ctx)
			if ch == nil {
				// No push support advertised (FR-A6): poll permanently,
				// no point asking again.
				pushSupported = false
			} else {
				e.setMode(ModePush)
				if e.pumpPush(ctx, ch, stop) {
					return // account context cancelled
				}
				fails++
				e.mu.Lock()
				e.status.Attempts = fails
				e.status.LastError = "push stream lost, reconnecting"
				e.publishLocked()
				e.mu.Unlock()
				// Between streams the footer must not claim "live" (FR-I5).
				e.setMode(ModeConnecting)
				if !sleepCtx(ctx, e.backoff(fails)) {
					return
				}
				continue
			}
		}
		e.setMode(ModePoll)
		e.pollOnce(ctx)
		fails = 0 // each poll cycle retries the push upgrade
		if !sleepCtx(ctx, e.liveCfg.pollInterval) {
			return
		}
	}
}

// backoff grows exponentially from the base and caps at 30s with up to 20%
// jitter so a fleet of clients never reconnects in lockstep (FR-A3).
func (e *Engine) backoff(fails int) time.Duration {
	d := e.liveCfg.backoffBase
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= backoffMax {
			break
		}
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d + time.Duration(rand.Int64N(int64(d/5)+1))
}

// pumpPush drains the EventSource channel until the stream dies (false) or
// the account context is cancelled (true). Bursts of StateChange events are
// coalesced into one reconcile pass per type set — push storms cost one
// round-trip, not one per event (FR-K4).
func (e *Engine) pumpPush(ctx context.Context, ch <-chan mail.Change, stop func() error) (ctxDone bool) {
	defer func() { _ = stop() }()
	e.streamUp()
	for {
		var change mail.Change
		select {
		case <-ctx.Done():
			return true
		case c, ok := <-ch:
			if !ok {
				return false
			}
			change = c
		}
		types := collectTypes(change)
	drain:
		for {
			select {
			case c, ok := <-ch:
				if !ok {
					e.reconcileTypes(ctx, types)
					return false
				}
				for t := range collectTypes(c) {
					types[t] = true
				}
			default:
				break drain
			}
		}
		e.reconcileTypes(ctx, types)
	}
}

// collectTypes flattens a StateChange into the set of type names to
// reconcile.
func collectTypes(c mail.Change) map[string]bool {
	out := map[string]bool{}
	for _, byType := range c.Changed {
		for typ := range byType {
			out[typ] = true
		}
	}
	return out
}

// pollOnce is the fallback tick (FR-B3): the same reconciliation path the
// push handler uses, driven purely by the stored state strings (FR-B5).
func (e *Engine) pollOnce(ctx context.Context) {
	types := map[string]bool{"Email": true, "Mailbox": true}
	// Contacts poll only while the store is warm — an account whose user
	// never opened contacts costs nothing (FR-L1).
	if e.contactsWarm() {
		types["ContactCard"] = true
		types["AddressBook"] = true
	}
	e.reconcileTypes(ctx, types)
}

// reconcileTypes runs /changes for every changed type and marks the sync
// healthy when the pass completes.
func (e *Engine) reconcileTypes(ctx context.Context, types map[string]bool) {
	if types["Mailbox"] {
		e.reconcileMailbox(ctx)
	}
	if types["Email"] {
		e.reconcileEmail(ctx)
	}
	// Contact types arrive on the same stream (types=*); both reconcilers
	// no-op while the contact store is cold.
	if types["ContactCard"] {
		e.reconcileContacts(ctx)
	}
	if types["AddressBook"] {
		e.reconcileAddressBooks(ctx)
	}
	e.markSynced()
}

// reconcileEmail folds Email/changes deltas into the store (FR-B2, FR-B5):
// destroyed ids are evicted with cursor repair, updated ids are refetched
// and patched, and fresh mail in the active mailbox slides in at the top of
// date-descending views (PLAN §4.1 case 3). No state string is invented:
// cannotCalculateChanges forces a cursor-anchored re-query.
func (e *Engine) reconcileEmail(ctx context.Context) {
	e.mu.Lock()
	since, hasWindow := e.emailState, e.window != nil
	e.mu.Unlock()
	if since == "" || !hasWindow {
		return // nothing bootstrapped yet; the next event catches up
	}

	var all mail.EmailChangeSet
	state := since
	for i := 0; i < maxChangeLoops; i++ {
		set, err := e.p.EmailChanges(ctx, state)
		if err != nil {
			if errors.Is(err, mail.ErrCannotCalculateChanges) {
				e.fullResync(ctx)
				return
			}
			e.setLastError(err)
			return
		}
		all.Updated = append(all.Updated, set.Updated...)
		all.Destroyed = append(all.Destroyed, set.Destroyed...)
		all.NewState = set.NewState
		if !set.HasMore {
			break
		}
		state = set.NewState
	}

	// Partition updated ids under the current window: ids already visible
	// (or in a cached thread) are refetched; the rest are probed only when
	// nothing else changed, to detect new mail for the slide-in.
	e.mu.Lock()
	w := e.window
	relevant, candidates := partitionUpdated(w, e.threads, all.Updated)
	e.mu.Unlock()

	var sums []mail.EmailSummary
	if len(relevant) > 0 {
		s, err := e.p.FetchSummaries(ctx, relevant)
		if err != nil {
			e.setLastError(err)
			return
		}
		sums = s
	} else if len(candidates) > 0 && len(candidates) <= probeCap {
		s, err := e.p.FetchSummaries(ctx, candidates)
		if err != nil {
			e.setLastError(err)
			return
		}
		sums = s
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.window != w {
		return // mailbox switched mid-reconcile; the fresh window is honest
	}

	// Destroys first: rows vanish, the cursor lands on its neighbour
	// (FR-D5), and any overlay for a destroyed id is moot.
	if len(all.Destroyed) > 0 {
		e.window.RemoveIDs(all.Destroyed)
		unnamed := false
		for _, id := range all.Destroyed {
			// Read the thread id before the summary goes: a vanished
			// member retires its thread's expandable-row count (FR-D1).
			threadID := mail.ID("")
			if s, ok := e.summaries[id]; ok {
				threadID = s.ThreadID
			} else {
				unnamed = true
			}
			delete(e.summaries, id)
			delete(e.fresh, id)
			e.dropOverlayLocked(id)
			e.dropThreadMemberLocked(id, threadID)
		}
		if unnamed {
			// A message we never fetched could belong to any thread on
			// screen, and a chevron promising a reply that is gone is
			// worse than no chevron: retire every count and let one
			// batched refresh re-learn them (FR-D1).
			clear(e.threadSizes)
		}
	}

	for _, s := range sums {
		e.absorbUpdatedSummaryLocked(w, s)
	}

	if all.NewState != "" {
		e.emailState = all.NewState
	}
	e.publishLocked()
}

// absorbUpdatedSummaryLocked patches one fetched summary into the store and
// routes new mail to the slide-in. Caller holds mu; w is the window the
// reconcile was computed against.
func (e *Engine) absorbUpdatedSummaryLocked(w *Window, s mail.EmailSummary) {
	known := false
	if _, ok := e.summaries[s.ID]; ok {
		known = true
	}
	for _, members := range e.threads {
		for _, mid := range members {
			if mid == s.ID {
				known = true
			}
		}
	}
	if known {
		// Patch in place (flags changed, subject edit, …). Overlay re-applies
		// on top so an unconfirmed local op survives server noise (FR-B7).
		e.summaries[s.ID] = e.reapplyOverlayLocked(s)
		if _, cached := e.threads[s.ThreadID]; !cached {
			return
		}
		e.insertThreadMemberLocked(s)
		return
	}

	// Unknown id: candidate new mail. Search views never slide in —
	// whether the message matches the open search is only knowable
	// server-side, so unknown ids wait for the next re-anchor (PLAN §4.1
	// case 3 is a mailbox-browsing behaviour).
	//
	// Whatever happens below, a new message is a new member of its
	// thread, so the count behind that thread's chevron is now stale —
	// drop it and let the next size refresh learn the truth (FR-D1). A
	// thread outside the view has no count to drop.
	delete(e.threadSizes, s.ThreadID)
	if w.query.Filter.Search != nil {
		return
	}
	// It belongs in the window when it is in the open mailbox and newer
	// than the current head (date-desc views only — the only sort M2
	// opens; others get the hint path).
	if !inMailbox(s, w.query.Filter.MailboxID) {
		return
	}
	if !dateDescending(w.query.Sort) {
		w.MarkNewAbove()
		return
	}
	if e.windowShouldSlide(w, s) {
		// One row per thread: a collapsed window represents a thread by a
		// single id, so a reply arriving into a thread already in the
		// window supersedes that row rather than adding a second chevron
		// beside it — the server would return only the newest member for
		// the thread (RFC 8621 §4.4.3). The row follows its conversation
		// to the top, where the date-desc sort now puts it.
		if old := e.windowThreadRow(w, s.ThreadID); old != "" && w.start == 0 {
			if _, cached := e.threads[s.ThreadID]; cached {
				if oldSum, ok := e.summaries[old]; ok {
					e.insertThreadMemberLocked(oldSum)
				}
			}
			// The cursor rides the conversation to its new home (FR-D5).
			follows := w.CursorID() == old
			w.RemoveIDs([]mail.ID{old})
			if w.InsertTop(s.ID) {
				e.fresh[s.ID] = time.Now()
				if follows {
					w.SeekID(s.ID)
				}
			}
		} else if w.InsertTop(s.ID) {
			e.fresh[s.ID] = time.Now()
		}
	} else {
		w.MarkNewAbove()
	}
	// Store the summary so the row renders once it is in the window. A
	// cached thread gains the new member too, so an expanded block shows
	// the reply in place instead of hiding it.
	e.summaries[s.ID] = e.reapplyOverlayLocked(s)
	if _, cached := e.threads[s.ThreadID]; cached {
		e.insertThreadMemberLocked(s)
	}
}

// windowThreadRow returns the window id already standing for thread t, if
// any — the row a fresh arrival must supersede. Caller holds mu.
func (e *Engine) windowThreadRow(w *Window, t mail.ID) mail.ID {
	if t == "" {
		return ""
	}
	for _, id := range w.IDs() {
		if s, ok := e.summaries[id]; ok && s.ThreadID == t {
			return id
		}
	}
	return ""
}

// windowShouldSlide reports whether s is newer than the window head, or the
// window is empty (any new mail slides in).
func (e *Engine) windowShouldSlide(w *Window, s mail.EmailSummary) bool {
	ids := w.IDs()
	if len(ids) == 0 {
		return true
	}
	head, ok := e.summaries[ids[0]]
	if !ok {
		return true
	}
	return s.ReceivedAt.After(head.ReceivedAt)
}

// inMailbox reports whether the summary lives in the given mailbox.
func inMailbox(s mail.EmailSummary, id mail.ID) bool {
	for _, mid := range s.MailboxIDs {
		if mid == id {
			return true
		}
	}
	return false
}

// dateDescending reports whether the sort is the default (or explicit)
// newest-first receivedAt order — the "inbox-like" view of PLAN §4.1.
func dateDescending(sort []mail.SortCriterion) bool {
	if len(sort) == 0 {
		return true // provider default: receivedAt descending
	}
	first := sort[0]
	return first.Property == "receivedAt" && first.IsDescending
}

// partitionUpdated splits updated ids into those the visible state needs
// refetched (window, cached threads, or overlaid) and the unknown remainder
// (slide-in candidates). Caller holds mu.
func partitionUpdated(w *Window, threads map[mail.ID][]mail.ID, updated []mail.ID) (relevant, candidates []mail.ID) {
	inWindow := map[mail.ID]bool{}
	if w != nil {
		for _, id := range w.IDs() {
			inWindow[id] = true
		}
	}
	cached := map[mail.ID]bool{}
	for _, members := range threads {
		for _, mid := range members {
			cached[mid] = true
		}
	}
	for _, id := range updated {
		switch {
		case inWindow[id] || cached[id]:
			relevant = append(relevant, id)
		default:
			candidates = append(candidates, id)
		}
	}
	return relevant, candidates
}

// insertThreadMemberLocked adds s to its cached thread in oldest-first
// order. Caller holds mu.
func (e *Engine) insertThreadMemberLocked(s mail.EmailSummary) {
	members := e.threads[s.ThreadID]
	for _, mid := range members {
		if mid == s.ID {
			return
		}
	}
	pos := len(members)
	for i, mid := range members {
		if ms, ok := e.summaries[mid]; ok && ms.ReceivedAt.After(s.ReceivedAt) {
			pos = i
			break
		}
	}
	members = append(members, "")
	copy(members[pos+1:], members[pos:])
	members[pos] = s.ID
	e.threads[s.ThreadID] = members
	e.threadSizes[s.ThreadID] = len(members)
}

// dropThreadMemberLocked forgets an id in its cached thread and retires
// the thread's member count: a destroy can take a thread from two members
// to one, and a chevron promising a reply under a message that no longer
// has one is a lie (FR-D1). threadID comes from the summary the caller is
// about to delete — an id with no summary was never a cached member (the
// cache keeps its members' summaries alive), so there is nothing stale to
// retire. Caller holds mu.
func (e *Engine) dropThreadMemberLocked(id, threadID mail.ID) {
	if threadID == "" {
		return
	}
	if _, cached := e.threads[threadID]; !cached {
		// Collapsed thread: only the count is known to be wrong, and the
		// next size refresh re-learns it.
		delete(e.threadSizes, threadID)
		return
	}
	members := e.threads[threadID][:0]
	for _, mid := range e.threads[threadID] {
		if mid != id {
			members = append(members, mid)
		}
	}
	if len(members) == 0 {
		delete(e.threads, threadID)
		delete(e.threadSizes, threadID)
		return
	}
	e.threads[threadID] = members
	e.threadSizes[threadID] = len(members)
}

// reconcileMailbox folds Mailbox/changes into the tree (FR-B6). Count and
// structure changes arrive together, so any delta refetches the full tree —
// one batched round-trip for a human-scale list, always structurally honest.
func (e *Engine) reconcileMailbox(ctx context.Context) {
	e.mu.Lock()
	since := e.mailboxState
	e.mu.Unlock()
	if since == "" {
		return
	}

	var changed bool
	state := since
	for i := 0; i < maxChangeLoops; i++ {
		set, err := e.p.MailboxChanges(ctx, state)
		if err != nil {
			if errors.Is(err, mail.ErrCannotCalculateChanges) {
				changed = true
				break
			}
			e.setLastError(err)
			return
		}
		if len(set.Updated) > 0 || len(set.Destroyed) > 0 {
			changed = true
		}
		state = set.NewState
		if !set.HasMore {
			break
		}
	}
	if !changed {
		e.mu.Lock()
		e.mailboxState = state
		e.mu.Unlock()
		return
	}

	list, err := e.p.Mailboxes(ctx)
	if err != nil {
		e.setLastError(err)
		return
	}

	e.mu.Lock()
	// The open mailbox may have been destroyed server-side: fall back to
	// the inbox after publishing so the list never shows a ghost mailbox
	// (FR-C1 roles). The reopen runs outside the lock (it takes it too).
	active := e.activeMailboxLocked()
	found := active == ""
	for _, mb := range e.mailboxes {
		if mb.ID == active {
			found = true
			break
		}
	}
	e.mailboxes = list.Mailboxes
	if list.State != "" {
		e.mailboxState = list.State
	}
	e.rebuildMailboxTreeLocked()
	e.publishLocked()
	e.mu.Unlock()

	if !found {
		for _, mb := range e.mailboxes {
			if mb.Role == mail.RoleInbox {
				_ = e.OpenMailbox(ctx, mb.ID)
				break
			}
		}
	}
}

// fullResync answers cannotCalculateChanges (and triage membership
// changes) with a cursor-anchored re-query (PLAN §4.1 case 2): the cursor id
// is the only honest position, so the fresh chunk starts there and position
// survives by id (FR-B5, FR-D5). An empty window re-seeds from the top —
// there is no cursor to anchor.
func (e *Engine) fullResync(ctx context.Context) {
	e.mu.Lock()
	w := e.window
	if w == nil {
		e.mu.Unlock()
		return
	}
	var r Request
	var spec mail.QuerySpec
	if id := w.CursorID(); id != "" {
		r = w.AnchorNeed(id)
		spec = e.querySpecLocked(0, e.cfg.Window.withDefaults().Chunk)
		spec.AnchorID = id
		spec.AnchorOffset = 0
	} else {
		var pos, limit int
		r, pos, limit = w.Seed(0)
		spec = e.querySpecLocked(pos, limit)
	}
	e.mu.Unlock()

	handle, sums, err := e.p.OpenQuery(ctx, spec)
	if err != nil {
		e.setLastError(err)
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.window != w {
		return
	}
	if err := w.Complete(r, handle.Start(), handle.IDs(), handle.Total(), handle.State()); err != nil {
		return
	}
	if st := handle.EmailState(); st != "" {
		e.emailState = st
	}
	e.absorbSummariesLocked(sums)
	e.alignCursorWithWindowLocked()
	e.publishLocked()
}

// streamUp marks a (re)established stream healthy: the previous drop's
// error is stale the moment a new stream is pumped — without this the
// footer shows "push stream lost" forever on an idle mailbox that never
// triggers a reconcile.
func (e *Engine) streamUp() {
	e.mu.Lock()
	e.status.LastError = ""
	e.status.Attempts = 0
	e.publishLocked()
	e.mu.Unlock()
}

// setMode records a sync-mode transition when it actually changes.
func (e *Engine) setMode(mode Mode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status.Mode == mode {
		return
	}
	e.status.Mode = mode
	e.publishLocked()
}

// setLastError records a sync failure for the status line (FR-I5) and
// repaints immediately.
func (e *Engine) setLastError(err error) {
	e.mu.Lock()
	e.status.LastError = truncateStatusErr(err)
	e.publishLocked()
	e.mu.Unlock()
}

// markSynced clears the error state, stamps the last successful pass, and
// repaints so "synced <time>" tracks real reconciles.
func (e *Engine) markSynced() {
	e.mu.Lock()
	e.status.LastSync = time.Now()
	e.status.LastError = ""
	e.status.Attempts = 0
	e.publishLocked()
	e.mu.Unlock()
}

// truncateStatusErr keeps status-line errors to one bounded line.
func truncateStatusErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	return s
}

// ClearFresh drops the new-mail highlight (app timer / cursor movement).
func (e *Engine) ClearFresh() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.fresh) == 0 {
		return e.snapshotLocked()
	}
	e.fresh = map[mail.ID]time.Time{}
	e.publishLocked()
	return e.snapshotLocked()
}

// sleepCtx waits d or until ctx ends; false means the context won.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
