package jmapclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// draftCreateHandle names the draft inside a batched Email/set so the
// EmailSubmission can reference it as "#draft" (RFC 8620 §3.7).
const draftCreateHandle = "draft"

// submissionCreateHandle names the submission; onSuccessUpdateEmail is
// keyed by it (RFC 8621 §7.5).
const submissionCreateHandle = "sub"

// toJMAPAddrs converts domain addresses to their wire form. A nil slice
// yields nil so `omitempty` omits it on create and `null` clears it on
// update.
func toJMAPAddrs(in []mail.Address) []*jmapmail.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]*jmapmail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, &jmapmail.Address{Name: a.Name, Email: a.Email})
	}
	return out
}

// attachmentParts renders uploaded blobs as attachment body parts
// (RFC 8621 §4.6: blobId only — never partId, never a Content-* header).
func attachmentParts(atts []mail.Attachment) []*email.BodyPart {
	if len(atts) == 0 {
		return nil
	}
	out := make([]*email.BodyPart, 0, len(atts))
	for _, a := range atts {
		t := a.Type
		if t == "" {
			t = "application/octet-stream"
		}
		out = append(out, &email.BodyPart{
			BlobID:      jmap.ID(a.BlobID),
			Name:        a.Name,
			Type:        t,
			Size:        uint64(a.Size),
			Disposition: "attachment",
		})
	}
	return out
}

// draftBody renders the plain-text body the way RFC 8621 §4.6 requires for
// create: exactly one text/plain part whose partId matches its bodyValues
// entry.
func draftBody(text string) (parts []*email.BodyPart, values map[string]*email.BodyValue) {
	return []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
		map[string]*email.BodyValue{"1": {Value: text}}
}

// draftCreate builds the Email/set create object for a new draft. Threading
// headers ride only here: inReplyTo/references are immutable (RFC 8621
// §4.1.2.5) so an update must not restate them.
func draftCreate(d mail.Draft) *email.Email {
	textBody, bodyValues := draftBody(d.Text)
	now := time.Now().UTC()
	e := &email.Email{
		MailboxIDs:  map[jmap.ID]bool{},
		Keywords:    map[string]bool{"$draft": true, "$seen": true},
		From:        toJMAPAddrs(d.From),
		To:          toJMAPAddrs(d.To),
		CC:          toJMAPAddrs(d.Cc),
		BCC:         toJMAPAddrs(d.Bcc),
		Subject:     d.Subject,
		TextBody:    textBody,
		BodyValues:  bodyValues,
		Attachments: attachmentParts(d.Attachments),
		ReceivedAt:  &now,
	}
	if d.MailboxID != "" {
		e.MailboxIDs[jmap.ID(d.MailboxID)] = true
	}
	if len(d.InReplyTo) > 0 {
		e.InReplyTo = append([]string(nil), d.InReplyTo...)
	}
	if len(d.References) > 0 {
		e.References = append([]string(nil), d.References...)
	}
	return e
}

// draftPatch is intentionally absent: Email content is immutable (RFC 8621
// §4.1.2), so there is no in-place draft update to build — see SaveDraft.

// SaveDraft implements mail.Provider (FR-H4).
//
// An edit is a *recreate*, not an update: every content property on Email
// (subject, from/to/cc/bcc, textBody, bodyValues, attachments) is immutable
// in RFC 8621 §4.1.2 — `Email/set update` may only patch keywords and
// mailbox membership, which live Stalwart enforces with invalidProperties.
// So the replacement is written first and the predecessor retired only once
// its successor exists: a rejected write leaves the old draft intact rather
// than losing the user's words.
func (c *Client) SaveDraft(ctx context.Context, d mail.Draft) (mail.ID, error) {
	registerEmailSetResponse()
	if c.session == nil {
		return d.ID, errors.New("jmapclient: not connected")
	}
	if d.MailboxID == "" {
		return d.ID, errors.New("jmapclient: SaveDraft: no drafts mailbox resolved")
	}

	created, err := c.createDraft(ctx, d)
	if err != nil {
		if d.ID != "" {
			// The old draft is still the live copy — hand it back so the
			// composer does not forget where its words live.
			return d.ID, err
		}
		return "", err
	}
	if d.ID != "" && d.ID != created {
		// Best effort: a failed retire leaves a stale row in Drafts, which
		// the user can delete — much better than losing the new copy.
		_ = c.destroyEmails(ctx, d.ID)
	}
	return created, nil
}

// createDraft performs the Email/set create and returns the server id.
func (c *Client) createDraft(ctx context.Context, d mail.Draft) (mail.ID, error) {
	set := &email.Set{
		Account: jmap.ID(c.accountID),
		Create:  map[jmap.ID]*email.Email{jmap.ID(draftCreateHandle): draftCreate(d)},
	}
	inv, err := c.do(ctx, set)
	if err != nil {
		return "", fmt.Errorf("jmapclient: Email/set draft: %w", err)
	}
	sr, ok := inv.Args.(*flexibleSetResponse)
	if !ok {
		return "", fmt.Errorf("jmapclient: Email/set draft: unexpected response type %T", inv.Args)
	}
	if len(sr.NotCreated) > 0 {
		return "", firstSetError("create", sr.NotCreated)
	}
	raw, ok := sr.Created[draftCreateHandle]
	if !ok {
		return "", errors.New("jmapclient: draft id missing from create response")
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("jmapclient: decode created draft: %w", err)
	}
	if created.ID == "" {
		return "", errors.New("jmapclient: created draft has no id")
	}
	return mail.ID(created.ID), nil
}

