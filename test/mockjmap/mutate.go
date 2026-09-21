package mockjmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// --- Email/set (M3 triage surface) ---

// emailSetArgs is the RFC 8620 §5.3 Email/set request as the fake models it:
// patches stay raw so keyword and mailbox deltas decode per key.
type emailSetArgs struct {
	Update  map[string]map[string]any `json:"update"`
	Destroy []string                  `json:"destroy"`
}

// setError is the RFC 8620 SetError object.
type setError = map[string]string

// buildSetResponse assembles the Email/set response map with the exact wire
// keys go-jmap decodes.
func buildSetResponse(s *Server, oldState string, updated, destroyed []string, notUpdated, notDestroyed map[string]setError) map[string]any {
	out := map[string]any{
		"accountId": "acc1",
		"oldState":  oldState,
		"newState":  s.emailState(),
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
	notUpdated := map[string]setError{}
	notDestroyed := map[string]setError{}
	updated := []string{}
	destroyed := []string{}

	// countDelta accumulates per-mailbox total/unread changes so fixture
	// counts stay honest after moves and destroys (FR-B6 realism).
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
		destroyed = append(destroyed, id)
	}
	sort.Strings(destroyed)

	if len(updated) > 0 || len(destroyed) > 0 {
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

	return buildSetResponse(s, oldState, updated, destroyed, notUpdated, notDestroyed)
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
