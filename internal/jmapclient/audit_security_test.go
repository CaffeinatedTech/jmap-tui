package jmapclient

// Audit regression tests for the client-side security fixes: credentials
// confined to the origins the user configured (origin-gated transport,
// refused cross-origin redirects), userinfo stripped before logging,
// size-capped JSON responses and attachment downloads, hostile attachment
// names kept out of the download URL — plus the live hostile-payload
// probe. Every test encodes the secure behavior; a FAIL means the
// regression is back.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
)

// --- helpers ---

type authRecorder struct {
	mu    sync.Mutex
	auths []string
	paths []string
}

func (r *authRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	r.paths = append(r.paths, req.URL.Path)
}

func (r *authRecorder) lastAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auths) == 0 {
		return ""
	}
	return r.auths[len(r.auths)-1]
}

func (r *authRecorder) lastPath() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.paths) == 0 {
		return ""
	}
	return r.paths[len(r.paths)-1]
}

// sessionBody renders a minimal valid RFC 8620 session pointing its apiUrl
// at apiURL, downloadUrl at downloadURL and eventSourceUrl at sseURL
// (empty download/sse URLs are simply omitted).
func sessionBody(apiURL string, extra ...string) []byte {
	downloadURL, sseURL := "", ""
	if len(extra) > 0 {
		downloadURL = extra[0]
	}
	if len(extra) > 1 {
		sseURL = extra[1]
	}
	sess := map[string]any{
		"capabilities": map[string]any{
			"urn:ietf:params:jmap:mail": map[string]any{},
		},
		"accounts": map[string]any{
			"acc1": map[string]any{"name": "tester", "isPersonal": true},
		},
		"primaryAccounts": map[string]any{"urn:ietf:params:jmap:mail": "acc1"},
		"username":        "tester",
		"apiUrl":          apiURL,
		"state":           "ses-1",
	}
	if downloadURL != "" {
		sess["downloadUrl"] = downloadURL
	}
	if sseURL != "" {
		sess["eventSourceUrl"] = sseURL
	}
	b, _ := json.Marshal(sess)
	return b
}

// --- C-1: Authorization must not follow cross-origin redirects ---

func TestAuditC1CrossOriginRedirectDropsAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	}))
	defer target.Close()
	targetRecorder := &authRecorder{}
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRecorder.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	})

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Different origin (different port) → Authorization must not travel.
		http.Redirect(w, r, target.URL+"/redirected", http.StatusFound)
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "sup3cret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Connect(ctx) // may error; what matters is what the target saw

	if got := targetRecorder.lastAuth(); got != "" {
		t.Errorf("FINDING C-1: cross-origin redirect target received Authorization (%q…); credentials must not follow redirects off-origin", truncateForLog(got))
	}
}

// --- C-2: session-supplied URLs on another origin must get no auth ---

func TestAuditC2CrossOriginSessionURLDropsAuth(t *testing.T) {
	apiRecorder := &authRecorder{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRecorder.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{}, "sessionState": "s"})
	}))
	defer api.Close()

	originRecorder := &authRecorder{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originRecorder.record(r)
		_, _ = w.Write(sessionBody(api.URL + "/api"))
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "sup3cret"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.post(ctx, newJSONRequest()); err != nil {
		t.Fatalf("post: %v", err)
	}

	if got := originRecorder.lastAuth(); got == "" {
		t.Fatalf("control broken: same-origin request lost Authorization")
	}
	if got := apiRecorder.lastAuth(); got != "" {
		t.Errorf("FINDING C-2: apiUrl on a different origin received Authorization (%q…); cross-origin session URLs must be fetched without credentials", truncateForLog(got))
	}
}

// --- C-4: URL userinfo must never reach the debug log ---
// (Logging strips userinfo; config rejects userinfo outright so a
// credentialed URL is doubly unreachable.)

func TestAuditC4UserInfoNotLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sessionBody("http://127.0.0.1:1/api"))
	}))
	defer srv.Close()

	// Graft userinfo onto the loopback URL: http://user:SECRET123@127.0.0.1:port
	withUserinfo := strings.Replace(srv.URL, "http://", "http://user:SECRET123@", 1)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	c := New(Options{ServerURL: withUserinfo, Username: "tester", Password: "pw", Logger: logger})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.Connect(ctx)

	if strings.Contains(buf.String(), "SECRET123") {
		t.Errorf("FINDING C-4: debug log contains URL userinfo (password in config URL leaks to the log file); log line: %s", firstLine(&buf))
	}
}

