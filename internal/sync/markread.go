package sync

import (
	"context"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// MarkMailboxRead marks every unread message in a mailbox as read
// (FR-C7). The provider sweeps Email/query + Email/set in chunks; on a
// clean sweep the engine updates the open window and the sidebar badge
// locally rather than firing another request — a bridge throttling the
// sweep's burst would answer a follow-up Mailbox/get with 429. Server
// truth reconciles through Mailbox/changes (and Email/changes) on the
// next push/poll. Returns how many messages were marked.
func (e *Engine) MarkMailboxRead(ctx context.Context, id mail.ID) (int, error) {
	n, markErr := e.p.MarkMailboxRead(ctx, id)
	if markErr != nil {
		// A partial sweep changed some messages; leave local state to the
		// server's reconciliation instead of painting rejected messages
		// read. The status line carries the error.
		e.setLastError(markErr)
		return n, markErr
	}

	e.mu.Lock()
	// Every unread message in this mailbox is now read, so flipping
	// $seen on its cached summaries is server truth, not a guess — and it
	// leaves the window and cursor untouched. The badge is zero for the
	// same reason; the update is optimistic until Mailbox/changes lands.
	for sid, s := range e.summaries {
		if !inMailbox(s, id) || s.Keywords.Has("$seen") {
			continue
		}
		s.Keywords = keywordsWith(s.Keywords, "$seen")
		e.summaries[sid] = s
	}
	for i := range e.mailboxes {
		if e.mailboxes[i].ID == id {
			e.mailboxes[i].UnreadEmails = 0
			break
		}
	}
	e.rebuildMailboxTreeLocked()
	e.publishLocked()
	e.mu.Unlock()
	return n, nil
}

// keywordsWith returns a copy of kws with name present (maps are shared
// with callers, so in-place mutation would leak).
func keywordsWith(kws mail.Keywords, name string) mail.Keywords {
	out := make(mail.Keywords, len(kws)+1)
	for k := range kws {
		out[k] = struct{}{}
	}
	out[name] = struct{}{}
	return out
}
