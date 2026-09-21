// Package mockjmap is an in-process fake JMAP server (httptest) used to test
// protocol and sync behaviour without a network. Server v0 implements session
// discovery, HTTP Basic auth, Mailbox/query and Mailbox/get — the surface the
// M0 jmapclient wrapper exercises. Scriptable event injection arrives with
// the sync-engine tests in M2.
package mockjmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

// Mailbox is a fixture mailbox served by the fake.
type Mailbox struct {
	ID           string
	ParentID     string
	Name         string
	Role         string
	SortOrder    uint64
	TotalEmails  uint64
	UnreadEmails uint64
}

// Server is the fake JMAP server.
type Server struct {
	ts       *httptest.Server
	username string
	password string

	mu        sync.Mutex
	mailboxes []Mailbox
}

// New starts the server and returns it. Close must be called when done.
func New(username, password string, mailboxes []Mailbox) *Server {
	s := &Server{
		username:  username,
		password:  password,
		mailboxes: append([]Mailbox(nil), mailboxes...),
	}
	s.ts = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Close shuts the server down.
func (s *Server) Close() { s.ts.Close() }

// URL is the server base URL, suitable as jmapclient's ServerURL.
func (s *Server) URL() string { return s.ts.URL }

// SetMailboxes replaces the mailbox fixtures.
func (s *Server) SetMailboxes(mailboxes []Mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes = append([]Mailbox(nil), mailboxes...)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/jmap" && r.Method == http.MethodGet:
		s.handleSession(w, r)
	case r.URL.Path == "/jmap/api" && r.Method == http.MethodPost:
		s.handleAPI(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	return ok && user == s.username && pass == s.password
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	base := "http://" + r.Host
	// RFC 8620 §2: the session resource. Capabilities are raw so the fake
	// needs no knowledge of typed capability structs.
	session := map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:core": map[string]any{
				"maxSizeUpload":         50000000,
				"maxConcurrentUpload":   4,
				"maxSizeRequest":        10000000,
				"maxConcurrentRequests": 8,
				"maxCallsInRequest":     32,
				"maxObjectsInGet":       100,
				"maxObjectsInSet":       50,
			},
			"urn:ietf:params:jmap:mail": map[string]any{
				"maxMailboxesPerEmail":     100,
				"mayCreateTopLevelMailbox": true,
			},
		},
		"accounts": map[string]any{
			"acc1": map[string]any{
				"name":       s.username,
				"isPersonal": true,
				"accountCapabilities": map[string]any{
					"urn:ietf:params:jmap:mail": map[string]any{},
				},
			},
		},
		"primaryAccounts": map[string]any{"urn:ietf:params:jmap:mail": "acc1"},
		"username":        s.username,
		"apiUrl":          base + "/jmap/api",
		"downloadUrl":     base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":       base + "/jmap/upload/{accountId}",
		"eventSourceUrl":  base + "/jmap/event/{types}/{closeafter}/{ping}",
		"state":           "ses-1",
	}
	writeJSON(w, http.StatusOK, session)
}

// apiRequest is the RFC 8620 RequestBody.
type apiRequest struct {
	Using       []string           `json:"using"`
	MethodCalls []apiInvocationRaw `json:"methodCalls"`
}

// apiInvocationRaw keeps method args raw; each handler decodes what it needs.
type apiInvocationRaw struct {
	Name   string          `json:"-"`
	CallID string          `json:"-"`
	Args   json.RawMessage `json:"-"`
}

func (i *apiInvocationRaw) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("invocation needs 3 elements, got %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &i.Name); err != nil {
		return err
	}
	i.Args = raw[1]
	return json.Unmarshal(raw[2], &i.CallID)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req apiRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"type":   "urn:ietf:params:jmap:error:notRequest",
			"status": 400,
			"detail": "malformed request body",
		})
		return
	}

	s.mu.Lock()
	mbs := append([]Mailbox(nil), s.mailboxes...)
	s.mu.Unlock()

	resp := &apiResponse{}
	for _, call := range req.MethodCalls {
		switch call.Name {
		case "Mailbox/query":
			resp.add(call.Name, call.CallID, mailboxQueryResponse(mbs))
		case "Mailbox/get":
			resp.add(call.Name, call.CallID, mailboxGetResponse(mbs, call.Args))
		default:
			resp.add("error", call.CallID, map[string]any{
				"type": "unknownMethod",
			})
		}
	}
	resp.SessionState = "ses-1"
	writeJSON(w, http.StatusOK, resp)
}

func mailboxQueryResponse(mbs []Mailbox) map[string]any {
	sorted := sortedIDs(mbs)
	return map[string]any{
		"accountId":           "acc1",
		"queryState":          "q-1",
		"canCalculateChanges": true,
		"position":            0,
		"ids":                 sorted,
		"total":               len(sorted),
	}
}

func mailboxGetResponse(mbs []Mailbox, args json.RawMessage) map[string]any {
	var get struct {
		Account string   `json:"accountId"`
		IDs     []string `json:"ids"`
	}
	_ = json.Unmarshal(args, &get)

	list := []map[string]any{}
	for _, mb := range mbs {
		if len(get.IDs) > 0 && !contains(get.IDs, mb.ID) {
			continue
		}
		list = append(list, map[string]any{
			"id":           mb.ID,
			"name":         mb.Name,
			"parentId":     mb.ParentID,
			"role":         mb.Role,
			"sortOrder":    mb.SortOrder,
			"totalEmails":  mb.TotalEmails,
			"unreadEmails": mb.UnreadEmails,
			"myRights": map[string]bool{
				"mayReadItems": true,
			},
			"isSubscribed": true,
		})
	}
	return map[string]any{
		"accountId": "acc1",
		"state":     "m-1",
		"list":      list,
	}
}

func sortedIDs(mbs []Mailbox) []string {
	ids := make([]string, 0, len(mbs))
	byID := make(map[string]Mailbox, len(mbs))
	for _, mb := range mbs {
		ids = append(ids, mb.ID)
		byID[mb.ID] = mb
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := byID[ids[i]], byID[ids[j]]
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return strings.Compare(a.Name, b.Name) < 0
	})
	return ids
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

type apiResponse struct {
	MethodResponses []apiInvocationOut `json:"methodResponses"`
	SessionState    string             `json:"sessionState"`
}

func (r *apiResponse) add(name, callID string, args any) {
	r.MethodResponses = append(r.MethodResponses, apiInvocationOut{name, args, callID})
}

type apiInvocationOut struct {
	Name   string `json:"-"`
	Args   any    `json:"-"`
	CallID string `json:"-"`
}

func (i apiInvocationOut) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{i.Name, i.Args, i.CallID})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
