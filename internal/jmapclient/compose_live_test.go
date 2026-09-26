package jmapclient

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// composeLiveFixture resolves the roles, identity, and one replyable
// message the M5 protocol probes need. Everything outside role mailboxes
// is read-only (AGENTS.md test-account rules).
type composeLiveFixture struct {
	account  string
	drafts   mail.ID
	sent     mail.ID
	inbox    mail.ID
	identity mail.ID

	origID     mail.ID
	origMsgID  []string
	origRefs   []string
	origThread mail.ID
	origSubj   string
	origFrom   *jmapmail.Address
}

// loadComposeFixture connects, resolves role mailboxes and the identity,
// and picks the newest inbox message carrying a Message-ID to reply to.
func loadComposeFixture(t *testing.T, ctx context.Context, c *Client) *composeLiveFixture {
	t.Helper()
	f := &composeLiveFixture{account: c.accountID}

	ids, err := c.Identities(ctx)
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("no identities on the test account")
	}
	f.identity = ids[0].ID

	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	for _, mb := range mbs.Mailboxes {
		switch mb.Role {
		case mail.RoleDrafts:
			f.drafts = mb.ID
		case mail.RoleSent:
			f.sent = mb.ID
		case mail.RoleInbox:
			f.inbox = mb.ID
		}
	}
	if f.drafts == "" || f.sent == "" || f.inbox == "" {
		t.Fatalf("role mailboxes missing: drafts=%q sent=%q inbox=%q", f.drafts, f.sent, f.inbox)
	}

	_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
		MailboxID:       f.inbox,
		CollapseThreads: false,
		Limit:           5,
	})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	if len(sums) == 0 {
		t.Skip("inbox is empty; nothing to reply to")
	}
	f.origID = sums[0].ID
	f.origSubj = sums[0].Subject
	if len(sums[0].From) > 0 {
		f.origFrom = mkAddr(sums[0].From[0].Name, sums[0].From[0].Email)
	}

	body, err := c.FetchBody(ctx, f.origID)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	f.origMsgID = body.MessageID
	f.origRefs = body.References
	f.origThread = body.ThreadID
	if len(f.origMsgID) == 0 {
		t.Skipf("message %s carries no Message-ID; cannot probe reply threading", f.origID)
	}
	t.Logf("original %s thread=%s messageId=%v references=%v", f.origID, f.origThread, f.origMsgID, f.origRefs)
	return f
}

// destroyEmails is cleanup: every probe artifact leaves the server.
func destroyEmails(t *testing.T, ctx context.Context, c *Client, ids ...mail.ID) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&email.Set{Account: jmap.ID(c.accountID), Destroy: jmapIDs(ids)})
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Logf("cleanup: Email/set destroy: %v", err)
		return
	}
	if inv, ok := invs[call]; ok {
		view, ok := asEmailSetView(inv)
		if !ok {
			return
		}
		// SaveDraft retires a draft's predecessor itself; re-destroying an
		// id that is already gone is expected, not a leak.
		for id, se := range view.NotDestroyed {
			if se != nil && se.Type != "notFound" {
				t.Logf("cleanup: %s not destroyed: %s %s", id, se.Type, seDesc(se.Description))
			}
		}
	}
}

// fetchEmail reads one Email's raw properties for assertions.
func fetchEmail(t *testing.T, ctx context.Context, c *Client, id mail.ID, props ...string) *email.Email {
	t.Helper()
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&email.Get{
		Account:    jmap.ID(c.accountID),
		IDs:        []jmap.ID{jmap.ID(id)},
		Properties: props,
	})
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("Email/get: %v", err)
	}
	inv, ok := invs[call]
	if !ok {
		t.Fatal("no Email/get response")
	}
	gr, ok := inv.Args.(*email.GetResponse)
	if !ok {
		t.Fatalf("unexpected Email/get response %T", inv.Args)
	}
	if len(gr.List) == 0 {
		t.Fatalf("Email/get returned no object for %s", id)
	}
	return gr.List[0]
}

