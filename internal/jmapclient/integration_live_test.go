package jmapclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/emailsubmission"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// mkAddr builds a go-jmap address for fixture creation.
func mkAddr(name, email string) *jmapmail.Address {
	return &jmapmail.Address{Name: name, Email: email}
}

// emailSetView is the provider-agnostic view of an Email/set response:
// once Mutate runs in the process, the wrapper's flexible decoder replaces
// go-jmap's typed registration, so live tests read both shapes.
type emailSetView struct {
	CreatedIDs   map[string]mail.ID // create-handle → server id
	Updated      []mail.ID
	Destroyed    []jmap.ID
	NotCreated   map[jmap.ID]*jmap.SetError
	NotUpdated   map[jmap.ID]*jmap.SetError
	NotDestroyed map[jmap.ID]*jmap.SetError
}

func asEmailSetView(inv *jmap.Invocation) (emailSetView, bool) {
	switch r := inv.Args.(type) {
	case *email.SetResponse:
		created := map[string]mail.ID{}
		for handle, em := range r.Created {
			if em != nil {
				created[string(handle)] = mail.ID(em.ID)
			}
		}
		updated := make([]mail.ID, 0, len(r.Updated))
		for id := range r.Updated {
			updated = append(updated, mail.ID(id))
		}
		return emailSetView{
			CreatedIDs:   created,
			Updated:      updated,
			Destroyed:    r.Destroyed,
			NotCreated:   r.NotCreated,
			NotUpdated:   r.NotUpdated,
			NotDestroyed: r.NotDestroyed,
		}, true
	case *flexibleSetResponse:
		notCreated := map[jmap.ID]*jmap.SetError{}
		for id, se := range r.NotCreated {
			notCreated[jmap.ID(id)] = se
		}
		notUpdated := map[jmap.ID]*jmap.SetError{}
		for id, se := range r.NotUpdated {
			notUpdated[jmap.ID(id)] = se
		}
		notDestroyed := map[jmap.ID]*jmap.SetError{}
		for id, se := range r.NotDestroyed {
			notDestroyed[jmap.ID(id)] = se
		}
		updated := append([]mail.ID{}, r.Updated...)
		created := map[string]mail.ID{}
		for handle, raw := range r.Created {
			var withID struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &withID) == nil {
				created[handle] = mail.ID(withID.ID)
			}
		}
		return emailSetView{
			CreatedIDs:   created,
			Updated:      updated,
			Destroyed:    r.Destroyed,
			NotCreated:   notCreated,
			NotUpdated:   notUpdated,
			NotDestroyed: notDestroyed,
		}, true
	}
	return emailSetView{}, false
}

// liveCreds returns the env-gated Stalwart test credentials (AGENTS.md:
// unset env ⇒ skip; never hardcode, never echo).
func liveCreds(t *testing.T) (url, user, pass string) {
	t.Helper()
	url = os.Getenv("JMAP_TUI_TEST_URL")
	user = os.Getenv("JMAP_TUI_TEST_USER")
	pass = os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if url == "" || user == "" || pass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER / _PASSWORD)")
	}
	return url, user, pass
}

// TestLiveSessionAndMailboxes is the live-server integration test (M0 gate).
func TestLiveSessionAndMailboxes(t *testing.T) {
	url, user, pass := liveCreds(t)

	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := c.SessionInfo()
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	t.Logf("connected: %d account(s), primary mail account %q, %d capabilities",
		len(info.Accounts), info.PrimaryMailAccount, len(info.Capabilities))

	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	roles := 0
	for _, mb := range mbs.Mailboxes {
		if mb.Role != "" {
			roles++
		}
	}
	t.Logf("mailboxes: %d total, %d with roles, state %q", len(mbs.Mailboxes), roles, mbs.State)
}