// --- S-2: hostile blob names must not traverse the download path ---

func TestAuditS2DownloadPathStaysInTemplate(t *testing.T) {
	rec := &authRecorder{}
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = w.Write([]byte("data"))
	}))
	defer download.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sessionBody("http://127.0.0.1:1/api", download.URL+"/{accountId}/{blobId}/{name}?type={type}"))
	}))
	defer origin.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	rc, err := c.DownloadBlob(ctx, "blob1", "../../../admin", "text/plain")
	if err == nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if p := rec.lastPath(); p == "" {
		t.Fatal("download never reached the server")
	} else if strings.Contains(p, "..") {
		t.Errorf("FINDING S-2: download path contains dot-segments (%q); PathEscape does not encode '..', a hostile attachment name can walk the server path with credentials attached", p)
	}
}

// --- S-1: redirect loops must fail fast, not hang ---
// (refuseCrossOriginRedirects replaces http.Client's default policy, so
// the 10-hop cap it inherits must be proven.)

func TestAuditS1RedirectLoopFailsFast(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err := c.Connect(ctx)
	if err == nil {
		t.Errorf("redirect loop reported success; want an error")
	}
	if time.Since(start) > 12*time.Second {
		t.Errorf("redirect loop took %v; want fast failure (default 10-hop limit)", time.Since(start))
	}
}

// --- S-3: JSON responses must be size-capped (32 MiB) ---