// mailboxList decodes an Email's mailbox id set.
func mailboxList(e *email.Email) []string {
	out := make([]string, 0, len(e.MailboxIDs))
	for id, on := range e.MailboxIDs {
		if on {
			out = append(out, string(id))
		}
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, s := range ids {
		if s == want {
			return true
		}
	}
	return false
}

// TestLiveM5ProbeReplyThreading verifies the RFC 8621 reply mechanism
// against live Stalwart: the immutable `inReplyTo`/`references` properties
// (settable on create) must persist and thread the draft into the original
// thread. REQUIREMENTS FR-H2's `inReplyToEmailId` does not exist in the
// RFC — this probe confirms its replacement. Artifacts are destroyed.
func TestLiveM5ProbeReplyThreading(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	now := time.Now().UTC()
	refs := append(append([]string{}, f.origRefs...), f.origMsgID...)
	req := &jmap.Request{Context: ctx}
	call := req.Invoke(&email.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*email.Email{
			"draft": {
				MailboxIDs: map[jmap.ID]bool{jmap.ID(f.drafts): true},
				From:       []*jmapmail.Address{mkAddr("", user)},
				To:         []*jmapmail.Address{f.origFrom},
				Subject:    "Re: " + f.origSubj,
				Keywords:   map[string]bool{"$draft": true, "$seen": true},
				InReplyTo:  f.origMsgID,
				References: refs,
				TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
				BodyValues: map[string]*email.BodyValue{
					"1": {Value: "probe reply body\n"},
				},
				ReceivedAt: &now,
			},
		},
	})
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("Email/set create: %v", err)
	}
	view, ok := asEmailSetView(invs[call])
	if !ok {
		t.Fatalf("unexpected Email/set response %T", invs[call].Args)
	}
	if len(view.NotCreated) > 0 {
		for cid, se := range view.NotCreated {
			t.Fatalf("draft not created: %s %s %s", cid, se.Type, seDesc(se.Description))
		}
	}
	draftID, ok := view.CreatedIDs["draft"]
	if !ok {
		t.Fatal("draft id missing from create response")
	}
	defer destroyEmails(t, ctx, c, draftID)

	got := fetchEmail(t, ctx, c, draftID, "id", "threadId", "inReplyTo", "references", "mailboxIds", "keywords")
	t.Logf("created draft thread=%s inReplyTo=%v references=%v mailboxes=%v",
		got.ThreadID, got.InReplyTo, got.References, mailboxList(got))

	if len(got.InReplyTo) == 0 || got.InReplyTo[0] != f.origMsgID[0] {
		t.Errorf("inReplyTo not persisted: got %v, want %v", got.InReplyTo, f.origMsgID)
	}
	if string(got.ThreadID) != string(f.origThread) {
		t.Errorf("reply did not thread: threadId=%s, want %s", got.ThreadID, f.origThread)
	}
	if !hasID(mailboxList(got), string(f.drafts)) {
		t.Errorf("draft not in drafts mailbox: %v", mailboxList(got))
	}
}

