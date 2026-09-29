package jmapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// summaryProperties is the small property set held for every message in a
// list window (FR-D4): enough to render rows, never a body.
var summaryProperties = []string{
	"threadId", "mailboxIds", "keywords", "from", "to", "subject",
	"receivedAt", "size", "hasAttachment", "preview",
}

// OpenQuery implements mail.Provider. The Email/query and the first
// Email/get travel in one round-trip via an #ids back-reference (FR-K4);
// every Page does the same.
func (c *Client) OpenQuery(ctx context.Context, spec mail.QuerySpec) (mail.QueryHandle, []mail.EmailSummary, error) {
	if c.session == nil {
		return nil, nil, errors.New("jmapclient: not connected")
	}
	h := &queryHandle{c: c, spec: spec}
	sums, err := h.page(ctx, spec.Position, spec.Limit)
	if err != nil {
		return nil, nil, err
	}
	return h, sums, nil
}

// queryHandle is the JMAP implementation of mail.QueryHandle.
type queryHandle struct {
	c          *Client
	spec       mail.QuerySpec
	ids        []mail.ID
	start      int // absolute position of the current page
	total      int
	state      string // queryState
	emailState string // Email/get state at fetch time (FR-B5 bootstrap)
}

func (h *queryHandle) IDs() []mail.ID     { return h.ids }
func (h *queryHandle) Start() int         { return h.start }
func (h *queryHandle) Total() int         { return h.total }
func (h *queryHandle) State() string      { return h.state }
func (h *queryHandle) EmailState() string { return h.emailState }

// Page implements mail.QueryHandle.
func (h *queryHandle) Page(ctx context.Context, position, limit int) ([]mail.ID, []mail.EmailSummary, error) {
	sums, err := h.page(ctx, position, limit)
	if err != nil {
		return nil, nil, err
	}
	return h.ids, sums, nil
}

// page issues the batched query+get and installs the result on the handle.
func (h *queryHandle) page(ctx context.Context, position, limit int) ([]mail.EmailSummary, error) {
	if limit <= 0 {
		limit = 1
	}
	req := &jmap.Request{Context: ctx}
	q := &email.Query{
		Account:         jmap.ID(h.c.accountID),
		Filter:          filterFor(h.spec),
		Sort:            sortFor(h.spec.Sort),
		CollapseThreads: h.spec.CollapseThreads,
		Position:        int64(position),
		Limit:           uint64(limit),
		CalculateTotal:  true,
	}
	if h.spec.AnchorID != "" {
		q.Position = 0
		q.Anchor = jmap.ID(h.spec.AnchorID)
		q.AnchorOffset = int64(h.spec.AnchorOffset)
	}
	qID := req.Invoke(q)
	get := &email.Get{
		Account:      jmap.ID(h.c.accountID),
		Properties:   summaryProperties,
		ReferenceIDs: &jmap.ResultReference{ResultOf: qID, Name: q.Name(), Path: "/ids"},
	}
	gID := req.Invoke(get)

	invs, err := h.c.runBatch(ctx, req)
	if err != nil {
		return nil, err
	}
	qinv, ok := invs[qID]
	if !ok {
		return nil, fmt.Errorf("jmapclient: Email/query: no response for call %q", qID)
	}
	qr, ok := qinv.Args.(*email.QueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Email/query: unexpected response type %T", qinv.Args)
	}
	ginv, ok := invs[gID]
	if !ok {
		return nil, fmt.Errorf("jmapclient: Email/get: no response for call %q", gID)
	}
	gr, ok := ginv.Args.(*email.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Email/get: unexpected response type %T", ginv.Args)
	}

	h.ids = convertIDs(qr.IDs)
	h.start = position
	if h.spec.AnchorID != "" {
		// Anchor addressing: the requested position is meaningless; the
		// server reports where the result actually begins (FR-B5).
		h.start = int(qr.Position)
	}
	h.total = int(qr.Total)
	h.state = qr.QueryState
	h.emailState = gr.State
	return convertSummaries(gr.List), nil
}

