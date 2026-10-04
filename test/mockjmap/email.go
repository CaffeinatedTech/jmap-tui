package mockjmap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Address is a fixture sender/recipient.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Email is a fixture email served by the fake. Bodies are plain strings —
// the fake does not model MIME structure, only the two part types the
// client reads (text/plain and text/html) plus attachment metadata.
type Email struct {
	ID            string
	ThreadID      string
	MailboxIDs    []string
	Keywords      map[string]bool
	MessageID     []string
	References    []string
	InReplyTo     []string
	From          []Address
	To            []Address
	Cc            []Address
	Bcc           []Address
	ReplyTo       []Address
	Subject       string
	ReceivedAt    time.Time
	Size          uint64
	HasAttachment bool
	Preview       string
	TextBody      string
	HTMLBody      string
	Attachments   []Attachment
}

// Attachment is a fixture attachment blob reference.
type Attachment struct {
	BlobID string
	Name   string
	Type   string
	Size   uint64
}

// SyntheticMailbox declares a virtual mailbox with Count generated
// messages: ids "<prefix>-<index>", deterministically derived content, and
// receivedAt descending in index order. Nothing is materialised until a
// query or get touches it, so 10k+ message fixtures stay cheap (M1 soak).
type SyntheticMailbox struct {
	MailboxID string
	Prefix    string
	Count     int
}

// syntheticBase is a fixed timestamp so tests never depend on wall time.
var syntheticBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// SetEmails replaces the email fixtures and journals the change: ids
// present in both sets count as updated, ids only in the old set count as
// destroyed.
func (s *Server) SetEmails(emails []Email) {
	old := make(map[string]bool)
	s.mu.Lock()
	for _, e := range s.emails {
		old[e.ID] = true
	}
	var updated, destroyed []string
	for _, e := range emails {
		delete(old, e.ID)
		updated = append(updated, e.ID)
	}
	for id := range old {
		destroyed = append(destroyed, id)
	}
	s.emails = append([]Email(nil), emails...)
	s.emailVersion++
	s.journal = append(s.journal, journalEntry{typ: "Email", updated: updated, destroyed: destroyed, version: s.emailVersion})
	s.mu.Unlock()
}

// CreateEmails appends fixtures and journals them as created (M2 sync tests
// drive live arrival this way).
func (s *Server) CreateEmails(emails []Email) {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := make([]string, 0, len(emails))
	for _, e := range emails {
		s.emails = append(s.emails, e)
		updated = append(updated, e.ID)
	}
	s.emailVersion++
	s.journal = append(s.journal, journalEntry{typ: "Email", updated: updated, version: s.emailVersion})
}

// UpdateEmails mutates the named fixtures in place and journals them as
// updated (flag flips, subject edits, moves). Unknown ids are ignored.
func (s *Server) UpdateEmails(ids []string, fn func(*Email)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := []string{}
	for i := range s.emails {
		if contains(ids, s.emails[i].ID) {
			fn(&s.emails[i])
			updated = append(updated, s.emails[i].ID)
		}
	}
	if len(updated) == 0 {
		return
	}
	s.emailVersion++
	s.journal = append(s.journal, journalEntry{typ: "Email", updated: updated, version: s.emailVersion})
}

// DestroyEmails removes fixtures and journals them as destroyed.
func (s *Server) DestroyEmails(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}
	kept := s.emails[:0]
	destroyed := []string{}
	for _, e := range s.emails {
		if gone[e.ID] {
			destroyed = append(destroyed, e.ID)
			continue
		}
		kept = append(kept, e)
	}
	if len(destroyed) == 0 {
		return
	}
	s.emails = kept
	s.emailVersion++
	s.journal = append(s.journal, journalEntry{typ: "Email", destroyed: destroyed, version: s.emailVersion})
}

// SetSyntheticMailbox sets (or clears with Count 0) the virtual mailbox.
// Synthetic content is generated on demand, so the journal records a bare
// version bump: /changes reports a new state with no per-id detail.
func (s *Server) SetSyntheticMailbox(sm SyntheticMailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	smCopy := sm
	s.synthetic = &smCopy
	s.emailVersion++
	s.journal = append(s.journal, journalEntry{typ: "Email", version: s.emailVersion})
}