// TestLiveM5ProbeSendToSent answers the two questions the send pipeline
// hinges on: (1) does `onSuccessUpdateEmail` move the submitted draft into
// Sent without the server also filing its own copy (a duplicate), and (2)
// does the submission report an undo window? One self-addressed send,
// fully cleaned up (AGENTS.md rules).
func TestLiveM5ProbeSendToSent(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	now := time.Now().UTC()
	subject := fmt.Sprintf("M5 send probe %d", now.UnixNano())
	req := &jmap.Request{Context: ctx}
	c1 := req.Invoke(&email.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*email.Email{
			"draft": {
				MailboxIDs: map[jmap.ID]bool{jmap.ID(f.drafts): true},
				From:       []*jmapmail.Address{mkAddr("jmap-tui M5 probe", user)},
				To:         []*jmapmail.Address{mkAddr("jmap-tui M5 probe", user)},
				Subject:    subject,
				Keywords:   map[string]bool{"$draft": true, "$seen": true},
				TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
				BodyValues: map[string]*email.BodyValue{
					"1": {Value: "M5 send probe — safe to ignore, auto-cleaned.\n"},
				},
				ReceivedAt: &now,
			},
		},
	})
	c2 := req.Invoke(&emailsubmission.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			"sub": {IdentityID: jmap.ID(f.identity), EmailID: "#draft"},
		},
		// RFC 8621 §7.5: the map is keyed by *EmailSubmission* id (its
		// creation reference), and each value patches the Email that the
		// submission references — patch form, so only Drafts membership
		// and the $draft keyword change.
		OnSuccessUpdateEmail: map[jmap.ID]jmap.Patch{
			"#sub": {
				"mailboxIds/" + string(f.drafts): nil,
				"mailboxIds/" + string(f.sent):   true,
				"keywords/$draft":                nil,
			},
		},
	})
	resp, err := c.post(ctx, req)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}

	var draftID mail.ID
	var undoStatus string
	for _, inv := range resp.Responses {
		switch {
		case inv.Name == "error":
			me, ok := inv.Args.(*jmap.MethodError)
			if !ok {
				t.Fatalf("batch error invocation with args %T", inv.Args)
			}
			t.Fatalf("batch error: %s %s", me.Type, deref(me.Description))
		case inv.Name == "Email/set" && inv.CallID == c1:
			v, ok := asEmailSetView(inv)
			if !ok {
				t.Fatalf("unexpected Email/set response %T", inv.Args)
			}
			draftID = v.CreatedIDs["draft"]
		case inv.Name == "EmailSubmission/set" && inv.CallID == c2:
			ur, ok := inv.Args.(*emailsubmission.SetResponse)
			if !ok {
				t.Fatalf("unexpected EmailSubmission/set response %T", inv.Args)
			}
			for cid, se := range ur.NotCreated {
				t.Fatalf("submission %s failed: %s %s", cid, se.Type, seDesc(se.Description))
			}
			for _, s := range ur.Created {
				if s != nil {
					undoStatus = s.UndoStatus
				}
			}
		}
	}
	if draftID == "" {
		t.Fatal("draft id never resolved")
	}
	t.Logf("submission undoStatus=%q (empty = field not reported)", undoStatus)

	// Let the server settle, then inspect where the message actually lives.
	time.Sleep(2 * time.Second)
	defer destroyEmails(t, ctx, c, draftID)

	got := fetchEmail(t, ctx, c, draftID, "id", "mailboxIds", "keywords", "subject")
	mbs := mailboxList(got)
	t.Logf("submitted email %s lives in %v", draftID, mbs)
	if !hasID(mbs, string(f.sent)) {
		t.Errorf("onSuccessUpdateEmail did not move the message to Sent: %v", mbs)
	}
	if hasID(mbs, string(f.drafts)) {
		t.Errorf("message still in Drafts after submit: %v", mbs)
	}

	copies := subjectMatches(t, ctx, c, f.sent, subject)
	t.Logf("Sent copies matching the probe subject: %d %v", len(copies), copies)
	if len(copies) > 1 {
		t.Errorf("server filed %d copies into Sent; the client would duplicate", len(copies))
	}
}

// subjectMatches returns ids in box whose subject contains sub.
func subjectMatches(t *testing.T, ctx context.Context, c *Client, box mail.ID, sub string) []mail.ID {
	t.Helper()
	_, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
		MailboxID:       box,
		CollapseThreads: false,
		Search:          &mail.SearchFilter{Subject: sub},
		Limit:           50,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	out := make([]mail.ID, 0, len(sums))
	for _, s := range sums {
		out = append(out, s.ID)
	}
	return out
}

