package sync

import (
	"context"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// TriageKind enumerates the triage actions (FR-G1, FR-G2). Delete-to-trash
// and archive arrive as TriageMove with the role mailbox resolved by the
// app; TriageDestroy is permanent delete inside Trash only.
type TriageKind int

// Triage actions.
const (
	TriageRead          TriageKind = iota // add $seen
	TriageUnread                          // remove $seen
	TriageStar                            // add $flagged
	TriageUnstar                          // remove $flagged
	TriageMove                            // relocate: drop every other membership, hold Mailbox
	TriageCopy                            // add Mailbox membership, keep the rest
	TriageRemoveMailbox                   // drop the Mailbox membership (undo primitive for copies)
	TriageDestroy                         // permanently delete (irreversible)
)

// TriageSpec is one triage action over a set of ids (FR-G3): applied
// optimistically as overlays, then confirmed by a single batched /set
// (FR-K4). Restore carries the exact reverse deltas for a move-undo;
// it is nil on first-class actions.
type TriageSpec struct {
	Kind    TriageKind
	IDs     []mail.ID
	Mailbox mail.ID // destination for Move/Copy/RemoveMailbox

	// Restore maps id → memberships to re-add in a move-undo (undo of a
	// move must restore exactly what the move removed, not guess).
	Restore map[mail.ID][]mail.ID
}

// Receipt reports one Triage outcome and how to undo it (FR-G5). Undo is
// nil for irreversible actions (destroy); the app cancels a *delayed*
// destroy with CancelDestroy instead.
type Receipt struct {
	Applied []mail.ID
	Failed  []mail.ID
	Err     string // aggregate error text for the toast/footer

	Undo *TriageSpec
	Snap Snapshot
}

// triagePlan is the resolved work for one TriageSpec: the batched mutation,
// the optimistic overlays, which rows leave the open view, and the undo.
type triagePlan struct {
	mutation          mail.Mutation
	overlays          map[mail.ID]Overlay
	hidesRow          map[mail.ID]bool
	touched           []mail.ID
	undo              *TriageSpec
	membershipChanged bool // open-mailbox membership changed ⇒ re-anchor after
}

// keywordForKind maps a keyword triage kind to (keyword, add).
func keywordForKind(k TriageKind) (string, bool) {
	switch k {
	case TriageRead:
		return "$seen", true
	case TriageUnread:
		return "$seen", false
	case TriageStar:
		return "$flagged", true
	case TriageUnstar:
		return "$flagged", false
	}
	return "", false
}

// planTriageLocked resolves spec against the current (overlay-aware)
// summaries. Caller holds mu. Ids already in the wanted state are no-ops;
// the undo spec covers only ids this action actually mutates.
func (e *Engine) planTriageLocked(spec TriageSpec) triagePlan {
	plan := triagePlan{
		overlays: map[mail.ID]Overlay{},
		hidesRow: map[mail.ID]bool{},
		mutation: mail.Mutation{Emails: map[mail.ID]mail.EmailPatch{}},
	}
	active := e.activeMailboxLocked()
	seenID := map[mail.ID]bool{}

	var kwUndoIDs, moveUndoIDs, copyUndoIDs []mail.ID
	var moveRestore map[mail.ID][]mail.ID

	for _, id := range spec.IDs {
		if seenID[id] {
			continue
		}
		seenID[id] = true
		s, ok := e.summaries[id]
		if !ok {
			continue // unknown id: nothing to act on
		}
		plan.touched = append(plan.touched, id)

		var ov Overlay
		var patch mail.EmailPatch
		hides, changed := false, false

		switch spec.Kind {
		case TriageRead, TriageUnread, TriageStar, TriageUnstar:
			kw, add := keywordForKind(spec.Kind)
			if s.Keywords.Has(kw) == add {
				continue // already in the wanted state
			}
			patch.SetKeywords = map[string]bool{kw: add}
			if add {
				ov.KeywordsAdd = []string{kw}
			} else {
				ov.KeywordsRemove = []string{kw}
			}
			kwUndoIDs = append(kwUndoIDs, id)

		case TriageCopy:
			if inMailbox(s, spec.Mailbox) {
				continue
			}
			patch.AddMailboxes = []mail.ID{spec.Mailbox}
			ov.MailboxAdd = []mail.ID{spec.Mailbox}
			changed = spec.Mailbox == active
			copyUndoIDs = append(copyUndoIDs, id)

		case TriageRemoveMailbox:
			if !inMailbox(s, spec.Mailbox) {
				continue
			}
			patch.RemoveMailboxes = []mail.ID{spec.Mailbox}
			ov.MailboxRemove = []mail.ID{spec.Mailbox}
			if spec.Mailbox == active {
				hides, changed = true, true
			}

		case TriageMove:
			dest := spec.Mailbox
			if spec.Restore != nil {
				// Undo of a move: exact reverse deltas — drop the
				// destination membership, re-add what the move removed.
				var adds []mail.ID
				for _, mb := range spec.Restore[id] {
					if mb != dest && !inMailbox(s, mb) {
						adds = append(adds, mb)
					}
				}
				if inMailbox(s, dest) {
					patch.RemoveMailboxes = []mail.ID{dest}
					ov.MailboxRemove = []mail.ID{dest}
				}
				if len(adds) > 0 {
					patch.AddMailboxes = adds
					ov.MailboxAdd = adds
				}
			} else {
				var removes []mail.ID
				for _, mb := range s.MailboxIDs {
					if mb != dest {
						removes = append(removes, mb)
					}
				}
				if len(removes) == 0 && inMailbox(s, dest) {
					continue // already exactly there
				}
				patch.RemoveMailboxes = removes
				ov.MailboxRemove = removes
				if !inMailbox(s, dest) {
					patch.AddMailboxes = []mail.ID{dest}
					ov.MailboxAdd = []mail.ID{dest}
				}
				if len(removes) > 0 || ov.MailboxAdd != nil {
					moveUndoIDs = append(moveUndoIDs, id)
					if moveRestore == nil {
						moveRestore = map[mail.ID][]mail.ID{}
					}
					moveRestore[id] = removes
				}
			}
			// Open-view consequences, from the post-patch membership:
			// the row leaves when the patch drops the active mailbox,
			// and the window needs a re-anchor when the patch adds it.
			if active != "" {
				after := applyTo(s, ov)
				if inMailbox(s, active) && !containsID(after, active) {
					hides, changed = true, true
				}
				if !inMailbox(s, active) && containsID(after, active) {
					changed = true
				}
			}

		case TriageDestroy:
			plan.mutation.Destroy = append(plan.mutation.Destroy, id)
			ov.Destroy = true
			hides = true
			changed = active != "" && inMailbox(s, active)
		}

		if patch.SetKeywords != nil || len(patch.AddMailboxes) > 0 || len(patch.RemoveMailboxes) > 0 {
			plan.mutation.Emails[id] = patch
		}
		if !ov.empty() {
			plan.overlays[id] = ov
		}
		if hides {
			plan.hidesRow[id] = true
		}
		if changed {
			plan.membershipChanged = true
		}
	}

	switch {
	case len(kwUndoIDs) > 0:
		inv := TriageRead
		switch spec.Kind {
		case TriageRead:
			inv = TriageUnread
		case TriageUnread:
			inv = TriageRead
		case TriageStar:
			inv = TriageUnstar
		case TriageUnstar:
			inv = TriageStar
		}
		plan.undo = &TriageSpec{Kind: inv, IDs: kwUndoIDs}
	case len(moveUndoIDs) > 0:
		plan.undo = &TriageSpec{Kind: TriageMove, IDs: moveUndoIDs, Mailbox: spec.Mailbox, Restore: moveRestore}
	case len(copyUndoIDs) > 0:
		plan.undo = &TriageSpec{Kind: TriageRemoveMailbox, IDs: copyUndoIDs, Mailbox: spec.Mailbox}
	}
	return plan
}

// previewMembership returns s's memberships after removing ov's removals
// (used for the hides-row decision before building the patch).
func previewMembership(s mail.EmailSummary, ov Overlay) []mail.ID {
	out := make([]mail.ID, 0, len(s.MailboxIDs))
	for _, mb := range s.MailboxIDs {
		keep := true
		for _, rm := range ov.MailboxRemove {
			if mb == rm {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, mb)
		}
	}
	return out
}

// applyTo returns s's memberships after the overlay deltas (adds included).
func applyTo(s mail.EmailSummary, ov Overlay) []mail.ID {
	out := previewMembership(s, ov)
	for _, mb := range ov.MailboxAdd {
		if !containsID(out, mb) {
			out = append(out, mb)
		}
	}
	return out
}

func containsID(ids []mail.ID, want mail.ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// Triage applies one triage action (FR-G1..G5): optimistic overlays first
// (FR-B7), then a single batched /set (FR-G3, FR-K4), then confirmation —
// refetched server truth replaces the optimistic state and a cursor-anchored
// re-query repairs the window when open-mailbox membership changed.
func (e *Engine) Triage(ctx context.Context, spec TriageSpec) (Receipt, error) {
	e.ensureSummaries(ctx, spec.IDs)

	e.mu.Lock()
	plan := e.planTriageLocked(spec)
	for id, ov := range plan.overlays {
		e.rememberOverlayLocked(id, pendingOp{ov: ov, appliedAt: time.Now()})
		if s, ok := e.summaries[id]; ok {
			applied := ov.apply(s)
			e.summaries[id] = applied
			if plan.hidesRow[id] && e.window != nil {
				e.window.RemoveIDs([]mail.ID{id})
			}
		}
	}
	noop := len(plan.mutation.Emails) == 0 && len(plan.mutation.Destroy) == 0
	e.publishLocked()
	snap := e.snapshotLocked()
	e.mu.Unlock()

	if noop {
		return Receipt{Snap: snap}, nil
	}

	res, err := e.p.Mutate(ctx, plan.mutation)
	if err != nil {
		// Transport-level failure: revert every optimistic patch with
		// server truth (FR-B7 revert path).
		e.revertTriage(ctx, plan.touched)
		return Receipt{Failed: plan.touched, Err: err.Error()}, err
	}

	// Confirm: refetch the affected ids in one batch — updated ids carry
	// server truth, rejected ids revert to it.
	fetch := dedupeIDs(append(append([]mail.ID{}, res.Updated...), plan.touched...))
	var byID map[mail.ID]mail.EmailSummary
	if len(fetch) > 0 {
		sums, ferr := e.p.FetchSummaries(ctx, fetch)
		if ferr != nil {
			e.setLastError(ferr)
		} else {
			byID = map[mail.ID]mail.EmailSummary{}
			for _, s := range sums {
				byID[s.ID] = s
			}
		}
	}

	e.mu.Lock()
	for _, id := range res.Updated {
		if s, ok := byID[id]; ok {
			e.summaries[id] = e.reapplyOverlayLocked(s)
		}
		e.dropOverlayLocked(id)
	}
	for _, id := range res.Destroyed {
		delete(e.summaries, id)
		e.dropOverlayLocked(id)
		e.dropThreadMemberLocked(id)
		delete(e.fresh, id)
	}
	rcpt := Receipt{Undo: plan.undo}
	for id := range res.NotUpdated {
		if s, ok := byID[id]; ok {
			e.summaries[id] = s
		}
		e.dropOverlayLocked(id)
		rcpt.Failed = append(rcpt.Failed, id)
	}
	for id := range res.NotDestroyed {
		if s, ok := byID[id]; ok {
			e.summaries[id] = s
		}
		e.dropOverlayLocked(id)
		rcpt.Failed = append(rcpt.Failed, id)
	}
	if len(rcpt.Failed) > 0 {
		rcpt.Err = "some messages were rejected by the server"
	}
	rcpt.Applied = append(rcpt.Applied, res.Updated...)
	rcpt.Applied = append(rcpt.Applied, res.Destroyed...)
	// Ids we optimistically touched that the server never mentioned: drop
	// their overlays and show fetched truth (or nothing).
	mentioned := map[mail.ID]bool{}
	for _, id := range res.Updated {
		mentioned[id] = true
	}
	for _, id := range res.Destroyed {
		mentioned[id] = true
	}
	for id := range res.NotUpdated {
		mentioned[id] = true
	}
	for id := range res.NotDestroyed {
		mentioned[id] = true
	}
	for _, id := range plan.touched {
		if mentioned[id] {
			continue
		}
		e.dropOverlayLocked(id)
		rcpt.Failed = append(rcpt.Failed, id)
	}
	e.publishLocked()
	rcpt.Snap = e.snapshotLocked()
	e.mu.Unlock()

	if plan.membershipChanged {
		e.fullResync(ctx)
	}
	return rcpt, nil
}

// ensureSummaries refetches ids the store has evicted (a moved-out row's
// summary leaves the keep set, but its undo must still resolve) so the
// plan can build exact deltas. Fetch failures are tolerated: the plan
// simply skips ids it cannot see.
func (e *Engine) ensureSummaries(ctx context.Context, ids []mail.ID) {
	e.mu.Lock()
	var missing []mail.ID
	seen := map[mail.ID]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := e.summaries[id]; !ok {
			missing = append(missing, id)
		}
	}
	e.mu.Unlock()
	if len(missing) == 0 {
		return
	}
	sums, err := e.p.FetchSummaries(ctx, missing)
	if err != nil {
		return
	}
	e.mu.Lock()
	for _, s := range sums {
		e.summaries[s.ID] = e.reapplyOverlayLocked(s)
	}
	e.mu.Unlock()
}

// revertTriage drops optimistic overlays and restores server truth for ids
// after a failed mutation (FR-B7 revert path).
func (e *Engine) revertTriage(ctx context.Context, ids []mail.ID) {
	if len(ids) == 0 {
		return
	}
	e.mu.Lock()
	for _, id := range ids {
		e.dropOverlayLocked(id)
	}
	e.mu.Unlock()

	sums, err := e.p.FetchSummaries(ctx, ids)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.setLastError(err)
		e.publishLocked()
		return
	}
	for _, s := range sums {
		e.summaries[s.ID] = s
	}
	e.publishLocked()
}

// PrepareDestroy optimistically hides ids pending a delayed destroy (FR-G2
// permanent delete inside Trash, FR-G5 undo as cancellation): rows vanish
// now, the server /set fires after the undo window, CancelDestroy aborts.
func (e *Engine) PrepareDestroy(ids []mail.ID) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		if _, ok := e.summaries[id]; !ok {
			continue
		}
		e.applyOverlayLocked(id, Overlay{Destroy: true})
	}
	e.publishLocked()
	return e.snapshotLocked()
}

// CancelDestroy aborts a prepared destroy: overlays drop and a cursor-
// anchored re-query re-materialises the rows at their positions.
func (e *Engine) CancelDestroy(ctx context.Context, ids []mail.ID) Snapshot {
	e.mu.Lock()
	for _, id := range ids {
		e.dropOverlayLocked(id)
	}
	e.publishLocked()
	e.mu.Unlock()
	e.fullResync(ctx)
	return e.Snapshot()
}

func dedupeIDs(ids []mail.ID) []mail.ID {
	seen := map[mail.ID]bool{}
	out := ids[:0]
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