// TestLiveReaderVerification is the M1 live gate: fixtures are created
// inside the designated agent-test mailbox tree only, exercised through
// the provider surface (query → summaries → bodies → threads), and the
// created mailbox is destroyed in cleanup. One batched Email/set carries
// every creation (rate courtesy, FR-K4).
func TestLiveReaderVerification(t *testing.T) {
	url, user, pass := liveCreds(t)

	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	readerID, createdAgentRoot := ensureReaderMailbox(t, c, ctx)
	t.Cleanup(func() {
		destroyMailbox(t, c, context.Background(), readerID)
		if createdAgentRoot != "" {
			// Destroying the root removes the whole created subtree.
			destroyMailbox(t, c, context.Background(), createdAgentRoot)
		}
	})

	seedReaderFixtures(t, c, ctx, readerID, user)

	// --- provider flow: mailbox tree ---
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var reader *mail.Mailbox
	for i := range mbs.Mailboxes {
		if mbs.Mailboxes[i].Name == "reader" && mbs.Mailboxes[i].ParentID != "" {
			reader = &mbs.Mailboxes[i]
		}
	}
	if reader == nil {
		t.Fatal("reader mailbox missing from the tree")
	}
	t.Logf("reader mailbox: id=%s total=%d unread=%d", reader.ID, reader.TotalEmails, reader.UnreadEmails)

	// --- collapsed query ---
	h, sums, err := c.OpenQuery(ctx, mail.QuerySpec{
		MailboxID:       reader.ID,
		CollapseThreads: true,
		Position:        0,
		Limit:           50,
	})
	if err != nil {
		t.Fatalf("OpenQuery: %v", err)
	}
	if h.Total() != 3 {
		t.Fatalf("collapsed total = %d, want 3 (2-member thread collapses)", h.Total())
	}
	if len(sums) != 3 {
		t.Fatalf("summaries = %d, want 3", len(sums))
	}
	t.Logf("collapsed query: total=%d state=%s first=%q", h.Total(), h.State(), sums[0].Subject)

	// --- thread expansion through inThread ---
	threadID := sums[0].ThreadID
	for _, s := range sums {
		if s.Subject == "live thread starter" {
			threadID = s.ThreadID
		}
	}
	th, thSums, err := c.OpenQuery(ctx, mail.QuerySpec{
		ThreadID: threadID,
		Sort:     []mail.SortCriterion{{Property: "receivedAt"}},
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("OpenQuery(thread): %v", err)
	}
	if th.Total() != 2 {
		t.Fatalf("thread members = %d, want 2", th.Total())
	}
	t.Logf("thread %s: %d members (oldest first: %q)", threadID, th.Total(), thSums[0].Subject)

	// --- HTML-only body through the FR-E2 preference path ---
	var htmlID mail.ID
	for _, s := range sums {
		if s.Subject == "live html only" {
			htmlID = s.ID
		}
	}
	body, err := c.FetchBody(ctx, htmlID)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if body.Text != "" || body.HTML == "" {
		t.Fatalf("html-only body mis-detected: text=%q", body.Text)
	}
	if len(body.HTML) > 0 && !strings.Contains(body.HTML, "live") {
		t.Logf("note: html body: %.80s", body.HTML)
	}
	t.Logf("html body fetched: %d bytes", len(body.HTML))

	// --- plain body ---
	var plainID mail.ID
	for _, s := range sums {
		if s.Subject == "live plain" {
			plainID = s.ID
		}
	}
	plain, err := c.FetchBody(ctx, plainID)
	if err != nil {
		t.Fatalf("FetchBody(plain): %v", err)
	}
	if plain.Text == "" {
		t.Fatal("plain body empty")
	}
	t.Logf("plain body fetched: %d bytes", len(plain.Text))
}

// ensureReaderMailbox creates agent-test/reader (and agent-test when
// missing). It returns the reader mailbox id and, when this run created
// the agent-test root, its id for cleanup.
func ensureReaderMailbox(t *testing.T, c *Client, ctx context.Context) (readerID string, createdAgentRoot string) {
	t.Helper()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var agentID string
	for _, mb := range mbs.Mailboxes {
		if mb.Name == "agent-test" {
			agentID = string(mb.ID)
		}
		if mb.Name == "reader" && mb.ParentID != "" {
			// Leftover from a previous run: purge so every run starts
			// with known fixtures (only touches our own test mailbox).
			destroyMailbox(t, c, ctx, string(mb.ID))
		}
	}

	var resp *mailbox.SetResponse
	if agentID == "" {
		// Create the agent-test root first, then reader beneath it.
		req := &jmap.Request{Context: ctx}
		rootSet := &mailbox.Set{Account: jmap.ID(c.accountID), Create: map[jmap.ID]*mailbox.Mailbox{
			"agent-root": {Name: "agent-test"},
		}}
		callID := req.Invoke(rootSet)
		invs, err := c.runBatch(ctx, req)
		if err != nil {
			t.Fatalf("create agent-test root: %v", err)
		}
		inv, ok := invs[callID]
		if !ok {
			t.Fatal("no Mailbox/set response for agent-test root")
		}
		resp = inv.Args.(*mailbox.SetResponse)
		cr, ok := resp.Created["agent-root"]
		if !ok {
			t.Fatalf("agent-test root not created: %+v", resp.NotCreated)
		}
		agentID = string(cr.ID)
	}

	req := &jmap.Request{Context: ctx}
	set := &mailbox.Set{Account: jmap.ID(c.accountID), Create: map[jmap.ID]*mailbox.Mailbox{
		"reader": {Name: "reader", ParentID: jmap.ID(agentID)},
	}}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	inv, ok := invs[callID]
	if !ok {
		t.Fatal("no Mailbox/set response for reader")
	}
	resp = inv.Args.(*mailbox.SetResponse)
	cr, ok := resp.Created["reader"]
	if !ok {
		for cid, se := range resp.NotCreated {
			t.Fatalf("mailbox %s not created: %s %s", cid, se.Type, seDesc(se.Description))
		}
		t.Fatal("reader mailbox not created")
	}
	if mbs0 := len(mbs.Mailboxes); mbs0 == 0 {
		t.Log("fresh account: created the full agent-test tree")
	}
	return string(cr.ID), agentID
}

// seedReaderFixtures creates the fixture emails in one batched /set.
func seedReaderFixtures(t *testing.T, c *Client, ctx context.Context, readerID, user string) {
	t.Helper()
	now := time.Now().UTC()
	msgID := fmt.Sprintf("<live-verify-%d@agent-test.local>", now.UnixNano())

	create := map[jmap.ID]*email.Email{
		"plain": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(readerID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "live plain",
			ReceivedAt: &now,
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{
				"1": {Value: "Hello from the live reader verification.\n"},
			},
		},
		"html": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(readerID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "live html only",
			ReceivedAt: &now,
			HTMLBody:   []*email.BodyPart{{PartID: "1", Type: "text/html"}},
			BodyValues: map[string]*email.BodyValue{
				"1": {Value: "<html><body><p>live <b>html</b> body</p></body></html>"},
			},
		},
		"thread-1": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(readerID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "live thread starter",
			ReceivedAt: &now,
			MessageID:  []string{msgID},
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{
				"1": {Value: "Thread starter body.\n"},
			},
		},
		"thread-2": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(readerID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "Re: live thread starter",
			ReceivedAt: &now,
			References: []string{msgID},
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{
				"1": {Value: "Thread reply body.\n"},
			},
		},
	}

	req := &jmap.Request{Context: ctx}
	set := &email.Set{Account: jmap.ID(c.accountID), Create: create}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("create emails: %v", err)
	}
	inv, ok := invs[callID]
	if !ok {
		t.Fatal("no Email/set response")
	}
	resp, ok := asEmailSetView(inv)
	if !ok {
		t.Fatalf("unexpected Email/set response %T", inv.Args)
	}
	if len(resp.NotCreated) > 0 {
		for id, e := range resp.NotCreated {
			desc := ""
			if e.Description != nil {
				desc = *e.Description
			}
			t.Errorf("email %s not created: %s", id, desc)
		}
		t.Fatal("fixture creation failed")
	}
	t.Logf("seeded %d fixture emails", len(resp.CreatedIDs))
}

