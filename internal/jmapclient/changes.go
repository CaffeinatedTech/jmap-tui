package jmapclient

import (
	"context"
	"errors"
	"fmt"

	jmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/identity"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// EmailChanges implements mail.Provider. Created ids fold into Updated:
// RFC 8620 §5.2 delivers both as "changed since oldState" from the
// reconciler's point of view.
func (c *Client) EmailChanges(ctx context.Context, sinceState string) (mail.EmailChangeSet, error) {
	if c.session == nil {
		return mail.EmailChangeSet{}, errors.New("jmapclient: not connected")
	}
	inv, err := c.do(ctx, &email.Changes{Account: jmap.ID(c.accountID), SinceState: sinceState})
	if err != nil {
		return mail.EmailChangeSet{}, wrapChangesErr("Email/changes", err)
	}
	cr, ok := inv.Args.(*email.ChangesResponse)
	if !ok {
		return mail.EmailChangeSet{}, fmt.Errorf("jmapclient: Email/changes: unexpected response type %T", inv.Args)
	}
	out := mail.EmailChangeSet{
		Updated:   convertIDs(append(append([]jmap.ID{}, cr.Created...), cr.Updated...)),
		Destroyed: convertIDs(cr.Destroyed),
		NewState:  cr.NewState,
		HasMore:   cr.HasMoreChanges,
	}
	return out, nil
}

// MailboxChanges implements mail.Provider.
func (c *Client) MailboxChanges(ctx context.Context, sinceState string) (mail.MailboxChangeSet, error) {
	if c.session == nil {
		return mail.MailboxChangeSet{}, errors.New("jmapclient: not connected")
	}
	inv, err := c.do(ctx, &mailbox.Changes{Account: jmap.ID(c.accountID), SinceState: sinceState})
	if err != nil {
		return mail.MailboxChangeSet{}, wrapChangesErr("Mailbox/changes", err)
	}
	cr, ok := inv.Args.(*mailbox.ChangesResponse)
	if !ok {
		return mail.MailboxChangeSet{}, fmt.Errorf("jmapclient: Mailbox/changes: unexpected response type %T", inv.Args)
	}
	out := mail.MailboxChangeSet{
		Updated:   convertIDs(append(append([]jmap.ID{}, cr.Created...), cr.Updated...)),
		Destroyed: convertIDs(cr.Destroyed),
		NewState:  cr.NewState,
		HasMore:   cr.HasMoreChanges,
	}
	return out, nil
}

// wrapChangesErr tags cannotCalculateChanges failures with the mail
// sentinel so callers branch with errors.Is; other method errors pass
// through.
func wrapChangesErr(call string, err error) error {
	var mce *MethodCallError
	if errors.As(err, &mce) && mce.Type == "cannotCalculateChanges" {
		return fmt.Errorf("jmapclient: %s: %w", call, mail.ErrCannotCalculateChanges)
	}
	return fmt.Errorf("jmapclient: %s: %w", call, err)
}

// Identities implements mail.Provider (FR-B1). Servers without the
// submission capability yield an empty list, never an error (FR-A6).
func (c *Client) Identities(ctx context.Context) ([]mail.Identity, error) {
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}
	if _, ok := c.session.RawCapabilities[emailsubmission.URI]; !ok {
		return nil, nil
	}
	inv, err := c.do(ctx, &identity.Get{Account: jmap.ID(c.accountID)})
	if err != nil {
		return nil, fmt.Errorf("jmapclient: Identity/get: %w", err)
	}
	gr, ok := inv.Args.(*identity.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Identity/get: unexpected response type %T", inv.Args)
	}
	out := make([]mail.Identity, 0, len(gr.List))
	for _, id := range gr.List {
		if id == nil {
			continue
		}
		out = append(out, mail.Identity{ID: mail.ID(id.ID), Name: id.Name, Email: id.Email})
	}
	return out, nil
}
