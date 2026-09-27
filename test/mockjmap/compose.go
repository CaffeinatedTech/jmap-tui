package mockjmap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

// readAll drains a request body; the fake has no size limits of its own
// (the session's maxSizeUpload would enforce them in a real server).
func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// --- identities (FR-H1) ---

// Identity is a fixture sendable identity served by Identity/get.
type Identity struct {
	ID    string
	Name  string
	Email string
}

// SetIdentities replaces the identity fixtures. With none set the fake
// serves a single identity derived from the login, which is what a real
// account looks like (FR-H1).
func (s *Server) SetIdentities(ids []Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identities = append([]Identity(nil), ids...)
}

// identityGetResponse answers Identity/get (RFC 8621 §5.1).
func (s *Server) identityGetResponse() map[string]any {
	s.mu.Lock()
	ids := append([]Identity(nil), s.identities...)
	user := s.username
	s.mu.Unlock()
	if len(ids) == 0 {
		ids = []Identity{{ID: "id-1", Name: user, Email: user}}
	}
	list := []map[string]any{}
	for _, id := range ids {
		list = append(list, map[string]any{
			"id":    id.ID,
			"name":  id.Name,
			"email": id.Email,
		})
	}
	return map[string]any{
		"accountId": "acc1",
		"state":     "id-1",
		"list":      list,
	}
}

// --- blob upload (FR-H3, RFC 8620 §6.1) ---

// blobSeq mints unique blob ids for uploads.
var blobSeq atomic.Uint64

// handleUpload serves POST /jmap/upload/{accountId}, storing the body in
// the blob set the download endpoint reads from.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := fmt.Sprintf("up%08d", blobSeq.Add(1))
	data, err := readAll(r)
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	s.SetBlob(id, data)
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId": "acc1",
		"blobId":    id,
		"type":      r.Header.Get("Content-Type"),
		"size":      len(data),
	})
}

// --- EmailSubmission/set (FR-H5) ---

