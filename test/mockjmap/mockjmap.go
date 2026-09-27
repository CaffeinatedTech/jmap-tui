// Package mockjmap is an in-process fake JMAP server (httptest) used to test
// protocol and sync behaviour without a network. Server v1 adds the M2
// surface: an EventSource stream with scriptable drops, Email/changes and
// Mailbox/changes backed by a change journal, and fixture mutators that
// journal + broadcast so sync-engine tests can drive live reconciliation.
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

// journalEntry records one fixture mutation for /changes replay: the type,
// the ids touched, and the state version the mutation produced.
type journalEntry struct {
	typ       string
	updated   []string
	destroyed []string
	version   int
}

// streamConn is one live EventSource connection.
type streamConn struct {
	close chan struct{}
	mu    sync.Mutex
	w     http.ResponseWriter
	flush http.Flusher
}

func (sc *streamConn) send(event, data string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	select {
	case <-sc.close:
		return false
	default:
	}
	if _, err := fmt.Fprintf(sc.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	sc.flush.Flush()
	return true
}

// Server is the fake JMAP server.
type Server struct {
	ts       *httptest.Server
	username string
	password string

	mu             sync.Mutex
	mailboxes      []Mailbox
	emails         []Email
	synthetic      *SyntheticMailbox
	blobs          map[string][]byte
	blobOwner      map[string]string // derived message blob id → owning email id
	derivedSeq     uint64            // mints derived (message-scoped) blob ids
	identities     []Identity
	mailboxVersion int
	emailVersion   int
	journal        []journalEntry

	// Contacts (RFC 9610): cards are stored as JSContact wire objects,
	// books as fixtures. Versions feed the same journal/state machinery as
	// mail types.
	books              []AddressBook
	cards              []map[string]any
	contactVersion     int
	addressBookVersion int
	contactSeq         int
	noContacts         bool

	// Test controls (M2 sync-engine suites).
	failStreams       int // reject this many stream connects before accepting
	noChanges         bool
	streams           map[*streamConn]struct{}
	lastMailboxNotify int
	lastEmailNotify   int
	lastContactNotify int
	lastBookNotify    int
	setCalls          int // total Email/set requests served (M3 rate tests)
	createSeq         int // mints ids for Email/set create (M5 drafts)
}

// SetCalls reports how many Email/set requests the server has served
// (batching assertions, FR-K4).
func (s *Server) SetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setCalls
}

// New starts the server and returns it. Close must be called when done.
func New(username, password string, mailboxes []Mailbox) *Server {
	s := &Server{
		username:  username,
		password:  password,
		mailboxes: append([]Mailbox(nil), mailboxes...),
		streams:   map[*streamConn]struct{}{},
		// A default address book exists without asking, exactly like the
		// live Stalwart auto-creates one on first account access.
		books: []AddressBook{{
			ID:           "ab1",
			Name:         "Address Book",
			IsDefault:    true,
			IsSubscribed: true,
		}},
	}
	s.mailboxVersion = 1
	s.emailVersion = 1
	s.contactVersion = 1
	s.addressBookVersion = 1
	s.ts = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Close shuts the server down.
func (s *Server) Close() { s.ts.Close() }

// URL is the server base URL, suitable as jmapclient's ServerURL.
func (s *Server) URL() string { return s.ts.URL }

// SetMailboxes replaces the mailbox fixtures and journals the change.
func (s *Server) SetMailboxes(mailboxes []Mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailboxes = append([]Mailbox(nil), mailboxes...)
	s.mailboxVersion++
	ids := make([]string, 0, len(s.mailboxes))
	for _, mb := range s.mailboxes {
		ids = append(ids, mb.ID)
	}
	s.journal = append(s.journal, journalEntry{typ: "Mailbox", updated: ids, version: s.mailboxVersion})
}

// FailStreams makes the next n EventSource connects fail (FR-B3 tests).
func (s *Server) FailStreams(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failStreams = n
}

// SetCannotCalculateChanges makes /changes answer cannotCalculateChanges
// (PLAN §4.1 case 2 tests).
func (s *Server) SetCannotCalculateChanges(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noChanges = v
}

// Notify broadcasts a StateChange event to every connected stream. Types
// whose state has not moved since the last Notify are omitted.
func (s *Server) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyLocked()
}

func (s *Server) notifyLocked() {
	changed := map[string]string{}
	if s.lastMailboxNotify != s.mailboxVersion {
		changed["Mailbox"] = s.mailboxState()
		s.lastMailboxNotify = s.mailboxVersion
	}
	if s.lastEmailNotify != s.emailVersion {
		changed["Email"] = s.emailState()
		s.lastEmailNotify = s.emailVersion
	}
	if s.lastContactNotify != s.contactVersion {
		changed["ContactCard"] = s.contactState()
		s.lastContactNotify = s.contactVersion
	}
	if s.lastBookNotify != s.addressBookVersion {
		changed["AddressBook"] = s.addressBookState()
		s.lastBookNotify = s.addressBookVersion
	}
	if len(changed) == 0 || len(s.streams) == 0 {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"@type":   "StateChange",
		"changed": map[string]any{"acc1": changed},
	})
	if err != nil {
		return
	}
	for sc := range s.streams {
		if !sc.send("state", string(payload)) {
			s.removeStreamLocked(sc)
		}
	}
}