// destroyMailbox removes a mailbox and its contents (test cleanup only).
// The mailbox is emptied via Email/set destroy first — Stalwart refuses to
// destroy non-empty mailboxes and rejects the onDestroyRemoveContents
// patch path.
func destroyMailbox(t *testing.T, c *Client, ctx context.Context, id string) {
	t.Helper()
	// Purge contents: query every id, then destroy them in one /set.
	qReq := &jmap.Request{Context: ctx}
	q := &email.Query{Account: jmap.ID(c.accountID), CalculateTotal: true, Limit: 1000}
	q.Filter = &email.FilterCondition{InMailbox: jmap.ID(id)}
	qID := qReq.Invoke(q)
	qInvs, err := c.runBatch(ctx, qReq)
	if err != nil {
		t.Logf("cleanup: query mailbox %s: %v", id, err)
		return
	}
	qr, ok := qInvs[qID].Args.(*email.QueryResponse)
	if !ok {
		t.Logf("cleanup: unexpected query response for %s", id)
		return
	}
	if len(qr.IDs) > 0 {
		dReq := &jmap.Request{Context: ctx}
		ds := &email.Set{Account: jmap.ID(c.accountID), Destroy: qr.IDs}
		dID := dReq.Invoke(ds)
		dInvs, err := c.runBatch(ctx, dReq)
		if err != nil {
			t.Logf("cleanup: destroy emails in %s: %v", id, err)
			return
		}
		if inv, ok := dInvs[dID]; ok {
			if view, ok := asEmailSetView(inv); ok && len(view.NotDestroyed) > 0 {
				t.Logf("cleanup: %d email(s) not destroyed", len(view.NotDestroyed))
			}
		}
	}

	req := &jmap.Request{Context: ctx}
	set := &mailbox.Set{Account: jmap.ID(c.accountID), Destroy: []jmap.ID{jmap.ID(id)}}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Logf("cleanup: destroy mailbox %s: %v", id, err)
		return
	}
	inv, ok := invs[callID]
	if !ok {
		return
	}
	if resp, ok := inv.Args.(*mailbox.SetResponse); ok && len(resp.NotDestroyed) > 0 {
		for mid, se := range resp.NotDestroyed {
			t.Logf("cleanup: mailbox %s not destroyed: %s %s", mid, se.Type, seDesc(se.Description))
		}
	}
}

// seDesc safely renders an optional SetError description.
func seDesc(d *string) string {
	if d == nil {
		return ""
	}
	return *d
}

