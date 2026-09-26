package jmapclient

import (
	"context"
	"errors"
	"fmt"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/thread"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// Threads implements mail.Provider via Thread/get (RFC 8621 §3.1): one
// batched call returns every requested thread's member ids, sorted
// oldest-first — the order a thread expands in (FR-D2) and the counts the
// list needs to mark expandable rows (FR-D1).
//
// It is deliberately not batched with the member Email/get: a "#ids"
// back-reference whose Thread/get list is empty resolves to null, and
// RFC 8620 §5.1 reads a null ids as "fetch every object" — a trap not
// worth one round-trip.
func (c *Client) Threads(ctx context.Context, threadIDs []mail.ID) (map[mail.ID][]mail.ID, error) {
	out := make(map[mail.ID][]mail.ID, len(threadIDs))
	if len(threadIDs) == 0 {
		return out, nil
	}
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}
	ids := make([]jmap.ID, 0, len(threadIDs))
	for _, t := range threadIDs {
		if t == "" {
			continue
		}
		ids = append(ids, jmap.ID(t))
	}
	if len(ids) == 0 {
		return out, nil
	}
	inv, err := c.do(ctx, &thread.Get{Account: jmap.ID(c.accountID), IDs: ids})
	if err != nil {
		return nil, fmt.Errorf("jmapclient: Thread/get: %w", err)
	}
	tr, ok := inv.Args.(*thread.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Thread/get: unexpected response type %T", inv.Args)
	}
	for _, th := range tr.List {
		if th == nil {
			continue
		}
		members := make([]mail.ID, 0, len(th.EmailIDs))
		for _, id := range th.EmailIDs {
			members = append(members, mail.ID(id))
		}
		out[mail.ID(th.ID)] = members
	}
	return out, nil
}
