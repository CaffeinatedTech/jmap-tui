package sync

import (
	"context"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// Overlay is a pending local mutation for one message, applied to the
// in-memory summary before the server confirms it (FR-B7 plumbing). M2
// ships the reconciliation machinery; M3 triage actions drive it. Fields
// are deltas, not absolute state, so re-applying after a server update
// composes cleanly.
type Overlay struct {
	KeywordsAdd    []string
	KeywordsRemove []string
	MailboxAdd     []mail.ID
	MailboxRemove  []mail.ID
	Destroy        bool // locally deleted; the row hides until confirm
}

// empty reports whether the overlay changes nothing (all-delta overlay).
func (o Overlay) empty() bool {
	return len(o.KeywordsAdd) == 0 && len(o.KeywordsRemove) == 0 &&
		len(o.MailboxAdd) == 0 && len(o.MailboxRemove) == 0 && !o.Destroy
}

// pendingOp tracks an applied overlay so server updates can re-apply it and
// confirm/fail paths can clear or revert it (FR-B7).
type pendingOp struct {
	ov        Overlay
	appliedAt time.Time
}

// maxOverlay bounds pending overlays (NFR-2); the oldest is dropped when
// exceeded, same policy as the thread cache.
const maxOverlay = 512

// ApplyOverlay records a pending mutation and reflects it in the in-memory
// summary immediately (optimistic write, FR-B7 plumbing). Destroy hides the
// row via the window's live-destroy path, as does any overlay that removes
// the open mailbox's membership (moves, copy-undo).
func (e *Engine) ApplyOverlay(id mail.ID, ov Overlay) Snapshot {
	if id == "" || ov.empty() {
		return e.Snapshot()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyOverlayLocked(id, ov)
	e.publishLocked()
	return e.snapshotLocked()
}

// applyOverlayLocked records and applies one overlay. Caller holds mu.
func (e *Engine) applyOverlayLocked(id mail.ID, ov Overlay) {
	e.rememberOverlayLocked(id, pendingOp{ov: ov, appliedAt: time.Now()})
	if s, ok := e.summaries[id]; ok {
		applied := ov.apply(s)
		e.summaries[id] = applied
		if e.window != nil {
			active := e.window.query.Filter.MailboxID
			switch {
			case ov.Destroy:
				e.window.RemoveIDs([]mail.ID{id})
			case active != "" && !inMailbox(applied, active) && inMailbox(s, active):
				// The optimistic patch removed the open mailbox's
				// membership: the row leaves the view now (FR-B7).
				e.window.RemoveIDs([]mail.ID{id})
			}
		}
	}
}

// apply patches a summary copy with the overlay deltas.
func (o Overlay) apply(s mail.EmailSummary) mail.EmailSummary {
	out := s
	kw := make(mail.Keywords, len(s.Keywords)+len(o.KeywordsAdd))
	for k := range s.Keywords {
		kw[k] = struct{}{}
	}
	for _, k := range o.KeywordsAdd {
		kw[k] = struct{}{}
	}
	for _, k := range o.KeywordsRemove {
		delete(kw, k)
	}
	out.Keywords = kw

	mbs := make(map[mail.ID]bool, len(s.MailboxIDs)+len(o.MailboxAdd))
	for _, id := range s.MailboxIDs {
		mbs[id] = true
	}
	for _, id := range o.MailboxAdd {
		mbs[id] = true
	}
	for _, id := range o.MailboxRemove {
		delete(mbs, id)
	}
	out.MailboxIDs = out.MailboxIDs[:0]
	for id := range mbs {
		out.MailboxIDs = append(out.MailboxIDs, id)
	}
	return out
}

// OverlayConfirmed drops the overlay: the server state now matches what the
// overlay predicted, so the stored (server-fresh) summary stands.
func (e *Engine) OverlayConfirmed(id mail.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dropOverlayLocked(id)
}

// RevertOverlay drops a failed overlay and refetches the server summary so
// the optimistic patch is undone with server truth (FR-B7 revert path).
func (e *Engine) RevertOverlay(ctx context.Context, id mail.ID) error {
	e.mu.Lock()
	e.dropOverlayLocked(id)
	e.mu.Unlock()

	sums, err := e.p.FetchSummaries(ctx, []mail.ID{id})
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range sums {
		e.summaries[s.ID] = s
	}
	e.publishLocked()
	return nil
}

// rememberOverlayLocked stores a pending op with LRU-style bounding.
func (e *Engine) rememberOverlayLocked(id mail.ID, op pendingOp) {
	if _, exists := e.overlay[id]; !exists {
		e.overlayOrder = append(e.overlayOrder, id)
	}
	e.overlay[id] = &op
	for len(e.overlayOrder) > maxOverlay {
		oldest := e.overlayOrder[0]
		e.overlayOrder = e.overlayOrder[1:]
		delete(e.overlay, oldest)
	}
}

// dropOverlayLocked removes a pending op. Caller holds mu.
func (e *Engine) dropOverlayLocked(id mail.ID) {
	if _, ok := e.overlay[id]; !ok {
		return
	}
	delete(e.overlay, id)
	for i, cand := range e.overlayOrder {
		if cand == id {
			e.overlayOrder = append(e.overlayOrder[:i], e.overlayOrder[i+1:]...)
			break
		}
	}
}

// reapplyOverlayLocked folds any pending overlay back onto a freshly
// fetched summary: the server hasn't confirmed the local op yet, so the
// optimistic view must survive server noise arriving mid-flight (FR-B7).
// Caller holds mu.
func (e *Engine) reapplyOverlayLocked(s mail.EmailSummary) mail.EmailSummary {
	op, ok := e.overlay[s.ID]
	if !ok {
		return s
	}
	return op.ov.apply(s)
}