// TestLiveM5ProbeDraftRoundTrip exercises the composer's autosave path:
// a create carrying Cc/Bcc and an attachment blob, then the recreate an
// edit must perform (RFC 8621 content immutability) with Cc and the
// attachment cleared. Artifacts are destroyed.
func TestLiveM5ProbeDraftRoundTrip(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	blob, err := c.UploadBlob(ctx, "probe.txt", "text/plain", int64(len("probe bytes")),
		strings.NewReader("probe bytes"))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	t.Logf("uploaded blob %q (%d bytes)", blob.BlobID, blob.Size)

	d := mail.Draft{
		MailboxID:   f.drafts,
		IdentityID:  f.identity,
		From:        []mail.Address{{Name: "jmap-tui M5 probe", Email: user}},
		To:          []mail.Address{{Email: "to@example.test"}},
		Cc:          []mail.Address{{Email: "cc@example.test"}},
		Bcc:         []mail.Address{{Email: "bcc@example.test"}},
		Subject:     "M5 draft roundtrip",
		Text:        "first body\n",
		Attachments: []mail.Attachment{blob},
	}
	id, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft create: %v", err)
	}
	defer func() { destroyEmails(t, ctx, c, id) }()

	got := fetchEmail(t, ctx, c, id, "id", "subject", "to", "cc", "bcc", "keywords", "mailboxIds", "attachments", "preview")
	for i, a := range got.Attachments {
		t.Logf("attachment[%d] blobId=%q name=%q type=%q size=%d disposition=%q",
			i, a.BlobID, a.Name, a.Type, a.Size, a.Disposition)
	}
	t.Logf("created %s: subject=%q to=%v cc=%v bcc=%v attachments=%d",
		id, got.Subject, addrEmails(got.To), addrEmails(got.CC), addrEmails(got.BCC), len(got.Attachments))
	if got.Subject != d.Subject {
		t.Errorf("subject=%q want %q", got.Subject, d.Subject)
	}
	if !hasAddr(got.CC, "cc@example.test") {
		t.Errorf("cc not persisted: %v", addrEmails(got.CC))
	}
	if !hasAddr(got.BCC, "bcc@example.test") {
		t.Errorf("bcc not persisted: %v", addrEmails(got.BCC))
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachment not persisted: %d parts", len(got.Attachments))
	}
	// The server derives its own blob id when it writes the message; that
	// id is what every later save of this draft must reference.
	saved := mail.Attachment{
		BlobID: mail.ID(got.Attachments[0].BlobID),
		Name:   got.Attachments[0].Name,
		Type:   got.Attachments[0].Type,
		Size:   int64(got.Attachments[0].Size),
	}

	// An edit recreates: new Email with the new content, old one retired.
	d.ID = id
	d.Subject = "M5 draft roundtrip (edited)"
	d.Text = "second body\n"
	d.Cc = nil
	d.Attachments = nil
	id2, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft recreate: %v", err)
	}
	destroyEmails(t, ctx, c, id) // predecessor, successor already exists
	id = id2
	defer func() { destroyEmails(t, ctx, c, id) }()

	got = fetchEmail(t, ctx, c, id, "id", "subject", "to", "cc", "bcc", "keywords", "mailboxIds", "attachments", "preview")
	t.Logf("recreated %s: subject=%q cc=%v attachments=%d keywords=%v preview=%q",
		id, got.Subject, addrEmails(got.CC), len(got.Attachments), got.Keywords, got.Preview)
	if got.Subject != d.Subject {
		t.Errorf("recreate did not carry the edit: subject=%q want %q", got.Subject, d.Subject)
	}
	if len(got.CC) != 0 {
		t.Errorf("cleared cc came back: %v", addrEmails(got.CC))
	}
	if len(got.Attachments) != 0 {
		t.Errorf("cleared attachments came back: %d parts", len(got.Attachments))
	}
	if !got.Keywords["$draft"] {
		t.Errorf("recreate dropped $draft: %v", got.Keywords)
	}
	if !hasID(mailboxList(got), string(f.drafts)) {
		t.Errorf("recreate misfiled the draft: %v", mailboxList(got))
	}

	// Third save, attachment carried over by the server's own blob id: the
	// reference must still resolve after its message was replaced.
	d.Attachments = []mail.Attachment{saved}
	d.Text = "third body\n"
	id3, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft with carried attachment: %v", err)
	}
	destroyEmails(t, ctx, c, id)
	id = id3
	defer func() { destroyEmails(t, ctx, c, id) }()
	got = fetchEmail(t, ctx, c, id, "id", "subject", "attachments")
	t.Logf("recreated %s with carried attachment: %d part(s)", id, len(got.Attachments))
	if len(got.Attachments) != 1 {
		t.Errorf("carried attachment lost on recreate: %d parts", len(got.Attachments))
	}
}

// addrEmails flattens an address list to bare emails for assertions.
func addrEmails(addrs []*jmapmail.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a != nil {
			out = append(out, a.Email)
		}
	}
	return out
}