// TestLiveM2PushGate is the M2 acceptance gate (REQUIREMENTS §7): with the
// sync engine running against the live server, mail sent to the account
// (send-to-self via EmailSubmission, per AGENTS.md test rules) appears and
// counts update over push without any manual refresh; a flag change on the
// delivered message patches the running engine. Latency is measured against
// the ~1s target with a generous hard bound. All created artifacts (draft,
// sent copy, delivered messages) are destroyed in cleanup.
func TestLiveM2PushGate(t *testing.T) {
	url, user, pass := liveCreds(t)

	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	identities, err := c.Identities(ctx)
	if err != nil {
		t.Fatalf("Identities: %v", err)
	}
	if len(identities) == 0 {
		t.Fatal("no identities on the test account; cannot send-to-self")
	}
	identityID := identities[0].ID
	t.Logf("identity: %s <%s>", identities[0].Name, identities[0].Email)

	// --- sync engine with push running, inbox open ---
	e := sync.NewEngine(c, sync.Config{})
	e.Start(ctx)
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	var inboxID mail.ID
	for _, mb := range e.Snapshot().Mailboxes {
		if mb.Mailbox.Role == mail.RoleInbox {
			inboxID = mb.Mailbox.ID
		}
	}
	if inboxID == "" {
		t.Fatal("no inbox role mailbox on the test account")
	}
	if err := e.OpenMailbox(ctx, inboxID); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	waitForLiveMode(t, e, 15*time.Second)

	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	subject := "jmap-tui M2 live gate " + nonce

	// --- send one message to self: draft + submission in one batch ---
	draftID, err := sendToSelf(t, c, ctx, identityID, user, subject)
	if err != nil {
		t.Fatalf("send-to-self: %v", err)
	}
	t.Logf("submission accepted (draft %s)", draftID)
	t.Cleanup(func() {
		purgeTestMessages(t, c, context.Background(), subject)
	})

	// --- arrival over push: message appears in the open inbox view ---
	t0 := time.Now()
	var arrivedAt time.Time
	deadline := t0.Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap := e.Snapshot()
		for _, r := range snap.Rows {
			if r.Summary.Subject == subject {
				arrivedAt = time.Now()
				break
			}
		}
		if !arrivedAt.IsZero() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if arrivedAt.IsZero() {
		t.Fatal("sent message never appeared in the live inbox view")
	}
	latency := arrivedAt.Sub(t0)
	t.Logf("push arrival latency: %v (target ~1s)", latency)
	if latency > 15*time.Second {
		t.Fatalf("arrival took %v; push path not working", latency)
	}

	// --- flag change over push: mark the delivered message read ---
	var msgID mail.ID
	for _, r := range e.Snapshot().Rows {
		if r.Summary.Subject == subject {
			msgID = r.Summary.ID
		}
	}
	if msgID == "" {
		t.Fatal("arrived message lost from the window before flag step")
	}
	var unreadBefore int
	for _, node := range e.Snapshot().Mailboxes {
		if node.Mailbox.ID == inboxID {
			unreadBefore = node.Mailbox.UnreadEmails
		}
	}
	if unreadBefore < 1 {
		t.Fatalf("inbox unread = %d before the test; unread count assertion needs >= 1", unreadBefore)
	}
	if err := setSeen(t, c, ctx, msgID, true); err != nil {
		t.Fatalf("flag flip: %v", err)
	}
	flagT0 := time.Now()
	waitForSnap(t, 15*time.Second, func() bool {
		for _, r := range e.Snapshot().Rows {
			if r.Summary.ID == msgID {
				return r.Summary.Keywords.Has("$seen")
			}
		}
		return false
	})
	t.Logf("flag patch latency: %v (target ~1s)", time.Since(flagT0))

	// --- counts over push: unread count drops by one via Mailbox/changes ---
	waitForSnap(t, 15*time.Second, func() bool {
		for _, node := range e.Snapshot().Mailboxes {
			if node.Mailbox.ID == inboxID {
				return node.Mailbox.UnreadEmails == unreadBefore-1
			}
		}
		return false
	})
	t.Logf("inbox unread count went %d → %d over Mailbox/changes", unreadBefore, unreadBefore-1)

	if mode := e.Snapshot().Status.Mode; mode != sync.ModePush {
		t.Fatalf("sync mode = %q, want push for the whole gate", mode)
	}
}

