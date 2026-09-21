package jmapclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	c     *Client
	spec  mail.QuerySpec
	ids   []mail.ID
	start int // absolute position of the current page
	total int
	state string
}

func (h *queryHandle) IDs() []mail.ID { return h.ids }
func (h *queryHandle) Start() int     { return h.start }
func (h *queryHandle) Total() int     { return h.total }
func (h *queryHandle) State() string  { return h.state }

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
	var qm jmap.Method
	var queryName string
	if h.spec.ThreadID != "" {
		// inThread lives inside the filter object per RFC 8621 §4.4.1;
		// v0.5.3's FilterCondition omits it, so the wrapper extends.
		f, err := json.Marshal(map[string]string{"inThread": string(h.spec.ThreadID)})
		if err != nil {
			return nil, fmt.Errorf("jmapclient: encode inThread filter: %w", err)
		}
		tq := &threadQuery{
			Account:         jmap.ID(h.c.accountID),
			Filter:          f,
			Sort:            sortFor(h.spec.Sort),
			Position:        int64(position),
			Limit:           uint64(limit),
			CalculateTotal:  true,
			CollapseThreads: h.spec.CollapseThreads,
		}
		qm, queryName = tq, tq.Name()
	} else {
		q := &email.Query{
			Account:         jmap.ID(h.c.accountID),
			Filter:          &email.FilterCondition{InMailbox: jmap.ID(h.spec.MailboxID)},
			Sort:            sortFor(h.spec.Sort),
			CollapseThreads: h.spec.CollapseThreads,
			Position:        int64(position),
			Limit:           uint64(limit),
			CalculateTotal:  true,
		}
		qm, queryName = q, q.Name()
	}
	qID := req.Invoke(qm)
	get := &email.Get{
		Account:      jmap.ID(h.c.accountID),
		Properties:   summaryProperties,
		ReferenceIDs: &jmap.ResultReference{ResultOf: qID, Name: queryName, Path: "/ids"},
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
	h.total = int(qr.Total)
	h.state = qr.QueryState
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

// bodyProperties keeps the fetched body lean: identity enough to download
// attachments later, never content (FR-E4, NFR-4).
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
		Properties:         summaryProperties,
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
	body := mail.EmailBody{ID: mail.ID(e.ID)}
	for _, p := range e.TextBody {
		if p.Type != "text/plain" {
			continue
		}
		if bv := e.BodyValues[p.PartID]; bv != nil {
			body.Text = bv.Value
			break
		}
	}
	if body.Text == "" {
		for _, p := range e.HTMLBody {
			if p.Type != "text/html" {
				continue
			}
			if bv := e.BodyValues[p.PartID]; bv != nil {
				body.HTML = bv.Value
				break
			}
		}
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

// threadQuery carries an Email/query with an RFC 8621 §4.4.1 inThread
// filter condition, which v0.5.3's FilterCondition struct omits (PLAN §9:
// extend the wrapper, not the app). Filter is pre-marshalled so inThread
// lands inside the filter object, where the RFC puts it.
type threadQuery struct {
	Account         jmap.ID                 `json:"accountId,omitempty"`
	Filter          json.RawMessage         `json:"filter,omitempty"`
	Sort            []*email.SortComparator `json:"sort,omitempty"`
	Position        int64                   `json:"position,omitempty"`
	Limit           uint64                  `json:"limit,omitempty"`
	CalculateTotal  bool                    `json:"calculateTotal,omitempty"`
	CollapseThreads bool                    `json:"collapseThreads,omitempty"`
}

func (tq *threadQuery) Name() string         { return "Email/query" }
func (tq *threadQuery) Requires() []jmap.URI { return []jmap.URI{jmapmail.URI} }

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
