package mockjmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// --- Email/set (M3 triage surface) ---

// emailSetArgs is the RFC 8620 §5.3 Email/set request as the fake models it:
// patches stay raw so keyword and mailbox deltas decode per key.
type emailSetArgs struct {
	Create  map[string]json.RawMessage `json:"create"`
	Update  map[string]map[string]any  `json:"update"`
	Destroy []string                   `json:"destroy"`
}

// createdEmail is the RFC 8621 §4.6 create object: content properties only
// — the fake mirrors the spec's immutability by accepting them solely here.
type createdEmail struct {
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	MessageID  []string        `json:"messageId"`
	InReplyTo  []string        `json:"inReplyTo"`
	References []string        `json:"references"`
	From       []Address       `json:"from"`
	To         []Address       `json:"to"`
	Cc         []Address       `json:"cc"`
	Bcc        []Address       `json:"bcc"`
	ReplyTo    []Address       `json:"replyTo"`
	Subject    string          `json:"subject"`
	ReceivedAt *time.Time      `json:"receivedAt"`
	TextBody   []partRef       `json:"textBody"`
	HTMLBody   []partRef       `json:"htmlBody"`
	BodyValues map[string]struct {
		Value string `json:"value"`
	} `json:"bodyValues"`
	Attachments []struct {
		BlobID      string `json:"blobId"`
		Name        string `json:"name"`
		Type        string `json:"type"`
		Size        uint64 `json:"size"`
		Disposition string `json:"disposition"`
	} `json:"attachments"`
}

// partRef is the subset of EmailBodyPart a create needs.
type partRef struct {
	PartID string `json:"partId"`
	Type   string `json:"type"`
}

// setError is the RFC 8620 SetError object.
type setError = map[string]string

// buildSetResponse assembles the Email/set response map with the exact wire
// keys go-jmap decodes.
func buildSetResponse(s *Server, oldState string, created map[string]any, updated, destroyed []string, notCreated, notUpdated, notDestroyed map[string]setError) map[string]any {
	out := map[string]any{
		"accountId": "acc1",
		"oldState":  oldState,
		"newState":  s.emailState(),
	}
	if len(created) > 0 {
		out["created"] = created
	}
	if len(notCreated) > 0 {
		out["notCreated"] = notCreated
	}
	if len(updated) > 0 {
		out["updated"] = updated
	}
	if len(destroyed) > 0 {
		out["destroyed"] = destroyed
	}
	if len(notUpdated) > 0 {
		out["notUpdated"] = notUpdated
	}
	if len(notDestroyed) > 0 {
		out["notDestroyed"] = notDestroyed
	}
	return out
}