// FetchSummaries implements mail.Provider for targeted refetches (window
// repairs, M2 change reconciliation).
func (c *Client) FetchSummaries(ctx context.Context, ids []mail.ID) ([]mail.EmailSummary, error) {
	if c.session == nil {
		return nil, errors.New("jmapclient: not connected")
	}
	get := &email.Get{
		Account:    jmap.ID(c.accountID),
		IDs:        jmapIDs(ids),
		Properties: summaryProperties,
	}
	inv, err := c.do(ctx, get)
	if err != nil {
		return nil, fmt.Errorf("jmapclient: Email/get: %w", err)
	}
	gr, ok := inv.Args.(*email.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmapclient: Email/get: unexpected response type %T", inv.Args)
	}
	return convertSummaries(gr.List), nil
}

// bodyFetchProperties is the full property set for body fetches: summary
// metadata plus the body structure parts and their values. Listing the
// body properties explicitly is required — a Properties list that omits
// them yields null body structure even with FetchAllBodyValues. The
// addressing and RFC 5322 threading fields ride along so one fetch serves
// both the preview and a reply/forward (FR-H2).
var bodyFetchProperties = append(append([]string{}, summaryProperties...),
	"blobId", "textBody", "htmlBody", "attachments", "bodyValues",
	"cc", "bcc", "replyTo", "messageId", "references", "inReplyTo")

// bodyProperties keeps the fetched body parts lean: identity enough to
// download attachments later, never content (FR-E4, NFR-4).
var bodyProperties = []string{"partId", "blobId", "size", "name", "type", "charset", "disposition"}

// FetchBody implements mail.Provider with the FR-E2 preference order: the
// first text/plain body part wins; the first text/html part is returned raw
// for in-repo conversion otherwise.
func (c *Client) FetchBody(ctx context.Context, id mail.ID) (mail.EmailBody, error) {
	if c.session == nil {
		return mail.EmailBody{}, errors.New("jmapclient: not connected")
	}
	get := &email.Get{
		Account:            jmap.ID(c.accountID),
		IDs:                []jmap.ID{jmap.ID(id)},
		Properties:         bodyFetchProperties,
		BodyProperties:     bodyProperties,
		FetchAllBodyValues: true,
	}
	inv, err := c.do(ctx, get)
	if err != nil {
		return mail.EmailBody{}, fmt.Errorf("jmapclient: Email/get body: %w", err)
	}
	gr, ok := inv.Args.(*email.GetResponse)
	if !ok {
		return mail.EmailBody{}, fmt.Errorf("jmapclient: Email/get body: unexpected response type %T", inv.Args)
	}
	if len(gr.List) == 0 {
		return mail.EmailBody{}, fmt.Errorf("jmapclient: Email/get body: %w: %s", ErrNotFound, id)
	}
	return convertBody(gr.List[0]), nil
}

func convertBody(e *email.Email) mail.EmailBody {
	body := mail.EmailBody{
		ID:         mail.ID(e.ID),
		ThreadID:   mail.ID(e.ThreadID),
		From:       convertAddresses(e.From),
		To:         convertAddresses(e.To),
		Cc:         convertAddresses(e.CC),
		Bcc:        convertAddresses(e.BCC),
		ReplyTo:    convertAddresses(e.ReplyTo),
		Subject:    e.Subject,
		MessageID:  append([]string(nil), e.MessageID...),
		References: append([]string(nil), e.References...),
		InReplyTo:  append([]string(nil), e.InReplyTo...),
	}
	if e.ReceivedAt != nil {
		body.ReceivedAt = *e.ReceivedAt
	}
	text := firstBodyValue(e.TextBody, e.BodyValues, "text/plain")
	html := firstBodyValue(e.HTMLBody, e.BodyValues, "text/html")
	// FR-E2: text/plain wins, but a degenerate placeholder part yields to
	// the HTML alternative.
	if text != "" && (!isDegenerateText(text) || html == "") {
		body.Text = text
	} else {
		body.HTML = html
	}
	for _, a := range e.Attachments {
		body.Attachments = append(body.Attachments, mail.Attachment{
			BlobID: mail.ID(a.BlobID),
			Name:   a.Name,
			Type:   a.Type,
			Size:   int64(a.Size),
		})
	}
	return body
}