// hasAddr reports whether an address list contains email.
func hasAddr(addrs []*jmapmail.Address, email string) bool {
	for _, a := range addrs {
		if a != nil && a.Email == email {
			return true
		}
	}
	return false
}

// TestLiveM5ProbeDraftPatchKeys pins down what `Email/set update` may
// touch on live Stalwart. RFC 8621 §4.1.2 marks every content property
// (subject, from/to/cc/bcc, textBody, bodyValues, attachments) immutable,
// so only keywords and mailbox membership are patchable — which is exactly
// why SaveDraft edits by recreating. If a server ever accepts more, this
// test fails loudly rather than the composer silently assuming otherwise.
func TestLiveM5ProbeDraftPatchKeys(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	blob, err := c.UploadBlob(ctx, "probe.txt", "text/plain", int64(len("probe bytes")),
		strings.NewReader("probe bytes"))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	full := mail.Draft{
		MailboxID:   f.drafts,
		IdentityID:  f.identity,
		From:        []mail.Address{{Name: "jmap-tui M5 probe", Email: user}},
		To:          []mail.Address{{Email: "to@example.test"}},
		Cc:          []mail.Address{{Email: "cc@example.test"}},
		Bcc:         []mail.Address{{Email: "bcc@example.test"}},
		Subject:     "M5 patch probe",
		Text:        "body\n",
		Attachments: []mail.Attachment{blob},
	}
	id, err := c.SaveDraft(ctx, full)
	if err != nil {
		t.Fatalf("SaveDraft create: %v", err)
	}
	defer destroyEmails(t, ctx, c, id)

	cases := []struct {
		key       string
		value     any
		patchable bool
	}{
		{"subject", "patched subject", false},
		{"from", toJMAPAddrs(full.From), false},
		{"to", toJMAPAddrs(full.To), false},
		{"cc", toJMAPAddrs(full.Cc), false},
		{"bcc", toJMAPAddrs(full.Bcc), false},
		{"textBody", []*email.BodyPart{{PartID: "1", Type: "text/plain"}}, false},
		{"bodyValues", map[string]*email.BodyValue{"1": {Value: "patched body\n"}}, false},
		{"attachments", attachmentParts(nil), false},
		{"keywords", map[string]bool{"$draft": true, "$seen": true}, true},
		{"keywords/$seen", true, true},
		{"keywords/$draft", true, true},
	}
	for _, tc := range cases {
		req := &jmap.Request{Context: ctx}
		call := req.Invoke(&email.Set{
			Account: jmap.ID(c.accountID),
			Update:  map[jmap.ID]jmap.Patch{jmap.ID(id): {tc.key: tc.value}},
		})
		invs, err := c.runBatch(ctx, req)
		if err != nil {
			t.Fatalf("patch %s: batch failed: %v", tc.key, err)
		}
		v, ok := asEmailSetView(invs[call])
		if !ok {
			t.Fatalf("patch %s: unexpected response %T", tc.key, invs[call].Args)
		}
		se, rejected := v.NotUpdated[jmap.ID(id)]
		if tc.patchable && rejected {
			t.Errorf("patch %s should be accepted, got %s %s", tc.key, se.Type, seDesc(se.Description))
		}
		if !tc.patchable && !rejected {
			t.Errorf("patch %s should be rejected as immutable (RFC 8621 §4.1.2) but was accepted", tc.key)
		}
		if rejected && se.Type != "invalidProperties" {
			t.Errorf("patch %s: got %s, want invalidProperties", tc.key, se.Type)
		}
	}
}

