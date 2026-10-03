package jmapclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/core"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// markReadChunkDefault caps one Email/set in a mark-read sweep. The
// server's advertised maxObjectsInSet lowers it further. Deliberately
// small: a bridge fronting IMAP (jmap-bridge) applies an Email/set by
// looping one backend patch per id, so a page near the protocol max can
// outrun the client's request timeout and abort the sweep. The cap
// trades a few more round-trips for pages that stay well inside the
// timeout on a slow backend.
const markReadChunkDefault = 100

// markReadRetries bounds how many times a transient sweep request is
// re-issued before the sweep gives up. Both the page query and the
// keyword-only $seen set are idempotent, so re-issuing cannot
// double-apply.
const markReadRetries = 2

// markReadChunk is the default chunk size; a var so tests can shrink it
// and exercise multi-chunk paging (the toastTTL precedent).
var markReadChunk = markReadChunkDefault

// MarkMailboxRead implements mail.Provider. JMAP has no wildcard set
// operation (RFC 8621 §4.6 keys Email/set by explicit id), so the sweep
// pages Email/query over the mailbox's unread messages and marks each
// page with one batched Email/set. Because marked messages drop out of
// the notKeyword:$seen filter, the query always reads from position 0 —
// advancing would skip a page every round.
//
// A page the server refuses aborts the sweep: the rejected ids would
// still be unread, so the next query would return them forever. Nothing
// is silently skipped, and the caller refreshes server truth after.
func (c *Client) MarkMailboxRead(ctx context.Context, mailboxID mail.ID) (int, error) {
	if c.session == nil {
		return 0, errors.New("jmapclient: not connected")
	}
	registerEmailSetResponse()

	limit := c.markReadLimit()
	marked := 0
	for {
		q := &email.Query{
			Account: jmap.ID(c.accountID),
			Filter: &email.FilterCondition{
				InMailbox:  jmap.ID(mailboxID),
				NotKeyword: "$seen",
			},
			Limit: uint64(limit),
		}
		inv, err := c.doRetry(ctx, q)
		if err != nil {
			return marked, fmt.Errorf("jmapclient: Email/query: %w", err)
		}
		qr, ok := inv.Args.(*email.QueryResponse)
		if !ok {
			return marked, fmt.Errorf("jmapclient: Email/query: unexpected response type %T", inv.Args)
		}
		if len(qr.IDs) == 0 {
			return marked, nil
		}

		set := &email.Set{Account: jmap.ID(c.accountID)}
		set.Update = make(map[jmap.ID]jmap.Patch, len(qr.IDs))
		for _, id := range qr.IDs {
			set.Update[id] = jmap.Patch{"keywords/$seen": true}
		}
		sinv, err := c.doRetry(ctx, set)
		if err != nil {
			return marked, fmt.Errorf("jmapclient: Email/set: %w", err)
		}
		sr, ok := sinv.Args.(*flexibleSetResponse)
		if !ok {
			return marked, fmt.Errorf("jmapclient: Email/set: unexpected response type %T", sinv.Args)
		}
		marked += len(sr.Updated)
		if len(sr.NotUpdated) > 0 {
			return marked, fmt.Errorf("jmapclient: mark mailbox read: server rejected %d message(s)", len(sr.NotUpdated))
		}
		if len(sr.Updated) == 0 {
			// A server that reports success but no updates would leave the
			// page unread and loop forever; stop and let the caller resync.
			return marked, errors.New("jmapclient: mark mailbox read: server reported no updates")
		}
		if len(qr.IDs) < limit {
			return marked, nil
		}
	}
}

// markReadLimit is the sweep's chunk size: markReadChunk lowered by the
// server's core maxObjectsInSet when one is advertised.
func (c *Client) markReadLimit() int {
	limit := markReadChunk
	if coreCap, ok := c.session.Capabilities[jmap.CoreURI].(*core.Core); ok {
		if m := int(coreCap.MaxObjectsInSet); m > 0 && m < limit {
			limit = m
		}
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// doRetry runs one method, re-issuing it on a transient failure with a
// short linear backoff. Only the idempotent mark-read calls use it: a
// dropped connection, client timeout, or 5xx is retried; auth failures,
// method errors, and other 4xx are deterministic and return at once.
func (c *Client) doRetry(ctx context.Context, m jmap.Method) (*jmap.Invocation, error) {
	var err error
	for attempt := 0; attempt <= markReadRetries; attempt++ {
		if attempt > 0 && !waitCtx(ctx, time.Duration(attempt)*500*time.Millisecond) {
			return nil, ctx.Err()
		}
		var inv *jmap.Invocation
		inv, err = c.do(ctx, m)
		if err == nil {
			return inv, nil
		}
		if !transientMarkRead(err) {
			return nil, err
		}
	}
	return nil, err
}

// transientMarkRead reports whether err is worth another attempt: a
// transport failure (dropped connection, timeout) or a server-side 5xx.
// A 429 is already retried inside the transport, and 4xx/auth/method
// errors are deterministic.
func transientMarkRead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrAuth) || errors.Is(err, context.Canceled) {
		return false
	}
	var me *MethodCallError
	if errors.As(err, &me) {
		return false
	}
	var se *ServerError
	if errors.As(err, &se) {
		return se.Status >= http.StatusInternalServerError
	}
	return true
}