// destroyEmails permanently deletes ids; used to retire a draft whose
// replacement already exists.
func (c *Client) destroyEmails(ctx context.Context, ids ...mail.ID) error {
	if len(ids) == 0 {
		return nil
	}
	inv, err := c.do(ctx, &email.Set{Account: jmap.ID(c.accountID), Destroy: jmapIDs(ids)})
	if err != nil {
		return err
	}
	sr, ok := inv.Args.(*flexibleSetResponse)
	if !ok {
		return fmt.Errorf("jmapclient: Email/set destroy: unexpected response type %T", inv.Args)
	}
	if len(sr.NotDestroyed) > 0 {
		return firstSetError("destroy", sr.NotDestroyed)
	}
	return nil
}

// Send implements mail.Provider (FR-H5, FR-H6): one batched request carries
// the (usually already-flushed) draft and the EmailSubmission, and on
// success the submission's onSuccessUpdateEmail patch moves the Email out of
// Drafts into Sent and clears $draft. d.ID empty means the draft has never
// been saved, so the create rides along in the same round-trip.
//
// The response is scanned *in order*, never indexed by call id: Stalwart
// answers the implicit Email/set that onSuccessUpdateEmail triggers by
// reusing the submission's call id (PLAN §7, M2 observation), which would
// shadow the submission's own response in a map.
func (c *Client) Send(ctx context.Context, d mail.Draft) (mail.SendReceipt, error) {
	registerEmailSetResponse()
	if c.session == nil {
		return mail.SendReceipt{}, errors.New("jmapclient: not connected")
	}
	if d.IdentityID == "" {
		return mail.SendReceipt{}, errors.New("jmapclient: Send: no identity selected")
	}
	if d.MailboxID == "" {
		return mail.SendReceipt{}, errors.New("jmapclient: Send: no drafts mailbox resolved")
	}

	req := &jmap.Request{Context: ctx}
	var emailCall string // empty when the draft already lives on the server
	var emailRef jmap.ID
	if d.ID == "" {
		emailCall = req.Invoke(&email.Set{
			Account: jmap.ID(c.accountID),
			Create:  map[jmap.ID]*email.Email{jmap.ID(draftCreateHandle): draftCreate(d)},
		})
		emailRef = "#" + draftCreateHandle
	} else {
		emailRef = jmap.ID(d.ID)
	}

	sub := &emailsubmission.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			jmap.ID(submissionCreateHandle): {IdentityID: jmap.ID(d.IdentityID), EmailID: emailRef},
		},
	}
	if d.SentMailboxID != "" {
		// Keyed by the submission's creation reference; each value patches
		// the Email that submission points at (RFC 8621 §7.5).
		sub.OnSuccessUpdateEmail = map[jmap.ID]jmap.Patch{
			"#" + submissionCreateHandle: {
				"mailboxIds/" + string(d.MailboxID):     nil,
				"mailboxIds/" + string(d.SentMailboxID): true,
				"keywords/$draft":                       nil,
			},
		}
	} else {
		// No role-sent mailbox to file into: retire the submitted copy
		// rather than leave a $draft row behind (PLAN §7 degradation).
		sub.OnSuccessDestroyEmail = []jmap.ID{"#" + submissionCreateHandle}
	}
	subCall := req.Invoke(sub)

	resp, err := c.post(ctx, req)
	if err != nil {
		return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: %w", err)
	}

	out := mail.SendReceipt{EmailID: d.ID}
	var failures []string
	for _, inv := range resp.Responses {
		switch {
		case inv.Name == "error":
			me, ok := inv.Args.(*jmap.MethodError)
			if !ok {
				return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: error invocation with args %T", inv.Args)
			}
			return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: call %s: %w", inv.CallID,
				&MethodCallError{Type: me.Type, Description: deref(me.Description)})

		case emailCall != "" && inv.Name == "Email/set" && inv.CallID == emailCall:
			sr, ok := inv.Args.(*flexibleSetResponse)
			if !ok {
				return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: unexpected Email/set response %T", inv.Args)
			}
			if len(sr.NotCreated) > 0 {
				failures = append(failures, firstSetError("create", sr.NotCreated).Error())
			}
			if raw, ok := sr.Created[draftCreateHandle]; ok {
				var created struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &created); err == nil {
					out.EmailID = mail.ID(created.ID)
				}
			}

		case inv.Name == "EmailSubmission/set" && inv.CallID == subCall:
			ur, ok := inv.Args.(*emailsubmission.SetResponse)
			if !ok {
				return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: unexpected EmailSubmission/set response %T", inv.Args)
			}
			for handle, se := range ur.NotCreated {
				failures = append(failures, fmt.Sprintf("submission %s: %s", handle, setErr(se)))
			}
			for _, s := range ur.Created {
				if s == nil {
					continue
				}
				out.SubmissionID = mail.ID(s.ID)
				out.UndoStatus = s.UndoStatus
				if s.SendAt != nil {
					out.SendAt = *s.SendAt
				}
			}
		}
	}
	if len(failures) > 0 {
		return mail.SendReceipt{}, fmt.Errorf("jmapclient: send: %s", failures[0])
	}
	if out.SubmissionID == "" {
		return mail.SendReceipt{}, errors.New("jmapclient: send: no EmailSubmission created")
	}
	if out.EmailID == "" {
		return mail.SendReceipt{}, errors.New("jmapclient: send: draft id never resolved")
	}
	return out, nil
}

// firstSetError renders the first entry of a /set rejection map.
func firstSetError(op string, m map[string]*jmap.SetError) error {
	for id, se := range m {
		return fmt.Errorf("email/set %s %s: %w", op, id, setErr(se))
	}
	return errors.New("rejected by server")
}