// waitForLiveMode waits until the engine reports an established push stream.
func waitForLiveMode(t *testing.T, e *sync.Engine, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.Snapshot().Status.Mode == sync.ModePush {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("engine never reached push mode (status %+v)", e.Snapshot().Status)
}

// waitForSnap polls a snapshot predicate.
func waitForSnap(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("snapshot predicate not met in time")
}

// sendToSelf creates a draft and submits it in one batched request
// (FR-K4), returning the created email id. The message travels the real
// SMTP path and lands in the account's own inbox.
func sendToSelf(t *testing.T, c *Client, ctx context.Context, identityID mail.ID, user, subject string) (mail.ID, error) {
	t.Helper()

	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		return "", err
	}
	var draftsID mail.ID
	for _, mb := range mbs.Mailboxes {
		if mb.Role == mail.RoleDrafts {
			draftsID = mb.ID
		}
	}
	if draftsID == "" {
		return "", errors.New("no drafts role mailbox")
	}

	now := time.Now().UTC()
	req := &jmap.Request{Context: ctx}
	c1 := req.Invoke(&email.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*email.Email{
			"draft": {
				MailboxIDs: map[jmap.ID]bool{jmap.ID(draftsID): true},
				From:       []*jmapmail.Address{mkAddr("jmap-tui gate", user)},
				To:         []*jmapmail.Address{mkAddr("jmap-tui gate", user)},
				Subject:    subject,
				Keywords:   map[string]bool{"$draft": true, "$seen": true},
				TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
				BodyValues: map[string]*email.BodyValue{
					"1": {Value: "M2 live gate message — safe to ignore, auto-cleaned.\n"},
				},
				ReceivedAt: &now,
			},
		},
	})
	c2 := req.Invoke(&emailsubmission.Set{
		Account: jmap.ID(c.accountID),
		Create: map[jmap.ID]*emailsubmission.EmailSubmission{
			"sub": {IdentityID: jmap.ID(identityID), EmailID: "#draft"},
		},
		// Self-cleaning: the draft copy is destroyed once submitted.
		OnSuccessDestroyEmail: []jmap.ID{"#draft"},
	})
	resp, err := c.post(ctx, req)
	if err != nil {
		return "", err
	}

	// Scan in order, not by call id: the server also answers the implicit
	// Email/set from onSuccessDestroyEmail, which can reuse the submission
	// call id and would shadow it in a map.
	var createdID mail.ID
	for _, inv := range resp.Responses {
		switch {
		case inv.Name == "error":
			me, ok := inv.Args.(*jmap.MethodError)
			if ok {
				return "", fmt.Errorf("batch error: %s %s", me.Type, deref(me.Description))
			}
			return "", fmt.Errorf("batch error invocation with args %T", inv.Args)
		case inv.Name == "Email/set" && inv.CallID == c1:
			view, ok := asEmailSetView(inv)
			if !ok {
				return "", fmt.Errorf("unexpected Email/set response %T", inv.Args)
			}
			if len(view.NotCreated) > 0 {
				for cid, se := range view.NotCreated {
					return "", fmt.Errorf("draft %s not created: %s %s", cid, se.Type, seDesc(se.Description))
				}
			}
			createdID, ok = view.CreatedIDs["draft"]
			if !ok {
				return "", errors.New("draft creation missing from response")
			}
		case inv.Name == "EmailSubmission/set" && inv.CallID == c2:
			uresp, ok := inv.Args.(*emailsubmission.SetResponse)
			if !ok {
				return "", fmt.Errorf("unexpected EmailSubmission/set response %T", inv.Args)
			}
			if len(uresp.NotCreated) > 0 {
				for cid, se := range uresp.NotCreated {
					return "", fmt.Errorf("submission %s failed: %s %s", cid, se.Type, seDesc(se.Description))
				}
			}
		}
	}
	if createdID == "" {
		return "", errors.New("draft id never resolved")
	}
	return createdID, nil
}

// setSeen flips the $seen keyword of one message.
func setSeen(t *testing.T, c *Client, ctx context.Context, id mail.ID, seen bool) error {
	t.Helper()
	req := &jmap.Request{Context: ctx}
	c1 := req.Invoke(&email.Set{
		Account: jmap.ID(c.accountID),
		Update: map[jmap.ID]jmap.Patch{
			jmap.ID(id): {"keywords/$seen": seen},
		},
	})
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		return err
	}
	inv, ok := invs[c1]
	if !ok {
		return errors.New("no Email/set response")
	}
	view, ok := asEmailSetView(inv)
	if !ok {
		return fmt.Errorf("unexpected Email/set response %T", inv.Args)
	}
	if len(view.NotUpdated) > 0 {
		for cid, se := range view.NotUpdated {
			return fmt.Errorf("update %s failed: %s %s", cid, se.Type, seDesc(se.Description))
		}
	}
	return nil
}

// purgeTestMessages destroys every message carrying the gate subject from
// every mailbox (drafts copy, sent copy, delivered inbox copies) — cleanup
// of artifacts this test created, per AGENTS.md.
func purgeTestMessages(t *testing.T, c *Client, ctx context.Context, subject string) {
	t.Helper()
	q := &email.Query{
		Account: jmap.ID(c.accountID),
		Filter:  &email.FilterCondition{Subject: subject},
		Limit:   50,
	}
	req := &jmap.Request{Context: ctx}
	c1 := req.Invoke(q)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Logf("cleanup: query %q: %v", subject, err)
		return
	}
	qr, ok := invs[c1].Args.(*email.QueryResponse)
	if !ok {
		t.Logf("cleanup: unexpected query response")
		return
	}
	if len(qr.IDs) == 0 {
		t.Logf("cleanup: nothing to purge for %q", subject)
		return
	}
	dreq := &jmap.Request{Context: ctx}
	c2 := dreq.Invoke(&email.Set{Account: jmap.ID(c.accountID), Destroy: qr.IDs})
	dinvs, err := c.runBatch(ctx, dreq)
	if err != nil {
		t.Logf("cleanup: destroy %d message(s): %v", len(qr.IDs), err)
		return
	}
	if inv, ok := dinvs[c2]; ok {
		if view, ok := asEmailSetView(inv); ok {
			t.Logf("cleanup: destroyed %d message(s), %d failed", len(qr.IDs)-len(view.NotDestroyed), len(view.NotDestroyed))
		}
	}
}