// emailSubmissionSetArgs is the RFC 8621 §7.5 request as the fake models
// it: creates plus the two onSuccess side-effect maps.
type emailSubmissionSetArgs struct {
	Create                map[string]json.RawMessage `json:"create"`
	OnSuccessUpdateEmail  map[string]map[string]any  `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail []string                   `json:"onSuccessDestroyEmail"`
}

// submissionSetResponse is one EmailSubmission/create result.
type submissionSetResponse struct {
	ID         string `json:"id"`
	EmailID    string `json:"emailId"`
	IdentityID string `json:"identityId"`
	UndoStatus string `json:"undoStatus"`
}

// emailSubmissionSet applies one EmailSubmission/set to the fixtures and
// returns both the submission response and the *implicit* Email/set that
// the onSuccess* maps trigger.
//
// The implicit response deliberately reuses the submission's call id: that
// is what live Stalwart does (PLAN §7, M2 observation), so a client that
// indexes batch responses by call id sees its own submission shadowed and
// fails here instead of in production.
func (s *Server) emailSubmissionSetResponse(args json.RawMessage, callID string, results map[string]map[string]any) (map[string]any, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var req emailSubmissionSetArgs
	_ = json.Unmarshal(args, &req)

	created := map[string]any{}
	notCreated := map[string]setError{}
	oldState := s.emailState()
	updated := []string{}
	destroyed := []string{}

	// resolveRef maps "#handle" to a real id using the results of earlier
	// calls in this same request (RFC 8620 §3.7).
	resolveRef := func(ref string) string {
		if !strings.HasPrefix(ref, "#") {
			return ref
		}
		handle := strings.TrimPrefix(ref, "#")
		for _, res := range results {
			if c, ok := res["created"].(map[string]any); ok {
				if obj, ok := c[handle].(map[string]any); ok {
					if id, ok := obj["id"].(string); ok {
						return id
					}
				}
			}
		}
		return ""
	}

	// --- creates ---
	handleOrder := make([]string, 0, len(req.Create))
	for h := range req.Create {
		handleOrder = append(handleOrder, h)
	}
	// Stable order so responses are deterministic for golden-style asserts.
	sortStrings(handleOrder)

	for _, h := range handleOrder {
		var create struct {
			IdentityID string `json:"identityId"`
			EmailID    string `json:"emailId"`
			ThreadID   string `json:"threadId"`
			SendAt     string `json:"sendAt"`
		}
		_ = json.Unmarshal(req.Create[h], &create)
		if create.IdentityID == "" {
			notCreated[h] = setError{"type": "invalidProperties", "description": "identityId is required"}
			continue
		}
		emailID := resolveRef(create.EmailID)
		if emailID == "" || s.lookupLocked(emailID) == nil {
			notCreated[h] = setError{"type": "notFound", "description": "emailId does not resolve"}
			continue
		}
		id := fmt.Sprintf("sub-%d", blobSeq.Add(1))
		created[h] = submissionSetResponse{
			ID: id, EmailID: emailID, IdentityID: create.IdentityID, UndoStatus: "pending",
		}

		// onSuccess side effects (RFC 8621 §7.5): keyed by the submission's
		// creation reference, each patching the Email that submission names.
		if patch, ok := req.OnSuccessUpdateEmail["#"+h]; ok {
			if e := s.lookupLocked(emailID); e != nil {
				s.applyEmailPatchLocked(e, patch)
				updated = append(updated, emailID)
			}
		}
		for _, ref := range req.OnSuccessDestroyEmail {
			if ref != "#"+h {
				continue
			}
			if idx := s.emailIndexLocked(emailID); idx >= 0 {
				e := s.emails[idx]
				for _, mb := range e.MailboxIDs {
					s.adjustCountsLocked(mb, -1, unreadDelta(&e))
				}
				s.emails = append(s.emails[:idx], s.emails[idx+1:]...)
				s.releaseOwnedBlobsLocked(emailID)
				destroyed = append(destroyed, emailID)
			}
		}
	}

	if len(updated) > 0 || len(destroyed) > 0 {
		s.emailVersion++
		s.journal = append(s.journal, journalEntry{
			typ: "Email", updated: updated, destroyed: destroyed, version: s.emailVersion,
		})
	}

	implicit := buildSetResponse(s, oldState, nil, updated, destroyed, nil, nil, nil)
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
	if len(updated) == 0 && len(destroyed) == 0 {
		// Nothing for the implicit call to do; omit it so the response
		// list matches what a real server emits.
		return out, nil
	}
	return out, implicit
}

// --- fixture helpers shared by the compose surface ---

// lookupLocked returns the fixture with id, or nil.
func (s *Server) lookupLocked(id string) *Email {
	for i := range s.emails {
		if s.emails[i].ID == id {
			return &s.emails[i]
		}
	}
	return nil
}

// emailIndexLocked returns the fixture index for id, or -1.
func (s *Server) emailIndexLocked(id string) int {
	for i := range s.emails {
		if s.emails[i].ID == id {
			return i
		}
	}
	return -1
}

// unreadDelta reports how many unread messages a fixture contributes to its
// mailboxes' counts (FR-B6 realism).
func unreadDelta(e *Email) int {
	if e.Keywords["$seen"] {
		return 0
	}
	return 1
}

// adjustCountsLocked moves a mailbox's total/unread counters.
func (s *Server) adjustCountsLocked(mb string, total, unread int) {
	for i := range s.mailboxes {
		if s.mailboxes[i].ID != mb {
			continue
		}
		s.mailboxes[i].TotalEmails = uint64(max(int(s.mailboxes[i].TotalEmails)+total, 0))
		s.mailboxes[i].UnreadEmails = uint64(max(int(s.mailboxes[i].UnreadEmails)+unread, 0))
		return
	}
}

// applyEmailPatchLocked applies an RFC 8620 §5.3 PatchObject to a fixture:
// JSON-pointer paths with an implicit leading "/", so "mailboxIds/<id>"
// adds/removes membership and "keywords/<kw>" flips a keyword.
func (s *Server) applyEmailPatchLocked(e *Email, patch map[string]any) {
	for key, val := range patch {
		switch {
		case key == "mailboxIds" || strings.HasPrefix(key, "mailboxIds/"):
			mb := strings.TrimPrefix(key, "mailboxIds/")
			if mb == "" {
				continue
			}
			set, _ := val.(bool)
			if set {
				if !contains(e.MailboxIDs, mb) {
					e.MailboxIDs = append(e.MailboxIDs, mb)
				}
				continue
			}
			kept := e.MailboxIDs[:0]
			for _, cur := range e.MailboxIDs {
				if cur != mb {
					kept = append(kept, cur)
				}
			}
			e.MailboxIDs = kept
		case key == "keywords" || strings.HasPrefix(key, "keywords/"):
			kw := strings.TrimPrefix(key, "keywords/")
			if kw == "" {
				continue
			}
			if b, ok := val.(bool); ok && b {
				if e.Keywords == nil {
					e.Keywords = map[string]bool{}
				}
				e.Keywords[kw] = true
			} else {
				delete(e.Keywords, kw)
			}
		}
	}
}

// sortStrings is a tiny insertion sort: the fixture lists here are short
// and pulling in "sort" for five lines is not worth it.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
