package jmapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// composeFixtures adds the role mailboxes compose needs; fixtures() alone
// has no Drafts or Sent.
func composeFixtures() []mockjmap.Mailbox {
	return append(fixtures(),
		mockjmap.Mailbox{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 5},
		mockjmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 6},
	)
}

// newComposeClient connects a client to a mock with Drafts and Sent.
func newComposeClient(t *testing.T) (*Client, *mockjmap.Server) {
	t.Helper()
	srv := mockjmap.New("tester@example.com", testPassword, composeFixtures())
	t.Cleanup(srv.Close)
	c := New(Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: testPassword})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, srv
}

// draft builds the composer's content for the tests.
func draft(subject, body string) mail.Draft {
	return mail.Draft{
		MailboxID:     "mb-drafts",
		SentMailboxID: "mb-sent",
		IdentityID:    "id-1",
		From:          []mail.Address{{Name: "Tester", Email: "tester@example.com"}},
		To:            []mail.Address{{Email: "eve@example.test"}},
		Cc:            []mail.Address{{Email: "dana@example.test"}},
		Subject:       subject,
		Text:          body,
	}
}

// draftInMailbox reports how many drafts the server still holds.
func draftInMailbox(t *testing.T, srv *mockjmap.Server, mailbox string) int {
	t.Helper()
	return srv.CountIn(mailbox)
}

// TestSaveDraftRecreatesOnEdit proves the RFC-mandated edit path: content
// properties are immutable (RFC 8621 §4.1.2), so a second save returns a
// *new* id and retires the predecessor — the caller must adopt the id the
// save returns.
func TestSaveDraftRecreatesOnEdit(t *testing.T) {
	c, srv := newComposeClient(t)
	ctx := context.Background()

	d := draft("first", "one\n")
	id1, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft create: %v", err)
	}
	if id1 == "" {
		t.Fatal("create returned no id")
	}
	if got := draftInMailbox(t, srv, "mb-drafts"); got != 1 {
		t.Fatalf("drafts after create = %d, want 1", got)
	}

	d.ID = id1
	d.Subject = "second"
	d.Text = "two\n"
	id2, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft edit: %v", err)
	}
	if id2 == id1 {
		t.Errorf("edit reused id %s; content is immutable, it must recreate", id1)
	}
	if got := draftInMailbox(t, srv, "mb-drafts"); got != 1 {
		t.Errorf("drafts after edit = %d, want 1 (predecessor retired)", got)
	}

	// A failed save must hand back the id whose content is still live, so
	// the composer never loses track of its server copy.
	d.ID = id2
	d.MailboxID = ""
	got, err := c.SaveDraft(ctx, d)
	if err == nil {
		t.Fatal("SaveDraft without a drafts mailbox should fail")
	}
	if got != id2 {
		t.Errorf("failed save returned %q, want the live draft %q", got, id2)
	}
	if n := draftInMailbox(t, srv, "mb-drafts"); n != 1 {
		t.Errorf("drafts after failed save = %d, want 1", n)
	}
}

// TestSendFilesDraftIntoSent covers the FR-H5/FR-H6 pipeline: one batched
// request submits the draft and onSuccessUpdateEmail moves it out of
// Drafts into Sent. The mock answers the implicit Email/set with the
// submission's call id (Stalwart's ordering), so a client that indexed
// responses by call id would fail here with "no EmailSubmission created".
func TestSendFilesDraftIntoSent(t *testing.T) {
	c, srv := newComposeClient(t)
	ctx := context.Background()

	rcpt, err := c.Send(ctx, draft("send me", "hello\n"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rcpt.EmailID == "" || rcpt.SubmissionID == "" {
		t.Fatalf("receipt = %+v, want both ids", rcpt)
	}
	if rcpt.UndoStatus != "pending" {
		t.Errorf("UndoStatus = %q, want pending (FR-H5 reports the server window)", rcpt.UndoStatus)
	}
	if got := draftInMailbox(t, srv, "mb-drafts"); got != 0 {
		t.Errorf("draft still in Drafts after send: %d", got)
	}
	if got := draftInMailbox(t, srv, "mb-sent"); got != 1 {
		t.Errorf("sent copies = %d, want 1 (FR-H6)", got)
	}
	if srv.SubjectIn("mb-sent", "send me") != 1 {
		t.Error("the submitted message is not the one filed in Sent")
	}
}

// TestSendExistingDraft submits a draft that was already autosaved: no
// second Email/set, just the submission pointing at the stored id.
func TestSendExistingDraft(t *testing.T) {
	c, srv := newComposeClient(t)
	ctx := context.Background()

	d := draft("autosaved", "body\n")
	id, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	d.ID = id
	rcpt, err := c.Send(ctx, d)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rcpt.EmailID != id {
		t.Errorf("EmailID = %s, want the autosaved draft %s", rcpt.EmailID, id)
	}
	if got := draftInMailbox(t, srv, "mb-sent"); got != 1 {
		t.Errorf("sent copies = %d, want 1", got)
	}
}

// TestSaveDraftKeepsThreadingHeaders proves a reply draft carries
// inReplyTo/references through the recreate path (FR-H2) — immutable
// properties are restated on every create, never patched.
func TestSaveDraftKeepsThreadingHeaders(t *testing.T) {
	c, srv := newComposeClient(t)
	srv.SetEmails([]mockjmap.Email{{
		ID: "e1", ThreadID: "thread-1", MailboxIDs: []string{"mb-inbox"},
		MessageID: []string{"<orig@example.test>"},
		From:      []mockjmap.Address{{Email: "eve@example.test"}},
		Subject:   "topic", ReceivedAt: time.Now().UTC(),
	}})
	ctx := context.Background()

	d := draft("Re: topic", "> quoted\n")
	d.InReplyTo = []string{"<orig@example.test>"}
	d.References = []string{"<orig@example.test>"}

	id1, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	d.ID = id1
	d.Text = "> quoted\n\nreplying\n"
	id2, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft edit: %v", err)
	}
	if id2 == id1 {
		t.Fatal("edit did not recreate")
	}
	sums, err := c.FetchSummaries(ctx, []mail.ID{id2})
	if err != nil || len(sums) != 1 {
		t.Fatalf("FetchSummaries: %v (%d)", err, len(sums))
	}
	if sums[0].ThreadID != "thread-1" {
		t.Errorf("threadId = %q, want thread-1 (reply must stay in the thread)", sums[0].ThreadID)
	}
}

// TestUploadBlobRoundTrip drives FR-H3 end to end against the mock's
// uploadUrl: bytes in, blob id out, bytes back out of downloadUrl.
func TestUploadBlobRoundTrip(t *testing.T) {
	c, _ := newComposeClient(t)
	ctx := context.Background()

	payload := []byte("attachment payload\x00\xff")
	att, err := c.UploadBlob(ctx, "notes.bin", "application/octet-stream",
		int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	if att.BlobID == "" {
		t.Fatal("upload returned no blobId")
	}
	if att.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", att.Size, len(payload))
	}
	if att.Type != "application/octet-stream" {
		t.Errorf("type = %q", att.Type)
	}
	if att.Name != "notes.bin" {
		t.Errorf("name = %q", att.Name)
	}

	rc, err := c.DownloadBlob(ctx, att.BlobID, att.Name, att.Type)
	if err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch: got %d bytes", len(got))
	}
}