// --- M3 live gate: triage workflow + attachment download ---

// ensureM3Mailboxes creates the M3 gate mailboxes under agent-test and
// returns their ids (agent-test root created if absent). Cleanup destroys
// both mailboxes with their contents, and the root itself when this run
// created it (AGENTS.md: test mailboxes only, full cleanup after).
func ensureM3Mailboxes(t *testing.T, c *Client, ctx context.Context) (gateID, destID string) {
	t.Helper()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	var agentID string
	for _, mb := range mbs.Mailboxes {
		if mb.Name == "agent-test" {
			agentID = string(mb.ID)
			break
		}
	}
	if agentID == "" {
		req := &jmap.Request{Context: ctx}
		rootSet := &mailbox.Set{Account: jmap.ID(c.accountID), Create: map[jmap.ID]*mailbox.Mailbox{
			"agent-root": {Name: "agent-test"},
		}}
		callID := req.Invoke(rootSet)
		invs, rerr := c.runBatch(ctx, req)
		if rerr != nil {
			t.Fatalf("create agent-test root: %v", rerr)
		}
		resp, ok := invs[callID].Args.(*mailbox.SetResponse)
		if !ok {
			t.Fatalf("unexpected Mailbox/set response %T", invs[callID].Args)
		}
		cr, ok := resp.Created["agent-root"]
		if !ok {
			t.Fatalf("agent-test root not created: %+v", resp.NotCreated)
		}
		agentID = string(cr.ID)
		t.Cleanup(func() {
			// Destroying the root removes the whole created subtree.
			destroyMailbox(t, c, context.Background(), agentID)
		})
	}

	create := map[jmap.ID]*mailbox.Mailbox{
		"gate": {Name: "m3-gate", ParentID: jmap.ID(agentID)},
		"dest": {Name: "m3-gate-dest", ParentID: jmap.ID(agentID)},
	}
	req := &jmap.Request{Context: ctx}
	set := &mailbox.Set{Account: jmap.ID(c.accountID), Create: create}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("create M3 mailboxes: %v", err)
	}
	inv, ok := invs[callID]
	if !ok {
		t.Fatal("no Mailbox/set response for M3 mailboxes")
	}
	resp, ok := inv.Args.(*mailbox.SetResponse)
	if !ok {
		t.Fatalf("unexpected Mailbox/set response %T", inv.Args)
	}
	gate, gateOK := resp.Created["gate"]
	dest, destOK := resp.Created["dest"]
	if !gateOK || !destOK {
		// Leftovers from a previous run: purge and recreate once.
		for _, mb := range mbs.Mailboxes {
			if mb.Name == "m3-gate" && mb.ParentID == mail.ID(agentID) {
				destroyMailbox(t, c, ctx, string(mb.ID))
			}
			if mb.Name == "m3-gate-dest" && mb.ParentID == mail.ID(agentID) {
				destroyMailbox(t, c, ctx, string(mb.ID))
			}
		}
		t.Fatal("M3 mailboxes not created fresh; rerun to purge leftovers")
	}
	return string(gate.ID), string(dest.ID)
}

// seedTriageFixtures creates two unread emails in the gate mailbox.
func seedTriageFixtures(t *testing.T, c *Client, ctx context.Context, gateID, user string) []mail.ID {
	t.Helper()
	now := time.Now().UTC()
	create := map[jmap.ID]*email.Email{
		"tri-1": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(gateID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "m3 triage one",
			ReceivedAt: &now,
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{"1": {Value: "Triage fixture one.\n"}},
		},
		"tri-2": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(gateID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			To:         []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "m3 triage two",
			ReceivedAt: &now,
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{"1": {Value: "Triage fixture two.\n"}},
		},
	}
	req := &jmap.Request{Context: ctx}
	set := &email.Set{Account: jmap.ID(c.accountID), Create: create}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("seed triage fixtures: %v", err)
	}
	view, ok := asEmailSetView(invs[callID])
	if !ok {
		t.Fatalf("unexpected Email/set response %T", invs[callID].Args)
	}
	if len(view.NotCreated) > 0 {
		t.Fatalf("seed failed: %+v", view.NotCreated)
	}
	t.Cleanup(func() {
		purgeByIDs(t, c, context.Background(), view.CreatedIDs["tri-1"], view.CreatedIDs["tri-2"])
	})
	return []mail.ID{view.CreatedIDs["tri-1"], view.CreatedIDs["tri-2"]}
}

