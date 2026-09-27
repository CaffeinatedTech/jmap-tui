package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
)

// ReplyContext returns everything the composer needs to build a reply,
// reply-all, or forward for id (FR-H1, FR-H2): addressing (including Cc,
// Bcc and Reply-To, which the lean summary property set omits), the RFC
// 5322 threading fields, and the display body text to quote.
//
// The body LRU already holds it once the preview has shown the message, so
// opening a reply from a message you just read costs no round-trip.
func (e *Engine) ReplyContext(ctx context.Context, id mail.ID) (mail.EmailBody, error) {
	if id == "" {
		return mail.EmailBody{}, errors.New("sync: reply context: no message selected")
	}
	if _, body, ok := e.bodies.get(id); ok && len(body.MessageID) > 0 {
		return body, nil
	}
	body, err := e.p.FetchBody(ctx, id)
	if err != nil {
		return mail.EmailBody{}, fmt.Errorf("sync: reply context: %w", err)
	}
	text := body.Text
	if text == "" && body.HTML != "" {
		text = mailtext.HTMLToText(body.HTML)
	}
	body.Text = text
	e.mu.Lock()
	e.bodies.put(id, text, body)
	e.mu.Unlock()
	return body, nil
}

// resolveDraftMailboxes fills the draft's mailbox roles from the loaded
// tree (FR-G4-style role resolution, kept in the engine because that is
// where the tree lives). Errors when the account has no Drafts mailbox —
// there would be nowhere for autosave to persist (FR-H4).
func (e *Engine) resolveDraftMailboxes(d *mail.Draft) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, mb := range e.mailboxes {
		switch mb.Role {
		case mail.RoleDrafts:
			if d.MailboxID == "" {
				d.MailboxID = mb.ID
			}
		case mail.RoleSent:
			if d.SentMailboxID == "" {
				d.SentMailboxID = mb.ID
			}
		}
	}
	if d.MailboxID == "" {
		return errors.New("sync: this account has no Drafts mailbox; drafts cannot be saved")
	}
	return nil
}

// SaveDraft persists the composer's draft (FR-H4) and returns its server
// id. The id changes on every save — Email content is immutable (RFC 8621
// §4.1.2), so the provider edits by recreating — which is why callers must
// always adopt the returned id rather than the one they passed in.
//
// The store is only touched when the Drafts mailbox is the open view: an
// open Drafts list shows the new row immediately, and everywhere else the
// server's own push reconciles it within the usual ~1s (FR-B2).
func (e *Engine) SaveDraft(ctx context.Context, d mail.Draft) (mail.ID, Snapshot, error) {
	if err := e.resolveDraftMailboxes(&d); err != nil {
		return d.ID, e.Snapshot(), err
	}
	oldID := d.ID
	id, err := e.p.SaveDraft(ctx, d)
	if err != nil {
		return oldID, e.Snapshot(), err
	}

	e.mu.Lock()
	if oldID != "" && oldID != id {
		e.forgetEmailLocked(oldID)
	}
	watchingDrafts := e.activeMailboxLocked() == d.MailboxID
	e.publishLocked()
	e.mu.Unlock()

	if watchingDrafts {
		e.insertIntoOpenMailbox(ctx, id, d.MailboxID)
	}
	return id, e.Snapshot(), nil
}

// DraftAttachments re-reads id's attachment metadata from the server.
// Draft blob ids are message-scoped: every recreate stores the bytes
// under fresh ids and the previous ids die with their message, so a
// composer that keeps carrying the ids from an earlier save eventually
// references a dead blob and the save fails with blobNotFound. Callers
// refresh after every successful save (FR-H4).
func (e *Engine) DraftAttachments(ctx context.Context, id mail.ID) ([]mail.Attachment, error) {
	body, err := e.p.FetchBody(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("sync: draft attachments: %w", err)
	}
	return body.Attachments, nil
}

// Send submits the message (FR-H5). The caller has already flushed the
// draft, so d.ID names server-side content that matches the composer.
// Afterwards the draft row leaves the store and — when the Sent mailbox is
// the open view — a cursor-anchored re-query lands the sent copy there
// without a manual refresh (FR-H6).
func (e *Engine) Send(ctx context.Context, d mail.Draft) (mail.SendReceipt, Snapshot, error) {
	if err := e.resolveDraftMailboxes(&d); err != nil {
		return mail.SendReceipt{}, e.Snapshot(), err
	}
	rcpt, err := e.p.Send(ctx, d)
	if err != nil {
		return mail.SendReceipt{}, e.Snapshot(), err
	}

	e.mu.Lock()
	if d.ID != "" {
		e.forgetEmailLocked(d.ID)
	}
	active := e.activeMailboxLocked()
	role := e.mailboxRoleLocked(active)
	e.publishLocked()
	e.mu.Unlock()

	switch role {
	case mail.RoleDrafts, mail.RoleSent:
		// Membership changed under the open view: re-anchor around the
		// cursor so totals and rows are honest (the same repair triage
		// runs after a move).
		e.fullResync(ctx)
	}
	return rcpt, e.Snapshot(), nil
}

// insertIntoOpenMailbox fetches id's summary and splices it into the open
// mailbox view, so an autosave lands as a row without waiting for push.
// A failure is tolerated: the push reconcile is the backstop.
func (e *Engine) insertIntoOpenMailbox(ctx context.Context, id mail.ID, mailbox mail.ID) {
	sums, err := e.p.FetchSummaries(ctx, []mail.ID{id})
	if err != nil || len(sums) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.activeMailboxLocked() != mailbox || e.window == nil {
		return
	}
	contains := false
	for _, existing := range e.window.IDs() {
		if existing == id {
			contains = true
			break
		}
	}
	if !contains && !e.window.InsertTop(id) {
		return
	}
	e.absorbSummariesLocked(sums)
	e.publishLocked()
}

// forgetEmailLocked drops every trace of id from the store: the window
// row, the summary, any thread membership, pending overlay, and the fresh
// highlight. Caller holds mu.
func (e *Engine) forgetEmailLocked(id mail.ID) {
	if e.window != nil {
		e.window.RemoveIDs([]mail.ID{id})
	}
	if e.saved != nil && e.saved.win != nil {
		e.saved.win.RemoveIDs([]mail.ID{id})
	}
	// The summary names the thread whose count a removal retires (FR-D1),
	// so read it before it goes.
	threadID := mail.ID("")
	if s, ok := e.summaries[id]; ok {
		threadID = s.ThreadID
	}
	delete(e.summaries, id)
	delete(e.fresh, id)
	e.dropOverlayLocked(id)
	e.dropThreadMemberLocked(id, threadID)
}

// mailboxRoleLocked resolves a mailbox id to its role; empty when unknown.
func (e *Engine) mailboxRoleLocked(id mail.ID) mail.Role {
	for _, mb := range e.mailboxes {
		if mb.ID == id {
			return mb.Role
		}
	}
	return ""
}