// emailSetResponse applies one Email/set to the fixtures under the server
// lock, journals the delta (Email and touched Mailboxes), and returns the
// response map. This is the M3 triage surface: keyword flips, mailbox
// add/remove (move/copy), and destroy.
func (s *Server) emailSetResponse(args json.RawMessage) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls++

	var req emailSetArgs
	_ = json.Unmarshal(args, &req)

	oldState := s.emailState()
	created := map[string]any{}
	notCreated := map[string]setError{}
	notUpdated := map[string]setError{}
	notDestroyed := map[string]setError{}
	updated := []string{}
	destroyed := []string{}

	// countDelta accumulates per-mailbox total/unread changes so fixture
	// counts stay honest after creates, moves, and destroys (FR-B6 realism).
	type delta struct{ total, unread int }
	countDelta := map[string]*delta{}
	deltas := func(mbs []string) []*delta {
		var out []*delta
		for _, mb := range mbs {
			d, ok := countDelta[mb]
			if !ok {
				d = &delta{}
				countDelta[mb] = d
			}
			out = append(out, d)
		}
		return out
	}

	// --- creates (M5 drafts): Email content is immutable in RFC 8621
	// §4.1.2, so this is the only way a message's content ever gets
	// written. The fake accepts content keys here and nowhere else.
	for handle, raw := range req.Create {
		var ce createdEmail
		if err := json.Unmarshal(raw, &ce); err != nil {
			notCreated[handle] = setError{"type": "invalidProperties", "description": err.Error()}
			continue
		}
		if desc := validateBodyValues(raw); desc != "" {
			notCreated[handle] = setError{"type": "invalidProperties", "description": desc}
			continue
		}
		e, mbs := s.materialiseCreateLocked(handle, ce)
		s.createSeq++
		e.ID = fmt.Sprintf("cr%06d", s.createSeq)
		if ce.InReplyTo != nil && e.ThreadID == "" {
			// Thread a reply into the original's thread the way a server
			// does: match on the replied-to Message-ID.
			if orig := s.byMessageIDLocked(ce.InReplyTo[0]); orig != nil {
				e.ThreadID = orig.ThreadID
			}
		}
		if e.ThreadID == "" {
			e.ThreadID = "th-" + e.ID
		}
		// Draft blob ids are message-scoped (Stalwart): validate the
		// source blob and re-register the bytes under ids owned by this
		// message, so destroying the message releases them and a later
		// reference answers blobNotFound — the semantics the composer's
		// post-save refresh exists for.
		if err := s.deriveAttachmentBlobsLocked(e); err != nil {
			notCreated[handle] = setError{"type": "blobNotFound", "description": err.Error()}
			continue
		}
		s.emails = append(s.emails, *e)
		for _, d := range deltas(mbs) {
			d.total++
			if !e.Keywords["$seen"] {
				d.unread++
			}
		}
		created[handle] = map[string]any{
			"id":       e.ID,
			"blobId":   "blob-" + e.ID,
			"threadId": e.ThreadID,
			"size":     e.Size,
		}
		updated = append(updated, e.ID)
	}
	sort.Strings(updated)

	// --- updates ---
	for id, patch := range req.Update {
		idx := -1
		for i := range s.emails {
			if s.emails[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			notUpdated[id] = setError{"type": "notFound"}
			continue
		}
		e := &s.emails[idx]
		beforeMbs := append([]string(nil), e.MailboxIDs...)
		beforeUnread := !e.Keywords["$seen"]

		for key, val := range patch {
			switch {
			case key == "keywords" || strings.HasPrefix(key, "keywords/"):
				kw := strings.TrimPrefix(key, "keywords/")
				if kw == "" {
					continue
				}
				set, ok := val.(bool)
				if !ok || !set { // JSON null or false removes
					delete(e.Keywords, kw)
				} else {
					if e.Keywords == nil {
						e.Keywords = map[string]bool{}
					}
					e.Keywords[kw] = true
				}
			case key == "mailboxIds" || strings.HasPrefix(key, "mailboxIds/"):
				mb := strings.TrimPrefix(key, "mailboxIds/")
				if mb == "" {
					continue
				}
				set, ok := val.(bool)
				if ok && set {
					if !contains(e.MailboxIDs, mb) {
						e.MailboxIDs = append(e.MailboxIDs, mb)
					}
				} else { // false or null removes
					kept := e.MailboxIDs[:0]
					for _, cur := range e.MailboxIDs {
						if cur != mb {
							kept = append(kept, cur)
						}
					}
					e.MailboxIDs = kept
				}
			}
		}

		// Mailbox-count deltas from this update.
		afterMbs := e.MailboxIDs
		for _, mb := range beforeMbs {
			if !contains(afterMbs, mb) {
				for _, d := range deltas([]string{mb}) {
					d.total--
					if beforeUnread {
						d.unread--
					}
				}
			}
		}
		for _, mb := range afterMbs {
			if !contains(beforeMbs, mb) {
				for _, d := range deltas([]string{mb}) {
					d.total++
					if !e.Keywords["$seen"] {
						d.unread++
					}
				}
			}
		}
		updated = append(updated, id)
	}
	sort.Strings(updated)

	// --- destroys ---
	for _, id := range req.Destroy {
		idx := -1
		for i := range s.emails {
			if s.emails[i].ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			notDestroyed[id] = setError{"type": "notFound"}
			continue
		}
		e := s.emails[idx]
		for _, mb := range e.MailboxIDs {
			for _, d := range deltas([]string{mb}) {
				d.total--
				if !e.Keywords["$seen"] {
					d.unread--
				}
			}
		}
		s.emails = append(s.emails[:idx], s.emails[idx+1:]...)
		s.releaseOwnedBlobsLocked(id)
		destroyed = append(destroyed, id)
	}
	sort.Strings(destroyed)

	if len(created) > 0 || len(updated) > 0 || len(destroyed) > 0 {
		s.emailVersion++
		s.journal = append(s.journal, journalEntry{
			typ:       "Email",
			updated:   updated,
			destroyed: destroyed,
			version:   s.emailVersion,
		})
	}

	// Fold count deltas into fixture mailboxes and journal them so clients
	// reconcile counts through the normal Mailbox/changes path.
	var touchedMbs []string
	for mb, d := range countDelta {
		for i := range s.mailboxes {
			if s.mailboxes[i].ID != mb {
				continue
			}
			s.mailboxes[i].TotalEmails = uint64(max(int(s.mailboxes[i].TotalEmails)+d.total, 0))
			s.mailboxes[i].UnreadEmails = uint64(max(int(s.mailboxes[i].UnreadEmails)+d.unread, 0))
			touchedMbs = append(touchedMbs, mb)
			break
		}
	}
	if len(touchedMbs) > 0 {
		sort.Strings(touchedMbs)
		s.mailboxVersion++
		s.journal = append(s.journal, journalEntry{
			typ:     "Mailbox",
			updated: touchedMbs,
			version: s.mailboxVersion,
		})
	}

	return buildSetResponse(s, oldState, created, updated, destroyed, notCreated, notUpdated, notDestroyed)
}