// TestLiveM5ProbeDraftRecreate proves the only RFC-legal way to edit a
// draft: content properties are immutable (RFC 8621 §4.1.2), so an edit is
// "write a new Email, retire the old one". It also settles which blob id a
// subsequent save may reference — the raw upload id or the id the server
// derived when the message was written — because getting that wrong makes
// every autosave after the first fail with blobNotFound.
func TestLiveM5ProbeDraftRecreate(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	payload := []byte("recreate probe payload")
	up, err := c.UploadBlob(ctx, "recreate.txt", "text/plain", int64(len(payload)), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}
	t.Logf("uploaded blob id %q", up.BlobID)

	base := mail.Draft{
		MailboxID:   f.drafts,
		IdentityID:  f.identity,
		From:        []mail.Address{{Name: "jmap-tui M5 probe", Email: user}},
		To:          []mail.Address{{Email: "recreate@example.test"}},
		Subject:     "M5 recreate probe",
		Text:        "v1\n",
		Attachments: []mail.Attachment{up},
	}
	idA, err := c.SaveDraft(ctx, base)
	if err != nil {
		t.Fatalf("SaveDraft v1: %v", err)
	}
	a := fetchEmail(t, ctx, c, idA, "id", "attachments")
	if len(a.Attachments) != 1 {
		t.Fatalf("v1 has %d attachments", len(a.Attachments))
	}
	derived := mail.Attachment{
		BlobID: mail.ID(a.Attachments[0].BlobID),
		Name:   a.Attachments[0].Name,
		Type:   a.Attachments[0].Type,
		Size:   int64(a.Attachments[0].Size),
	}
	t.Logf("v1 attachment blob id %q (upload id %q)", derived.BlobID, up.BlobID)

	// Edit: a fresh Email carries the new content, then the old one goes.
	base.ID = idA
	base.Subject = "M5 recreate probe (edited)"
	base.Text = "v2\n"
	idB, err := c.SaveDraft(ctx, base)
	if err != nil {
		t.Fatalf("SaveDraft v2 (recreate): %v", err)
	}
	t.Logf("recreate produced new draft id %q (old %q)", idB, idA)
	if idB == idA {
		t.Error("recreate returned the old id; content edits cannot reuse it")
	}
	b := fetchEmail(t, ctx, c, idB, "id", "subject", "textBody", "attachments")
	if b.Subject != base.Subject {
		t.Errorf("v2 subject=%q want %q", b.Subject, base.Subject)
	}
	if len(b.Attachments) != 1 {
		t.Errorf("v2 lost the attachment: %d parts", len(b.Attachments))
	}
	// Retire the predecessor only once its successor exists.
	destroyEmails(t, ctx, c, idA)
	defer destroyEmails(t, ctx, c, idB)

	// Can the *upload* blob id be referenced again after v1 was destroyed?
	again := base
	again.ID = ""
	again.Attachments = []mail.Attachment{up}
	idC, err := c.SaveDraft(ctx, again)
	if err != nil {
		t.Logf("re-referencing the ORIGINAL upload blob id after destroy: FAILED (%v)", err)
	} else {
		cc := fetchEmail(t, ctx, c, idC, "id", "subject", "attachments")
		t.Logf("re-referencing the ORIGINAL upload blob id after destroy: ok, %d attachment(s)", len(cc.Attachments))
		destroyEmails(t, ctx, c, idC)
	}
	_ = idC
}

// TestLiveM5ProbeEmptyBodyDraft is the autosave a brand-new composer
// triggers: recipient filled in, subject and body still empty. go-jmap
// tags BodyValue.Value omitempty, so a "" body used to ship a bodyValues
// entry with no "value" member while textBody advertised partId "1" —
// live Stalwart answers that with "invalidProperties: Missing body value
// for partId". The create must be accepted with no text part at all.
// Artifact destroyed.
func TestLiveM5ProbeEmptyBodyDraft(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := loadComposeFixture(t, ctx, c)

	d := mail.Draft{
		MailboxID:  f.drafts,
		IdentityID: f.identity,
		From:       []mail.Address{{Name: "jmap-tui M5 probe", Email: user}},
		To:         []mail.Address{{Email: "to@example.test"}},
	}
	id, err := c.SaveDraft(ctx, d)
	if err != nil {
		t.Fatalf("SaveDraft with no subject or body: %v", err)
	}
	defer func() { destroyEmails(t, ctx, c, id) }()

	got := fetchEmail(t, ctx, c, id, "id", "keywords", "mailboxIds", "preview")
	t.Logf("empty-bodied draft %s: keywords=%v preview=%q", id, got.Keywords, got.Preview)
	if !got.Keywords["$draft"] {
		t.Errorf("$draft missing: %v", got.Keywords)
	}
	if !hasID(mailboxList(got), string(f.drafts)) {
		t.Errorf("misfiled: %v", mailboxList(got))
	}
}