// emailSnapshot is an immutable view of the email fixtures, taken under the
// server lock so request handling never holds it.
type emailSnapshot struct {
	emails         []Email
	synthetic      *SyntheticMailbox
	emailVersion   int
	mailboxVersion int
}

// lookup resolves real and synthetic fixture ids.
func (snap *emailSnapshot) lookup(id string) (Email, bool) {
	if snap.synthetic != nil && snap.synthetic.Count > 0 && strings.HasPrefix(id, snap.synthetic.Prefix+"-") {
		if idx, err := strconv.Atoi(strings.TrimPrefix(id, snap.synthetic.Prefix+"-")); err == nil && idx >= 0 && idx < snap.synthetic.Count {
			return syntheticEmail(*snap.synthetic, idx), true
		}
	}
	for i := range snap.emails {
		if snap.emails[i].ID == id {
			return snap.emails[i], true
		}
	}
	return Email{}, false
}

func syntheticEmail(sm SyntheticMailbox, idx int) Email {
	id := fmt.Sprintf("%s-%06d", sm.Prefix, idx)
	return Email{
		ID:         id,
		ThreadID:   id, // each synthetic message is its own thread
		MailboxIDs: []string{sm.MailboxID},
		Keywords:   map[string]bool{"$seen": idx%3 == 0},
		From:       []Address{{Name: "Synthetic Sender", Email: "synthetic@example.test"}},
		To:         []Address{{Name: "Test Recipient", Email: "recipient@example.test"}},
		Subject:    fmt.Sprintf("Synthetic message %06d", idx),
		ReceivedAt: syntheticBase.Add(-time.Duration(idx) * 24 * time.Hour),
		Size:       4096,
		Preview:    fmt.Sprintf("Body of synthetic message %06d.", idx),
		TextBody:   fmt.Sprintf("Body of synthetic message %06d.\n\nLine two.\n", idx),
	}
}

// resultRef is the wire form of an RFC 8620 §3.7 result reference.
type resultRef struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// queryFilter is the subset of Email/query FilterCondition the fake
// honours (FR-F1): mailbox scoping plus the search fields. Thread
// membership is not a filter — Thread/get answers it (RFC 8621 §3).
type queryFilter struct {
	InMailbox     string     `json:"inMailbox"`
	Text          string     `json:"text"`
	From          string     `json:"from"`
	To            string     `json:"to"`
	Subject       string     `json:"subject"`
	After         *time.Time `json:"after"`
	Before        *time.Time `json:"before"`
	HasKeyword    string     `json:"hasKeyword"`
	NotKeyword    string     `json:"notKeyword"`
	HasAttachment *bool      `json:"hasAttachment"`
}

type querySort struct {
	Property     string `json:"property"`
	IsAscending  bool   `json:"isAscending"`
	IsDescending bool   `json:"isDescending"`
}

type emailQueryArgs struct {
	Filter          queryFilter `json:"filter"`
	Sort            []querySort `json:"sort"`
	Position        int         `json:"position"`
	Anchor          string      `json:"anchor"`
	AnchorOffset    int         `json:"anchorOffset"`
	Limit           int         `json:"limit"`
	CalculateTotal  bool        `json:"calculateTotal"`
	CollapseThreads bool        `json:"collapseThreads"`
}

// scopedEmail is a candidate result: identity and ordering keys only.
type scopedEmail struct {
	id       string
	threadID string
	at       time.Time
	from     string // lowercased senders, for from sorting
	subject  string
	size     uint64
}

// emailSortLess orders two candidates by one sort property; unknown
// properties fall back to receivedAt. Equal keys keep caller stability.
func emailSortLess(a, b scopedEmail, prop string) bool {
	switch prop {
	case "from":
		return a.from < b.from
	case "subject":
		return a.subject < b.subject
	case "size":
		return a.size < b.size
	default: // "receivedAt" and friends
		return a.at.Before(b.at)
	}
}