// validateBodyValues enforces the create-side body rule the way a real
// server does (Stalwart crates/jmap/src/email/set.rs): every partId named
// in textBody/htmlBody must resolve to a bodyValues entry that carries a
// "value" member — an empty string is fine, a *missing* one is not, since
// the parser drops the whole map and then fails the lookup. Checked on the
// raw JSON because the decoded form cannot tell "" from absent. It returns
// the SetError description verbatim (empty when valid), so the fake's
// rejection reads exactly like the server's.
func validateBodyValues(raw json.RawMessage) string {
	var probe struct {
		TextBody   []partRef                  `json:"textBody"`
		HTMLBody   []partRef                  `json:"htmlBody"`
		BodyValues map[string]json.RawMessage `json:"bodyValues"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err.Error()
	}
	check := func(parts []partRef) string {
		for _, p := range parts {
			if p.PartID == "" { // blob-backed attachment part
				continue
			}
			bv, ok := probe.BodyValues[p.PartID]
			if !ok {
				return fmt.Sprintf("Missing body value for partId %q", p.PartID)
			}
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(bv, &entry); err != nil {
				return err.Error()
			}
			if _, ok := entry["value"]; !ok {
				return fmt.Sprintf("Missing body value for partId %q", p.PartID)
			}
		}
		return ""
	}
	if desc := check(probe.TextBody); desc != "" {
		return desc
	}
	return check(probe.HTMLBody)
}

// materialiseCreateLocked turns a create object into a fixture plus the
// mailboxes it lands in, deriving the body, preview, and size the way a
// server does when it renders the message (RFC 8621 §4.6).
func (s *Server) materialiseCreateLocked(handle string, ce createdEmail) (*Email, []string) {
	e := &Email{
		Keywords:   map[string]bool{},
		MessageID:  ce.MessageID,
		InReplyTo:  ce.InReplyTo,
		References: ce.References,
		From:       ce.From,
		To:         ce.To,
		Cc:         ce.Cc,
		Bcc:        ce.Bcc,
		ReplyTo:    ce.ReplyTo,
		Subject:    ce.Subject,
		ReceivedAt: time.Now().UTC(),
	}
	if ce.ReceivedAt != nil {
		e.ReceivedAt = ce.ReceivedAt.UTC()
	}
	for mb := range ce.MailboxIDs {
		e.MailboxIDs = append(e.MailboxIDs, mb)
	}
	sort.Strings(e.MailboxIDs)
	for kw, on := range ce.Keywords {
		if on {
			e.Keywords[kw] = true
		}
	}
	if e.MessageID == nil {
		e.MessageID = []string{fmt.Sprintf("<%s@mock.jmap>", handle)}
	}

	// Body: the first text/plain part's value wins, exactly like the
	// client's FR-E2 preference order.
	for _, p := range ce.TextBody {
		if p.Type != "text/plain" {
			continue
		}
		if bv, ok := ce.BodyValues[p.PartID]; ok {
			e.TextBody = bv.Value
			break
		}
	}
	if e.TextBody == "" {
		for _, bv := range ce.BodyValues {
			e.TextBody = bv.Value
			break
		}
	}
	for _, p := range ce.HTMLBody {
		if p.Type == "text/html" {
			if bv, ok := ce.BodyValues[p.PartID]; ok {
				e.HTMLBody = bv.Value
			}
			break
		}
	}
	for _, a := range ce.Attachments {
		if a.Disposition != "" && a.Disposition != "attachment" {
			continue
		}
		e.Attachments = append(e.Attachments, Attachment{
			BlobID: a.BlobID, Name: a.Name, Type: a.Type, Size: a.Size,
		})
	}
	e.HasAttachment = len(e.Attachments) > 0
	e.Size = uint64(len(e.TextBody) + len(e.HTMLBody))
	e.Preview = previewOf(e.TextBody, e.HTMLBody, e.Subject)
	return e, e.MailboxIDs
}

// byMessageIDLocked finds the fixture carrying the given Message-ID.
func (s *Server) byMessageIDLocked(msgID string) *Email {
	for i := range s.emails {
		for _, m := range s.emails[i].MessageID {
			if m == msgID {
				return &s.emails[i]
			}
		}
	}
	return nil
}

// previewOf derives the one-line preview Email/get reports (FR-D1).
func previewOf(text, html, subject string) string {
	src := text
	if src == "" {
		src = html
	}
	src = strings.Join(strings.Fields(src), " ")
	if src == "" {
		return subject
	}
	if len(src) > 120 {
		return src[:120]
	}
	return src
}

// --- blob download (FR-E4) ---

// SetBlob registers blob bytes for the download endpoint. Fixture
// attachments reference the registered blob id.
func (s *Server) SetBlob(id string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobs == nil {
		s.blobs = map[string][]byte{}
	}
	s.blobs[id] = append([]byte(nil), data...)
}

// deriveAttachmentBlobsLocked validates every one of e's attachment blob
// ids against the blob set and re-registers the bytes under fresh ids
// owned by e — real servers scope message blobs to their message
// (Stalwart derives new ids at write and frees them with the message), so
// a replaced message's old ids answer blobNotFound. Caller holds s.mu.
func (s *Server) deriveAttachmentBlobsLocked(e *Email) error {
	for i := range e.Attachments {
		src := e.Attachments[i].BlobID
		data, ok := s.blobs[src]
		if !ok {
			return fmt.Errorf("blobId %s does not exist on this server", src)
		}
		s.derivedSeq++
		derived := fmt.Sprintf("dm%08d", s.derivedSeq)
		if s.blobOwner == nil {
			s.blobOwner = map[string]string{}
		}
		s.blobs[derived] = data
		s.blobOwner[derived] = e.ID
		e.Attachments[i].BlobID = derived
	}
	return nil
}

// releaseOwnedBlobsLocked frees the derived blob ids owned by emailID —
// the message-scoped GC that makes a stale reference fail. Caller holds
// s.mu.
func (s *Server) releaseOwnedBlobsLocked(emailID string) {
	for bid, owner := range s.blobOwner {
		if owner == emailID {
			delete(s.blobOwner, bid)
			delete(s.blobs, bid)
		}
	}
}

// handleDownload serves /jmap/download/{accountId}/{blobId}/{name} from the
// registered blob set (RFC 8620 §2 downloadUrl template).
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Trim the prefix, then parse accountId/blobId/name (name may contain
	// any characters and there may be a ?type= query).
	rest := strings.TrimPrefix(r.URL.Path, "/jmap/download/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	data, ok := s.blobs[parts[1]]
	s.mu.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("unknown blob %q", parts[1]), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