// TestSendRejectsMissingConfiguration pins the pre-flight guards so a
// misconfigured account fails with a sentence, not a server error.
func TestSendRejectsMissingConfiguration(t *testing.T) {
	c, _ := newComposeClient(t)
	ctx := context.Background()

	d := draft("x", "y")
	d.IdentityID = ""
	if _, err := c.Send(ctx, d); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Errorf("Send without identity: %v", err)
	}
	d = draft("x", "y")
	d.MailboxID = ""
	if _, err := c.SaveDraft(ctx, d); err == nil || !strings.Contains(err.Error(), "drafts mailbox") {
		t.Errorf("SaveDraft without a drafts mailbox: %v", err)
	}
}

// TestIdentitiesAdvertised pins the capability gate: Identity/get only
// answers when the submission capability is advertised (FR-A6), so the
// mock must declare it or compose would silently see no identities.
func TestIdentitiesAdvertised(t *testing.T) {
	c, _ := newComposeClient(t)
	ids, err := c.Identities(context.Background())
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	if len(ids) != 1 || ids[0].Email != "tester@example.com" {
		t.Errorf("Identities = %+v", ids)
	}
	info, err := c.SessionInfo()
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	if !containsString(info.Capabilities, "urn:ietf:params:jmap:submission") {
		t.Error("session does not advertise the submission capability")
	}
}

// TestDraftCreateEmptyBodyOmitsPart pins the wire shape of a fresh
// composer's autosave: no subject, no body, only a recipient. go-jmap
// tags BodyValue.Value with omitempty, so a "" body would ship a
// bodyValues entry with no "value" member while textBody still advertises
// partId "1" — which a real server answers with
// "invalidProperties: Missing body value for partId".
func TestDraftCreateEmptyBodyOmitsPart(t *testing.T) {
	raw, err := json.Marshal(draftCreate(draft("", "")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wire["textBody"]; ok {
		t.Errorf("empty body still advertises a text part: %s", raw)
	}
	if _, ok := wire["bodyValues"]; ok {
		t.Errorf("empty body still ships bodyValues: %s", raw)
	}

	raw, err = json.Marshal(draftCreate(draft("s", "hello\n")))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wire["textBody"]; !ok {
		t.Errorf("non-empty body lost its text part: %s", raw)
	}
	if !strings.Contains(string(wire["bodyValues"]), `"value":"hello`) {
		t.Errorf("non-empty body lost its value: %s", wire["bodyValues"])
	}
}

// TestSaveDraftEmptyBody is the autosave of a composer the user has not
// typed a subject or body into yet: the create must be accepted, and the
// stored draft must come back with an empty body.
func TestSaveDraftEmptyBody(t *testing.T) {
	c, srv := newComposeClient(t)
	ctx := context.Background()

	d := draft("", "")
	id, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft with no subject/body: %v", err)
	}
	if id == "" {
		t.Fatal("create returned no id")
	}
	if got := draftInMailbox(t, srv, "mb-drafts"); got != 1 {
		t.Fatalf("drafts after create = %d, want 1", got)
	}

	got, err := c.FetchBody(ctx, id)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if got.Text != "" {
		t.Errorf("empty draft came back with body %q", got.Text)
	}
}