// purgeByIDs destroys fixture emails (cleanup).
func purgeByIDs(t *testing.T, c *Client, ctx context.Context, ids ...mail.ID) {
	t.Helper()
	jids := make([]jmap.ID, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			jids = append(jids, jmap.ID(id))
		}
	}
	if len(jids) == 0 {
		return
	}
	req := &jmap.Request{Context: ctx}
	set := &email.Set{Account: jmap.ID(c.accountID), Destroy: jids}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Logf("cleanup: destroy fixtures: %v", err)
		return
	}
	if view, ok := asEmailSetView(invs[callID]); ok && len(view.NotDestroyed) > 0 {
		t.Logf("cleanup: %d fixture(s) not destroyed", len(view.NotDestroyed))
	}
}

// serverEmailState is the independent verification: a raw Email/get reads
// keywords and mailbox memberships straight from the server (REQUIREMENTS
// §7 M3: server state verified to match via an independent client).
func serverEmailState(t *testing.T, c *Client, ctx context.Context, id mail.ID) (mail.Keywords, []mail.ID, bool) {
	t.Helper()
	req := &jmap.Request{Context: ctx}
	get := &email.Get{
		Account:    jmap.ID(c.accountID),
		IDs:        []jmap.ID{jmap.ID(id)},
		Properties: []string{"keywords", "mailboxIds"},
	}
	callID := req.Invoke(get)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("Email/get verify: %v", err)
	}
	gr, ok := invs[callID].Args.(*email.GetResponse)
	if !ok {
		t.Fatalf("unexpected Email/get response %T", invs[callID].Args)
	}
	if len(gr.List) == 0 {
		return nil, nil, false // destroyed / not found
	}
	out := mail.Keywords{}
	for k, v := range gr.List[0].Keywords {
		if v {
			out[k] = struct{}{}
		}
	}
	var mbs []mail.ID
	for mb := range gr.List[0].MailboxIDs {
		mbs = append(mbs, mail.ID(mb))
	}
	return out, mbs, true
}

func assertKeyword(t *testing.T, kw mail.Keywords, name string, want bool, step string) {
	t.Helper()
	if kw.Has(name) != want {
		t.Fatalf("%s: keyword %s presence = %v, want %v", step, name, kw.Has(name), want)
	}
}

func assertMembership(t *testing.T, mbs []mail.ID, id mail.ID, want bool, step string) {
	t.Helper()
	found := false
	for _, mb := range mbs {
		if mb == id {
			found = true
			break
		}
	}
	if found != want {
		t.Fatalf("%s: membership in %s = %v (memberships %v)", step, id, found, mbs)
	}
}