// hasSearchFilter reports whether the filter carries content conditions
// beyond mailbox scoping — the signal to materialise and match full
// emails instead of riding the cheap tuple fast-path.
func (f queryFilter) hasSearchFilter() bool {
	return f.Text != "" || f.From != "" || f.To != "" || f.Subject != "" ||
		f.After != nil || f.Before != nil || f.HasKeyword != "" ||
		f.NotKeyword != "" || f.HasAttachment != nil
}

// tokenHaystack lowercases and splits content into whole tokens —
// Stalwart-class servers match whole tokens only (PLAN §7): a partial
// word matches nothing on any searchable field.
func tokenHaystack(parts ...string) map[string]bool {
	tokens := map[string]bool{}
	for _, p := range parts {
		for _, f := range strings.Fields(strings.ToLower(p)) {
			tokens[f] = true
		}
	}
	return tokens
}

// tokenMatch reports whether every word of the needle appears as a whole
// token (servers tokenize the needle too — a multi-word needle is an AND
// of its words).
func tokenMatch(tokens map[string]bool, needle string) bool {
	for _, w := range strings.Fields(strings.ToLower(needle)) {
		if !tokens[w] {
			return false
		}
	}
	return true
}

// matchesSearch applies the RFC 8621 §4.4.1 conditions the fake honours,
// with the token semantics real servers use (verified against Stalwart,
// PLAN §7): text/from/to/subject match whole tokens only, AND across a
// multi-word needle; before/after are exclusive receivedAt bounds.
func matchesSearch(e Email, f queryFilter) bool {
	if f.Text != "" {
		froms, tos := addrStrings(e.From), addrStrings(e.To)
		parts := append([]string{e.Subject, e.TextBody, e.HTMLBody, e.Preview}, froms...)
		parts = append(parts, tos...)
		if !tokenMatch(tokenHaystack(parts...), f.Text) {
			return false
		}
	}
	if f.From != "" && !tokenMatch(tokenHaystack(addrStrings(e.From)...), f.From) {
		return false
	}
	if f.To != "" && !tokenMatch(tokenHaystack(addrStrings(e.To)...), f.To) {
		return false
	}
	if f.Subject != "" && !tokenMatch(tokenHaystack(e.Subject), f.Subject) {
		return false
	}
	if f.After != nil && !e.ReceivedAt.After(*f.After) {
		return false
	}
	if f.Before != nil && !e.ReceivedAt.Before(*f.Before) {
		return false
	}
	if f.HasKeyword != "" && !e.Keywords[f.HasKeyword] {
		return false
	}
	if f.NotKeyword != "" && e.Keywords[f.NotKeyword] {
		return false
	}
	if f.HasAttachment != nil && *f.HasAttachment != e.HasAttachment {
		return false
	}
	return true
}

// addrStrings flattens addresses into "Name Email" strings for tokenising.
func addrStrings(addrs []Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Name+" "+a.Email)
	}
	return out
}

// scoped resolves the candidate set for a query filter. Synthetic scopes
// are generated lazily as tuples; search filters materialise candidates
// and match full content (FR-F1).
func (snap *emailSnapshot) scoped(f queryFilter) []scopedEmail {
	search := f.hasSearchFilter()
	var out []scopedEmail
	add := func(e Email) {
		if search && !matchesSearch(e, f) {
			return
		}
		out = append(out, scopedEmail{
			id:       e.ID,
			threadID: e.ThreadID,
			at:       e.ReceivedAt,
			from:     strings.ToLower(strings.Join(addrStrings(e.From), ", ")),
			subject:  strings.ToLower(e.Subject),
			size:     e.Size,
		})
	}
	switch {
	case snap.synthetic != nil && f.InMailbox == snap.synthetic.MailboxID:
		for i := 0; i < snap.synthetic.Count; i++ {
			add(syntheticEmail(*snap.synthetic, i))
		}
	default:
		for i := range snap.emails {
			if f.InMailbox == "" || contains(snap.emails[i].MailboxIDs, f.InMailbox) {
				add(snap.emails[i])
			}
		}
		// Synthetic content lives in exactly one mailbox; it joins the
		// candidate set under the all-mailbox scope too.
		if snap.synthetic != nil && f.InMailbox == "" && snap.synthetic.Count > 0 {
			for i := 0; i < snap.synthetic.Count; i++ {
				add(syntheticEmail(*snap.synthetic, i))
			}
		}
	}
	return out
}