// firstBodyValue returns the value of the first part of the given MIME type
// present in bodyValues, or "" when none carries a value.
func firstBodyValue(parts []*email.BodyPart, values map[string]*email.BodyValue, mime string) string {
	for _, p := range parts {
		if p.Type != mime {
			continue
		}
		if bv := values[p.PartID]; bv != nil {
			return bv.Value
		}
	}
	return ""
}

// maxDegenerateText is the length below which a text/plain alternative is
// treated as a sender placeholder rather than the real body. GOG.com ships
// "Plain text version not available" (31 chars) as its text part; such
// senders intend the HTML alternative to be read (FR-E2).
const maxDegenerateText = 120

// isDegenerateText reports whether a text/plain part is placeholder-only
// (very short or whitespace-only) rather than a real plain-text body.
func isDegenerateText(s string) bool {
	return len(strings.TrimSpace(s)) < maxDegenerateText
}

// filterFor maps a query spec onto the RFC 8621 §4.4.1 condition. Mailbox
// browsing sends inMailbox alone; search queries (FR-F1) add the content
// fields, with MailboxID acting as the scope — empty meaning all mailboxes.
// hasAttachment=false stays local: go-jmap's bool omitempty cannot
// transmit it (mail.SearchFilter documents the degradation).
func filterFor(spec mail.QuerySpec) *email.FilterCondition {
	cond := &email.FilterCondition{}
	if spec.Search != nil {
		s := spec.Search
		cond.Text, cond.From, cond.To, cond.Subject = s.Text, s.From, s.To, s.Subject
		if !s.After.IsZero() {
			after := s.After
			cond.After = &after
		}
		if !s.Before.IsZero() {
			before := s.Before
			cond.Before = &before
		}
		cond.HasKeyword = s.HasKeyword
		if s.HasAttachment != nil && *s.HasAttachment {
			cond.HasAttachment = true
		}
	}
	if spec.MailboxID != "" {
		cond.InMailbox = jmap.ID(spec.MailboxID)
	}
	return cond
}

func sortFor(crits []mail.SortCriterion) []*email.SortComparator {
	if len(crits) == 0 {
		crits = []mail.SortCriterion{{Property: "receivedAt", IsDescending: true}}
	}
	out := make([]*email.SortComparator, 0, len(crits))
	for _, c := range crits {
		out = append(out, &email.SortComparator{Property: c.Property, IsAscending: !c.IsDescending})
	}
	return out
}

func convertIDs(ids []jmap.ID) []mail.ID {
	out := make([]mail.ID, 0, len(ids))
	for _, id := range ids {
		out = append(out, mail.ID(id))
	}
	return out
}

func jmapIDs(ids []mail.ID) []jmap.ID {
	out := make([]jmap.ID, 0, len(ids))
	for _, id := range ids {
		out = append(out, jmap.ID(id))
	}
	return out
}

func convertSummaries(list []*email.Email) []mail.EmailSummary {
	out := make([]mail.EmailSummary, 0, len(list))
	for _, e := range list {
		s := mail.EmailSummary{
			ID:            mail.ID(e.ID),
			ThreadID:      mail.ID(e.ThreadID),
			Keywords:      convertKeywords(e.Keywords),
			From:          convertAddresses(e.From),
			To:            convertAddresses(e.To),
			Subject:       e.Subject,
			Size:          int64(e.Size),
			HasAttachment: e.HasAttachment,
			Preview:       e.Preview,
		}
		for id := range e.MailboxIDs {
			s.MailboxIDs = append(s.MailboxIDs, mail.ID(id))
		}
		if e.ReceivedAt != nil {
			s.ReceivedAt = *e.ReceivedAt
		}
		out = append(out, s)
	}
	return out
}

func convertKeywords(kw map[string]bool) mail.Keywords {
	if len(kw) == 0 {
		return nil
	}
	out := make(mail.Keywords, len(kw))
	for k, v := range kw {
		if v {
			out[k] = struct{}{}
		}
	}
	return out
}

func convertAddresses(addrs []*jmapmail.Address) []mail.Address {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		out = append(out, mail.Address{Name: a.Name, Email: a.Email})
	}
	return out
}
