package jmapclient

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// mkAddr builds a go-jmap address for fixture creation.
func mkAddr(name, email string) *jmapmail.Address {
	return &jmapmail.Address{Name: name, Email: email}
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
	for _, mb := range mbs {
		if mb.Role != "" {
			roles++
		}
	}
	t.Logf("mailboxes: %d total, %d with roles", len(mbs), roles)
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
	for i := range mbs {
		if mbs[i].Name == "reader" && mbs[i].ParentID != "" {
			reader = &mbs[i]
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
	for _, mb := range mbs {
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
	if mbs0 := len(mbs); mbs0 == 0 {
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
	resp, ok := inv.Args.(*email.SetResponse)
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
	t.Logf("seeded %d fixture emails", len(resp.Created))
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
			if resp, ok := inv.Args.(*email.SetResponse); ok && len(resp.NotDestroyed) > 0 {
				t.Logf("cleanup: %d email(s) not destroyed", len(resp.NotDestroyed))
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