func TestAuditS3JSONResponseSizeCap(t *testing.T) {
	const cap32MiB = 32 << 20
	// A valid session whose username field alone exceeds the cap.
	huge := strings.Repeat("a", cap32MiB+8<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"username":%q,"capabilities":{"urn:ietf:params:jmap:mail":{}},"accounts":{"acc1":{"name":"t"}},"primaryAccounts":{"urn:ietf:params:jmap:mail":"acc1"},"apiUrl":"http://127.0.0.1:1/api","state":"s"}`, huge)
	}))
	defer srv.Close()

	c := New(Options{ServerURL: srv.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Errorf("FINDING S-3: >32MiB JSON response accepted (no size cap on decode); want a size-limit error (32 MiB cap)")
	} else if !strings.Contains(strings.ToLower(err.Error()), "too large") &&
		!strings.Contains(strings.ToLower(err.Error()), "size") &&
		!strings.Contains(strings.ToLower(err.Error()), "limit") {
		t.Logf("note: response rejected, but error does not name a size limit: %v", err)
	}
}

// --- S-4: attachment download reads must be capped (100 MiB) ---
// (The cap lives in DownloadBlob so both the TUI save
// path and direct callers inherit it.)

func TestAuditS4AttachmentSizeCap(t *testing.T) {
	const cap100MiB = 100 << 20
	const stream = cap100MiB + 1<<20

	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := make([]byte, 1<<20)
		for sent := 0; sent < stream; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer download.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(sessionBody("http://127.0.0.1:1/api", download.URL+"/{accountId}/{blobId}/{name}?type={type}"))
	}))
	defer download.Close()

	c := New(Options{ServerURL: origin.URL, Username: "tester", Password: "pw"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	rc, err := c.DownloadBlob(ctx, "blob1", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer func() { _ = rc.Close() }()

	n, err := io.Copy(io.Discard, rc)
	if err == nil && n > cap100MiB {
		t.Errorf("FINDING S-4: read %d bytes (>%d) with no error; a 100MiB cap must fail the save", n, cap100MiB)
	}
}

// --- small utilities ---

func newJSONRequest() *jmap.Request {
	return &jmap.Request{Using: []jmap.URI{jmap.URI("urn:ietf:params:jmap:mail")}}
}

func truncateForLog(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}

func firstLine(buf *bytes.Buffer) string {
	s := buf.String()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- live probe against Stalwart ---

// Sends ONE hostile-payload message from the test account to itself,
// reads it back, and measures where controls survive: server round-trip →
// HTML conversion → the verbatim text path. Cleans up after itself
// (destroyEmails). Skips without JMAP_TUI_TEST_* creds.

func TestAuditLiveHostilePayloadRoundTrip(t *testing.T) {
	url, user, pass := liveCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c := New(Options{ServerURL: url, Username: user, Password: pass, Timeout: 30 * time.Second})
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	ids, err := c.Identities(ctx)
	if err != nil || len(ids) == 0 {
		t.Fatalf("identities: %v (n=%d)", err, len(ids))
	}
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("mailboxes: %v", err)
	}
	var drafts, sent mail.ID
	for _, mb := range mbs.Mailboxes {
		switch mb.Role {
		case mail.RoleDrafts:
			drafts = mb.ID
		case mail.RoleSent:
			sent = mb.ID
		}
	}
	if drafts == "" || sent == "" {
		t.Fatalf("role mailboxes missing: drafts=%q sent=%q", drafts, sent)
	}

	hostileSubject := "audit \x1b]0;owned\x07 \x1b[2J subject"
	hostileText := "line one\rEVIL REWRITE\x07ding\nline two \x1b]8;;https://evil.test\x07CLICK\x1b]8;;\x07"

	receipt, err := c.Send(ctx, mail.Draft{
		IdentityID:    ids[0].ID,
		MailboxID:     drafts,
		SentMailboxID: sent,
		From:          []mail.Address{{Email: user}},
		To:            []mail.Address{{Email: user}}, // self-send: test address only
		Subject:       hostileSubject,
		Text:          hostileText,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Self-send materializes two Email objects: the Sent copy
	// (receipt.EmailID) and the Inbox delivery. Sweep both at the end.
	defer func() {
		destroyEmails(t, ctx, c, receipt.EmailID)
		var leftovers []mail.ID
		_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{CollapseThreads: false, Limit: 50})
		if err == nil {
			for _, s := range sums {
				if strings.Contains(s.Subject, "audit") && strings.Contains(s.Subject, "subject") {
					leftovers = append(leftovers, s.ID)
				}
			}
		}
		destroyEmails(t, ctx, c, leftovers...)
	}()

	var found mail.EmailSummary
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
			CollapseThreads: false,
			Limit:           5,
		})
		if err == nil {
			for _, s := range sums {
				if s.ID == receipt.EmailID || (strings.Contains(s.Subject, "audit") && strings.Contains(s.Subject, "subject")) {
					found = s
					break
				}
			}
		}
		if found.ID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if found.ID == "" {
		t.Fatal("hostile message never arrived in query results")
	}

	// 1) Server round-trip on the subject: does Stalwart preserve control
	// bytes? If yes, client-side sanitization before rendering is
	// mandatory.
	subjControls := countControls(found.Subject)
	t.Logf("live subject: %d controls, subject=%q", subjControls, found.Subject)
	if subjControls > 0 {
		t.Logf("CONFIRMED T-3 (live): server preserves control bytes in subject → render-boundary sanitization is the only defense")
	} else {
		t.Logf("note: server normalized the subject; client must still defend (other servers will not normalize)")
	}

	// 2) Body round-trip on the verbatim text path (engine.go:976).
	body, err := c.FetchBody(ctx, found.ID)
	if err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	textControls := countControls(body.Text)
	t.Logf("live text body: %d controls", textControls)
	if textControls > 0 {
		t.Logf("CONFIRMED T-1 (live): text/plain body carries %d controls verbatim into the viewport path", textControls)
	}

	// 3) HTML path as the app runs it: conversion must strip controls.
	if body.HTML != "" {
		if n := countControls(mailtext.HTMLToText(body.HTML)); n > 0 {
			t.Errorf("FINDING T-2 (live): HTMLToText emitted %d control bytes from live HTML", n)
		}
	}

	// 4) Preview field (subject-line summary) — same sink as the list row.
	if n := countControls(found.Preview); n > 0 {
		t.Logf("FINDING T-3 (live): preview carries %d controls", n)
	}
}

func countControls(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || r == 0x1b || (r >= 0x80 && r <= 0x9f) {
			n++
		}
	}
	return n
}