// TestLiveTriageVerification is the M3 gate: the full triage workflow —
// read, star, undo, move, copy, delete, permanent destroy — runs through
// the sync engine against the live server, with every step verified
// independently via raw Email/get. Attachment upload → DownloadBlob is
// verified as the FR-E4 round-trip.
func TestLiveTriageVerification(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	gateID, destID := ensureM3Mailboxes(t, c, ctx)
	t.Cleanup(func() {
		destroyMailbox(t, c, context.Background(), destID)
		destroyMailbox(t, c, context.Background(), gateID)
	})

	ids := seedTriageFixtures(t, c, ctx, gateID, user)
	e1, e2 := ids[0], ids[1]

	// The engine drives the workflow exactly as the app would.
	e := sync.NewEngine(c, sync.Config{})
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, mail.ID(gateID)); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	rows := e.Snapshot().Rows
	if len(rows) != 2 {
		t.Fatalf("gate rows = %d, want 2", len(rows))
	}

	// --- read: both in one batched /set (FR-G3) ---
	start := time.Now()
	rcpt, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageRead, IDs: ids})
	if err != nil {
		t.Fatalf("Triage(read): %v", err)
	}
	t.Logf("read batch: %d ids confirmed in %s", len(rcpt.Applied), time.Since(start).Round(time.Millisecond))
	kw1, _, found := serverEmailState(t, c, ctx, e1)
	if !found {
		t.Fatal("e1 missing after read")
	}
	assertKeyword(t, kw1, "$seen", true, "read")
	kw2, _, _ := serverEmailState(t, c, ctx, e2)
	assertKeyword(t, kw2, "$seen", true, "read")

	// --- star, then undo via the receipt (FR-G5) ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageStar, IDs: ids})
	if err != nil {
		t.Fatalf("Triage(star): %v", err)
	}
	kw1, _, _ = serverEmailState(t, c, ctx, e1)
	assertKeyword(t, kw1, "$flagged", true, "star")
	if rcpt.Undo == nil {
		t.Fatal("star produced no undo spec")
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(unstar-undo): %v", err)
	}
	kw1, _, _ = serverEmailState(t, c, ctx, e1)
	assertKeyword(t, kw1, "$flagged", false, "star undo")
	kw1, _, _ = serverEmailState(t, c, ctx, e1)
	assertKeyword(t, kw1, "$seen", true, "star undo keeps read state")

	// --- move to dest, verify, undo, verify (FR-G2) ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: mail.ID(destID)})
	if err != nil {
		t.Fatalf("Triage(move): %v", err)
	}
	_, mbs1, _ := serverEmailState(t, c, ctx, e1)
	assertMembership(t, mbs1, mail.ID(destID), true, "move")
	assertMembership(t, mbs1, mail.ID(gateID), false, "move")
	if rcpt.Undo == nil {
		t.Fatal("move produced no undo spec")
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(move-undo): %v", err)
	}
	_, mbs1, _ = serverEmailState(t, c, ctx, e1)
	assertMembership(t, mbs1, mail.ID(gateID), true, "move undo")
	assertMembership(t, mbs1, mail.ID(destID), false, "move undo")

	// --- copy + undo (membership additive) ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageCopy, IDs: ids, Mailbox: mail.ID(destID)})
	if err != nil {
		t.Fatalf("Triage(copy): %v", err)
	}
	_, mbs1, _ = serverEmailState(t, c, ctx, e1)
	assertMembership(t, mbs1, mail.ID(destID), true, "copy")
	assertMembership(t, mbs1, mail.ID(gateID), true, "copy")
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(copy-undo): %v", err)
	}
	_, mbs1, _ = serverEmailState(t, c, ctx, e1)
	assertMembership(t, mbs1, mail.ID(destID), false, "copy undo")

	// --- delete = move to role-trash; destroy is permanent there (FR-G2) ---
	_, trashMB := func() (mail.ID, mail.Mailbox) {
		mbs, err := c.Mailboxes(ctx)
		if err != nil {
			t.Fatalf("Mailboxes: %v", err)
		}
		for _, mb := range mbs.Mailboxes {
			if mb.Role == mail.RoleTrash {
				return mb.ID, mb
			}
		}
		return "", mail.Mailbox{}
	}()
	if trashMB.ID == "" {
		t.Fatal("server has no role-trash mailbox")
	}
	if _, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageMove, IDs: []mail.ID{e1}, Mailbox: trashMB.ID}); err != nil {
		t.Fatalf("Triage(delete): %v", err)
	}
	_, mbs1, _ = serverEmailState(t, c, ctx, e1)
	assertMembership(t, mbs1, trashMB.ID, true, "delete")
	if _, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageDestroy, IDs: []mail.ID{e1}}); err != nil {
		t.Fatalf("Triage(destroy): %v", err)
	}
	if _, _, found := serverEmailState(t, c, ctx, e1); found {
		t.Fatal("destroyed message still on the server")
	}
	t.Log("delete → trash → permanent destroy verified")

	// --- FR-E4: upload blob → create email with attachment → DownloadBlob ---
	payload := []byte("attachment round-trip \xf0\x9f\x93\x8e payload")
	blobID, err := c.uploadBlob(ctx, payload, "application/octet-stream")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	now := time.Now().UTC()
	req := &jmap.Request{Context: ctx}
	attSet := &email.Set{Account: jmap.ID(c.accountID), Create: map[jmap.ID]*email.Email{
		"att": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(gateID): true},
			From:       []*jmapmail.Address{mkAddr("Agent Test", user)},
			Subject:    "m3 attachment carrier",
			ReceivedAt: &now,
			Attachments: []*email.BodyPart{{
				BlobID: jmap.ID(blobID), Type: "application/octet-stream",
				Name: "m3-payload.bin", Disposition: "attachment",
			}},
		},
	}}
	setID := req.Invoke(attSet)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("create attachment email: %v", err)
	}
	attView, ok := asEmailSetView(invs[setID])
	if !ok || len(attView.NotCreated) > 0 {
		t.Fatalf("attachment email not created: %+v", attView.NotCreated)
	}
	attEmail := attView.CreatedIDs["att"]
	t.Cleanup(func() { purgeByIDs(t, c, context.Background(), attEmail) })

	body, err := c.FetchBody(ctx, attEmail)
	if err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(body.Attachments))
	}
	rc, err := c.DownloadBlob(ctx, body.Attachments[0].BlobID, body.Attachments[0].Name, body.Attachments[0].Type)
	if err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("download read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded %d bytes, want %d (content mismatch)", len(got), len(payload))
	}
	t.Logf("attachment round-trip verified: %d bytes via %s", len(got), body.Attachments[0].Name)
}

// uploadBlob POSTs raw bytes to the session uploadUrl (RFC 8620 §6.1) and
// returns the server-assigned blob id (fixture creation for FR-E4).
func (c *Client) uploadBlob(ctx context.Context, data []byte, mediaType string) (string, error) {
	if c.session == nil || c.session.UploadURL == "" {
		return "", errors.New("no uploadUrl")
	}
	u := strings.NewReplacer("{accountId}", url.PathEscape(c.accountID)).Replace(c.session.UploadURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mediaType)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload: HTTP %d", resp.StatusCode)
	}
	var out struct {
		BlobID string `json:"blobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.BlobID, nil
}