func emailQueryResponse(snap *emailSnapshot, args json.RawMessage) map[string]any {
	var q emailQueryArgs
	_ = json.Unmarshal(args, &q)

	cands := snap.scoped(q.Filter)

	// RFC 8621 §4.4.1: default sort is receivedAt descending. Any
	// property the client asks for is honoured (FR-D8).
	prop := "receivedAt"
	desc := true
	if len(q.Sort) > 0 {
		if q.Sort[0].Property != "" {
			prop = q.Sort[0].Property
		}
		desc = !q.Sort[0].IsAscending
	}
	sort.SliceStable(cands, func(i, j int) bool {
		less := emailSortLess(cands[i], cands[j], prop)
		if desc {
			return !less && emailSortLess(cands[j], cands[i], prop)
		}
		return less
	})

	if q.CollapseThreads {
		seen := map[string]bool{}
		kept := cands[:0]
		for _, c := range cands {
			if seen[c.threadID] {
				continue
			}
			seen[c.threadID] = true
			kept = append(kept, c)
		}
		cands = kept
	}

	// Anchor: position = index of the anchor id, plus the requested offset.
	position := q.Position
	if q.Anchor != "" {
		position = len(cands)
		for i, c := range cands {
			if c.id == q.Anchor {
				position = i + q.AnchorOffset
				break
			}
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = len(cands)
	}
	start := position
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > len(cands) {
		end = len(cands)
	}
	ids := make([]string, 0, max(0, end-start))
	for i := start; i < end; i++ {
		ids = append(ids, cands[i].id)
	}

	// total is always included; calculateTotal is accepted but harmless to
	// over-send for a test double.
	return map[string]any{
		"accountId":           "acc1",
		"queryState":          fmt.Sprintf("q-%d", snap.emailVersion),
		"canCalculateChanges": false,
		"position":            start,
		"ids":                 ids,
		"total":               len(cands),
	}
}

// threadGetResponse answers Thread/get (RFC 8621 §3.1): the member ids of
// each requested thread, sorted receivedAt ascending exactly as the RFC
// orders Thread.emailIds — oldest first, so a client can expand in
// reading order without a second sort. A thread nothing belongs to is
// reported notFound, never an empty list at an existing index.
func threadGetResponse(snap *emailSnapshot, args json.RawMessage) map[string]any {
	var g struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal(args, &g)

	list := []map[string]any{}
	notFound := []string{}
	for _, id := range g.IDs {
		members := snap.threadMembers(id)
		if len(members) == 0 {
			notFound = append(notFound, id)
			continue
		}
		ids := make([]string, 0, len(members))
		for _, e := range members {
			ids = append(ids, e.ID)
		}
		list = append(list, map[string]any{"id": id, "emailIds": ids})
	}
	resp := map[string]any{
		"accountId": "acc1",
		"state":     fmt.Sprintf("t-%d", snap.emailVersion),
		"list":      list,
	}
	if len(notFound) > 0 {
		resp["notFound"] = notFound
	}
	return resp
}

// threadMembers collects a thread's fixtures oldest-first. Synthetic
// messages are each their own thread (ThreadID == id), so a synthetic id
// resolves to exactly itself — the single-message case the client must
// render without a chevron.
func (snap *emailSnapshot) threadMembers(threadID string) []Email {
	var out []Email
	for i := range snap.emails {
		if snap.emails[i].ThreadID == threadID {
			out = append(out, snap.emails[i])
		}
	}
	if len(out) == 0 {
		if e, ok := snap.lookup(threadID); ok && e.ThreadID == threadID {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	return out
}

// bodyGetRequested reports whether an Email/get call asks for body
// values, which distinguishes a body hydration from a summary fetch.
func bodyGetRequested(args json.RawMessage) bool {
	var g struct {
		FetchTextBodyValues bool `json:"fetchTextBodyValues"`
		FetchHTMLBodyValues bool `json:"fetchHTMLBodyValues"`
		FetchAllBodyValues  bool `json:"fetchAllBodyValues"`
	}
	_ = json.Unmarshal(args, &g)
	return g.FetchTextBodyValues || g.FetchHTMLBodyValues || g.FetchAllBodyValues
}

func emailGetResponse(snap *emailSnapshot, args json.RawMessage, results map[string]map[string]any) map[string]any {
	var g struct {
		IDs                 []string   `json:"ids"`
		ReferenceIDs        *resultRef `json:"#ids"`
		FetchTextBodyValues bool       `json:"fetchTextBodyValues"`
		FetchHTMLBodyValues bool       `json:"fetchHTMLBodyValues"`
		FetchAllBodyValues  bool       `json:"fetchAllBodyValues"`
	}
	_ = json.Unmarshal(args, &g)
	// Resolve an "#ids" back-reference to an earlier Email/query in this
	// request (RFC 8620 §3.7) — how the client batches every page.
	if len(g.IDs) == 0 && g.ReferenceIDs != nil {
		if prev, ok := results[g.ReferenceIDs.ResultOf]; ok {
			if ids, ok := prev["ids"].([]string); ok {
				g.IDs = ids
			}
		}
	}
	wantBodies := g.FetchTextBodyValues || g.FetchHTMLBodyValues || g.FetchAllBodyValues

	list := []map[string]any{}
	notFound := []string{}
	for _, id := range g.IDs {
		e, ok := snap.lookup(id)
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		list = append(list, emailGetItem(e, wantBodies))
	}
	resp := map[string]any{
		"accountId": "acc1",
		"state":     fmt.Sprintf("e-%d", snap.emailVersion),
		"list":      list,
	}
	if len(notFound) > 0 {
		resp["notFound"] = notFound
	}
	return resp
}

func emailGetItem(e Email, wantBodies bool) map[string]any {
	m := map[string]any{
		"id":            e.ID,
		"blobId":        "blob-" + e.ID,
		"threadId":      e.ThreadID,
		"mailboxIds":    mailboxIDSet(e.MailboxIDs),
		"keywords":      e.Keywords,
		"messageId":     e.MessageID,
		"references":    e.References,
		"inReplyTo":     e.InReplyTo,
		"from":          addrList(e.From),
		"to":            addrList(e.To),
		"cc":            addrList(e.Cc),
		"bcc":           addrList(e.Bcc),
		"replyTo":       addrList(e.ReplyTo),
		"subject":       e.Subject,
		"receivedAt":    e.ReceivedAt.Format(time.RFC3339),
		"size":          e.Size,
		"hasAttachment": e.HasAttachment,
		"preview":       e.Preview,
	}
	if e.TextBody != "" {
		m["textBody"] = []map[string]any{{"partId": "1", "type": "text/plain"}}
	}
	if e.HTMLBody != "" {
		m["htmlBody"] = []map[string]any{{"partId": "2", "type": "text/html"}}
	}
	if len(e.Attachments) > 0 {
		parts := []map[string]any{}
		for i, a := range e.Attachments {
			parts = append(parts, map[string]any{
				"partId":      fmt.Sprintf("3.%d", i+1),
				"blobId":      a.BlobID,
				"name":        a.Name,
				"type":        a.Type,
				"size":        a.Size,
				"disposition": "attachment",
			})
		}
		m["attachments"] = parts
	}
	if wantBodies {
		bv := map[string]any{}
		if e.TextBody != "" {
			bv["1"] = map[string]any{"value": e.TextBody}
		}
		if e.HTMLBody != "" {
			bv["2"] = map[string]any{"value": e.HTMLBody}
		}
		m["bodyValues"] = bv
	}
	return m
}

func mailboxIDSet(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func addrList(addrs []Address) []map[string]any {
	out := []map[string]any{}
	for _, a := range addrs {
		out = append(out, map[string]any{"name": a.Name, "email": a.Email})
	}
	return out
}

// CountIn reports how many fixture emails are in the given mailbox
// (compose assertions on FR-H4/H6 filing).
func (s *Server) CountIn(mailbox string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.emails {
		if contains(e.MailboxIDs, mailbox) {
			n++
		}
	}
	return n
}

// SubjectIn reports how many emails in mailbox carry exactly subject.
func (s *Server) SubjectIn(mailbox, subject string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.emails {
		if e.Subject == subject && contains(e.MailboxIDs, mailbox) {
			n++
		}
	}
	return n
}