func (s *Server) mailboxState() string     { return fmt.Sprintf("m-%d", s.mailboxVersion) }
func (s *Server) emailState() string       { return fmt.Sprintf("e-%d", s.emailVersion) }
func (s *Server) contactState() string     { return fmt.Sprintf("c-%d", s.contactVersion) }
func (s *Server) addressBookState() string { return fmt.Sprintf("a-%d", s.addressBookVersion) }

func (s *Server) removeStreamLocked(sc *streamConn) {
	if _, ok := s.streams[sc]; ok {
		delete(s.streams, sc)
		close(sc.close)
	}
}

// DropStreams terminates every connected EventSource stream (kill-the-push
// tests, FR-B3).
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sc := range s.streams {
		s.removeStreamLocked(sc)
	}
}

// StreamCount reports connected EventSource streams (test assertions).
func (s *Server) StreamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/.well-known/jmap" && r.Method == http.MethodGet:
		s.handleSession(w, r)
	case r.URL.Path == "/jmap/api" && r.Method == http.MethodPost:
		s.handleAPI(w, r)
	case strings.HasPrefix(r.URL.Path, "/jmap/event/") && r.Method == http.MethodGet:
		s.handleEvent(w, r)
	case strings.HasPrefix(r.URL.Path, "/jmap/download/") && r.Method == http.MethodGet:
		s.handleDownload(w, r)
	case strings.HasPrefix(r.URL.Path, "/jmap/upload/") && r.Method == http.MethodPost:
		s.handleUpload(w, r)
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
	s.mu.Lock()
	noContacts := s.noContacts
	s.mu.Unlock()

	// RFC 8620 §2: the session resource. Capabilities are raw so the fake
	// needs no knowledge of typed capability structs.
	caps := map[string]any{
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
		// Compose (M5) needs the submission capability or Identity/get
		// is gated off and the client sees no identities (FR-A6).
		"urn:ietf:params:jmap:submission": map[string]any{
			"maxDelayedSend": 0,
		},
	}
	// Contacts (M9, FR-L6) advertise by default; DisableContacts() drops
	// them for gating tests.
	accountCaps := map[string]any{
		"urn:ietf:params:jmap:mail":       map[string]any{},
		"urn:ietf:params:jmap:submission": map[string]any{},
	}
	if !noContacts {
		caps["urn:ietf:params:jmap:contacts"] = map[string]any{}
		accountCaps["urn:ietf:params:jmap:contacts"] = map[string]any{}
	}
	session := map[string]any{
		"capabilities": caps,
		"accounts": map[string]any{
			"acc1": map[string]any{
				"name":                s.username,
				"isPersonal":          true,
				"accountCapabilities": accountCaps,
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

// handleEvent is the RFC 8620 §7.3 EventSource endpoint: it streams `state`
// events on demand and blocks until the connection is dropped.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="jmap"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.failStreams > 0 {
		s.failStreams--
		s.mu.Unlock()
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.mu.Unlock()
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sc := &streamConn{close: make(chan struct{}), w: w, flush: flusher}
	s.streams[sc] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.removeStreamLocked(sc)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": hello\n\n")
	flusher.Flush()

	// Push current state unconditionally so late subscribers reconcile
	// immediately (independent of the delta bookkeeping Notify uses); then
	// block until the server or client closes the stream.
	s.mu.Lock()
	if payload, err := json.Marshal(map[string]any{
		"@type": "StateChange",
		"changed": map[string]any{"acc1": map[string]string{
			"Mailbox":     s.mailboxState(),
			"Email":       s.emailState(),
			"ContactCard": s.contactState(),
			"AddressBook": s.addressBookState(),
		}},
	}); err == nil {
		sc.send("state", string(payload))
	}
	s.mu.Unlock()

	select {
	case <-sc.close:
	case <-r.Context().Done():
	}
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
	snap := &emailSnapshot{
		emails:         append([]Email(nil), s.emails...),
		synthetic:      s.synthetic,
		emailVersion:   s.emailVersion,
		mailboxVersion: s.mailboxVersion,
	}
	s.mu.Unlock()

	resp := &apiResponse{}
	// results indexes each call's response map so later calls in the same
	// request can resolve "#ids"-style back-references (RFC 8620 §3.7).
	results := map[string]map[string]any{}
	for _, call := range req.MethodCalls {
		var args any
		switch call.Name {
		case "Mailbox/query":
			args = mailboxQueryResponse(mbs, snap)
		case "Mailbox/get":
			args = mailboxGetResponse(mbs, snap, call.Args)
		case "Mailbox/changes":
			args = s.changesResponse("Mailbox", call.Args)
		case "Email/query":
			args = emailQueryResponse(snap, call.Args)
		case "Email/get":
			args = emailGetResponse(snap, call.Args, results)
		case "Thread/get":
			args = threadGetResponse(snap, call.Args)
		case "Email/changes":
			args = s.changesResponse("Email", call.Args)
		case "Email/set":
			args = s.emailSetResponse(call.Args)
		case "Identity/get":
			args = s.identityGetResponse()
		case "AddressBook/get", "AddressBook/changes", "ContactCard/get", "ContactCard/changes", "ContactCard/set":
			// A session without the contacts capability must fail these
			// the way a real server does (FR-L6).
			if s.contactsDisabled() {
				resp.add("error", call.CallID, map[string]any{"type": "unknownMethod"})
				continue
			}
			switch call.Name {
			case "AddressBook/get":
				args = s.addressBookGetResponse(call.Args)
			case "AddressBook/changes":
				args = s.changesResponse("AddressBook", call.Args)
			case "ContactCard/get":
				args = s.contactCardGetResponse(call.Args)
			case "ContactCard/changes":
				args = s.changesResponse("ContactCard", call.Args)
			case "ContactCard/set":
				args = s.contactSetResponse(call.Args)
			}
		case "EmailSubmission/set":
			// The implicit Email/set rides the *same* call id as the
			// submission, after it — Stalwart's ordering (PLAN §7).
			out, implicit := s.emailSubmissionSetResponse(call.Args, call.CallID, results)
			resp.add(call.Name, call.CallID, out)
			results[call.CallID] = out
			if implicit != nil {
				resp.add("Email/set", call.CallID, implicit)
			}
			continue
		default:
			args = map[string]any{"type": "unknownMethod"}
			resp.add("error", call.CallID, args)
			continue
		}
		// An error-shaped args map rides under the generic "error" method
		// name so protocol clients decode it as a MethodError (RFC 8620
		// §3.6.1) — how cannotCalculateChanges travels.
		if m, ok := args.(map[string]any); ok && m["type"] == "cannotCalculateChanges" {
			resp.add("error", call.CallID, args)
			continue
		}
		resp.add(call.Name, call.CallID, args)
		if m, ok := args.(map[string]any); ok {
			results[call.CallID] = m
		}
	}
	resp.SessionState = "ses-1"
	writeJSON(w, http.StatusOK, resp)
}

// changesResponse replays the journal for one type between sinceState and
// now, or answers cannotCalculateChanges when the test knob demands it.
func (s *Server) changesResponse(typ string, args json.RawMessage) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req struct {
		SinceState string `json:"sinceState"`
	}
	_ = json.Unmarshal(args, &req)

	current := s.mailboxState()
	switch typ {
	case "Email":
		current = s.emailState()
	case "ContactCard":
		current = s.contactState()
	case "AddressBook":
		current = s.addressBookState()
	}
	if s.noChanges {
		return map[string]any{"type": "cannotCalculateChanges"}
	}

	prefix := strings.ToLower(typ[0:1]) + "-"
	var since int
	if _, err := fmt.Sscanf(req.SinceState, prefix+"%d", &since); err != nil && req.SinceState != current {
		return map[string]any{"type": "cannotCalculateChanges"}
	}
	updated := map[string]bool{}
	destroyed := map[string]bool{}
	for _, e := range s.journal {
		if e.typ != typ || e.version <= since {
			continue
		}
		for _, id := range e.updated {
			if !destroyed[id] {
				updated[id] = true
			}
		}
		for _, id := range e.destroyed {
			delete(updated, id)
			destroyed[id] = true
		}
	}
	ids := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for id := range m {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	return map[string]any{
		"accountId":      "acc1",
		"oldState":       req.SinceState,
		"newState":       current,
		"hasMoreChanges": false,
		"updated":        ids(updated),
		"destroyed":      ids(destroyed),
	}
}

func mailboxQueryResponse(mbs []Mailbox, snap *emailSnapshot) map[string]any {
	sorted := sortedIDs(mbs)
	return map[string]any{
		"accountId":           "acc1",
		"queryState":          fmt.Sprintf("qm-%d", snap.mailboxVersion),
		"canCalculateChanges": true,
		"position":            0,
		"ids":                 sorted,
		"total":               len(sorted),
	}
}

func mailboxGetResponse(mbs []Mailbox, snap *emailSnapshot, args json.RawMessage) map[string]any {
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
		"state":     fmt.Sprintf("m-%d", snap.mailboxVersion),
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
