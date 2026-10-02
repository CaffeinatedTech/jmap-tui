package sync

import (
	"context"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// MarkMailboxRead marks every unread message in a mailbox as read
// (FR-C7). The provider sweeps Email/query + Email/set in chunks; the
// engine then refreshes server truth rather than guessing which ids
// changed: the mailbox tree is refetched for unread badges, and cached
// summaries in that mailbox get $seen so the open window repaints without
// a cursor jump or a re-query. A later Email/changes push reconciles
// against the server. Returns how many messages were marked.
func (e *Engine) MarkMailboxRead(ctx context.Context, id mail.ID) (int, error) {
	n, markErr := e.p.MarkMailboxRead(ctx, id)

	// On a clean sweep every unread message in the mailbox is read, so
	// flipping $seen on cached summaries that belong to it is server
	// truth, not a guess — and it leaves the window and cursor untouched.
	// A partial failure is left to the server's reconciliation instead of
	// painting rejected messages read.
	if markErr == nil {
		e.mu.Lock()
		for sid, s := range e.summaries {
			if !inMailbox(s, id) || s.Keywords.Has("$seen") {
				continue
			}
			s.Keywords = keywordsWith(s.Keywords, "$seen")
			e.summaries[sid] = s
		}
		e.mu.Unlock()
	}

	// Unread badges are server truth: refetch the tree rather than
	// derive the delta. LoadMailboxes publishes the fresh snapshot.
	if lerr := e.LoadMailboxes(ctx); lerr != nil && markErr == nil {
		markErr = lerr
	}
	if markErr != nil {
		e.setLastError(markErr)
	}
	return n, markErr
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
