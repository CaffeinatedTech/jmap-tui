package jmapclient

// UNCOMMITTED GATE SCRATCH — the jmap-bridge M2 gate's jmap-tui half:
// the triage workflow (read, star, move, copy, archive, delete, undo,
// destroy) round-trips through the bridge served over HTTP, with a
// second, independent IMAP session confirming the backend actually
// moved (PLAN §12 M2). Per jmap-tui/AGENTS.md this file stays
// uncommitted until the user decides; it skips unless the bridge creds
// (JMAP_TUI_TEST_*) are set, and the second-client half additionally
// needs the backend session (JMAP_TUI_TEST_FLIP_*).

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	jmap "git.sr.ht/~rockorager/go-jmap"
	jmapmail "git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

const (
	m2GateSubjectA = "M2 gate one"
	m2GateSubjectB = "M2 gate two"
)

// m2IMAP runs one python3/imaplib operation against the bridge's
// backend: "hasflag" reports whether the subject carries the flag,
// "in"/"absent" report presence in a folder. IMAP stays outside
// jmap-tui's dependency set, and the checker must be a genuinely
// independent client anyway.
func m2IMAP(t *testing.T, mode, folder, subject string) {
	t.Helper()
	host, port, user, pass := flipCreds(t)
	script := `
import imaplib, sys
mode, host, port, user, pw, folder, subject = sys.argv[1:8]
m = imaplib.IMAP4(host, int(port))
m.login(user, pw)
m.select(folder, readonly=True)
typ, data = m.uid("search", None, "HEADER", "Subject", chr(34) + subject + chr(34))
uid = data[0].split()[-1] if data and data[0] else None
if mode == "in":
    sys.exit(0 if uid else "subject not in " + folder)
if mode == "absent":
    sys.exit("subject still in " + folder if uid else 0)
if uid is None:
    sys.exit("subject not in " + folder)
typ, data = m.uid("fetch", uid, "(FLAGS)")
flags = data[0].decode(errors="replace") if isinstance(data[0], bytes) else str(data[0])
sys.exit(0 if ("\\Flagged" in flags) == (mode == "flagged") else "flags " + flags)
`
	cmd := exec.Command("python3", "-c", script, mode, host, port, user, pass, folder, subject)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("imap %s via python3: %v (%s)", mode, err, strings.TrimSpace(string(out)))
	}
}

// m2EnsureMailbox returns the id of a top-level mailbox, creating it
// through the bridge when it is missing (Mailbox/set is half of what
// this gate exercises — the role is detected from the name on the
// refresh that follows the CREATE).
func m2EnsureMailbox(t *testing.T, c *Client, ctx context.Context, name string) string {
	t.Helper()
	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	for _, mb := range mbs.Mailboxes {
		if mb.Name == name {
			return string(mb.ID)
		}
	}
	req := &jmap.Request{Context: ctx}
	set := &mailbox.Set{Account: jmap.ID(c.accountID), Create: map[jmap.ID]*mailbox.Mailbox{
		"mk": {Name: name},
	}}
	callID := req.Invoke(set)
	invs, rerr := c.runBatch(ctx, req)
	if rerr != nil {
		t.Fatalf("create %s: %v", name, rerr)
	}
	resp, ok := invs[callID].Args.(*mailbox.SetResponse)
	if !ok {
		t.Fatalf("unexpected Mailbox/set response %T", invs[callID].Args)
	}
	cr, ok := resp.Created["mk"]
	if !ok {
		t.Fatalf("create %s failed: %+v", name, resp.NotCreated)
	}
	t.Cleanup(func() { destroyMailbox(t, c, context.Background(), string(cr.ID)) })
	// The bridge refreshes roles after CREATE; refetch so the caller
	// sees the mailbox (and its role) it just made.
	mbs, err = c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes after create: %v", err)
	}
	for _, mb := range mbs.Mailboxes {
		if string(mb.ID) == string(cr.ID) {
			return string(mb.ID)
		}
	}
	return string(cr.ID)
}

// m2Seed creates two messages through the bridge's Email/set create
// (FR-M.11) and purges them afterwards.
func m2Seed(t *testing.T, c *Client, ctx context.Context, mailboxID, user string) []mail.ID {
	t.Helper()
	// Idempotent: a leftover from an earlier (or manual) run must not
	// satisfy a "message left this folder" assertion by accident.
	m2PurgeBySubject(t, c, ctx, mailboxID, m2GateSubjectA, m2GateSubjectB)
	now := time.Now().UTC()
	create := map[jmap.ID]*email.Email{
		"a": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(mailboxID): true},
			From:       []*jmapmail.Address{mkAddr("Gate", user)},
			To:         []*jmapmail.Address{mkAddr("Gate", user)},
			Subject:    m2GateSubjectA,
			ReceivedAt: &now,
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{"1": {Value: "M2 gate body one.\n"}},
		},
		"b": {
			MailboxIDs: map[jmap.ID]bool{jmap.ID(mailboxID): true},
			From:       []*jmapmail.Address{mkAddr("Gate", user)},
			To:         []*jmapmail.Address{mkAddr("Gate", user)},
			Subject:    m2GateSubjectB,
			ReceivedAt: &now,
			TextBody:   []*email.BodyPart{{PartID: "1", Type: "text/plain"}},
			BodyValues: map[string]*email.BodyValue{"1": {Value: "M2 gate body two.\n"}},
		},
	}
	req := &jmap.Request{Context: ctx}
	set := &email.Set{Account: jmap.ID(c.accountID), Create: create}
	callID := req.Invoke(set)
	invs, err := c.runBatch(ctx, req)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	view, ok := asEmailSetView(invs[callID])
	if !ok {
		t.Fatalf("unexpected Email/set response %T", invs[callID].Args)
	}
	if len(view.NotCreated) > 0 {
		t.Fatalf("seed failed: %+v", view.NotCreated)
	}
	ids := []mail.ID{view.CreatedIDs["a"], view.CreatedIDs["b"]}
	t.Cleanup(func() { purgeByIDs(t, c, context.Background(), ids...) })
	return ids
}

// m2PurgeBySubject destroys any message in mailboxID whose subject is
// one of the given gate subjects.
func m2PurgeBySubject(t *testing.T, c *Client, ctx context.Context, mailboxID string, subjects ...string) {
	t.Helper()
	qReq := &jmap.Request{Context: ctx}
	q := &email.Query{Account: jmap.ID(c.accountID), Limit: 500}
	q.Filter = &email.FilterCondition{InMailbox: jmap.ID(mailboxID)}
	qID := qReq.Invoke(q)
	qInvs, err := c.runBatch(ctx, qReq)
	if err != nil || len(qInvs[qID].Args.(*email.QueryResponse).IDs) == 0 {
		return
	}
	ids := qInvs[qID].Args.(*email.QueryResponse).IDs
	gReq := &jmap.Request{Context: ctx}
	g := &email.Get{Account: jmap.ID(c.accountID), IDs: ids, Properties: []string{"subject"}}
	gID := gReq.Invoke(g)
	gInvs, err := c.runBatch(ctx, gReq)
	if err != nil {
		return
	}
	gr, ok := gInvs[gID].Args.(*email.GetResponse)
	if !ok {
		return
	}
	var doomed []jmap.ID
	for _, e := range gr.List {
		for _, want := range subjects {
			if e.Subject == want {
				doomed = append(doomed, e.ID)
			}
		}
	}
	if len(doomed) == 0 {
		return
	}
	dReq := &jmap.Request{Context: ctx}
	dReq.Invoke(&email.Set{Account: jmap.ID(c.accountID), Destroy: doomed})
	if _, err := c.runBatch(ctx, dReq); err != nil {
		t.Logf("purge leftovers: %v", err)
	}
}

// TestLiveM2BridgeTriageRoundTrip is the M2 gate, jmap-tui side.
func TestLiveM2BridgeTriageRoundTrip(t *testing.T) {
	url, user, pass := liveCreds(t)
	c := New(Options{ServerURL: url, Username: user, Password: pass, Auth: liveAuth()})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The roles triage resolves: delete goes to trash, archive to
	// archive (the bridge derives both from the folder names).
	trashID := m2EnsureMailbox(t, c, ctx, "Trash")
	archiveID := m2EnsureMailbox(t, c, ctx, "Archive")
	gateID := m2EnsureMailbox(t, c, ctx, "m2-gate")
	destID := m2EnsureMailbox(t, c, ctx, "m2-dest")
	gateName, destName := "m2-gate", "m2-dest"

	// Seed first: the engine's plan works off its loaded summaries, so
	// messages created after OpenMailbox would be invisible to triage.
	ids := m2Seed(t, c, ctx, gateID, user)

	e := sync.NewEngine(c, sync.Config{})
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("LoadMailboxes: %v", err)
	}
	if err := e.OpenMailbox(ctx, mail.ID(gateID)); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	if snap := e.Snapshot(); len(snap.Rows) < 2 {
		t.Logf("snapshot: total=%d start=%d active=%q rows=%d",
			snap.Total, snap.Start, snap.ActiveMailbox, len(snap.Rows))
		t.Fatalf("gate rows = %d, want the two seeded messages", len(snap.Rows))
	}

	// --- read: one batched Email/set (FR-G3) ---
	rcpt, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageRead, IDs: ids})
	if err != nil {
		t.Fatalf("Triage(read): %v", err)
	}
	if len(rcpt.Failed) > 0 || rcpt.Err != "" {
		t.Fatalf("read triage failed: failed=%v err=%q applied=%v", rcpt.Failed, rcpt.Err, rcpt.Applied)
	}
	for _, id := range ids {
		kw, _, found := serverEmailState(t, c, ctx, id)
		if !found {
			t.Fatalf("email %s missing after read", id)
		}
		assertKeyword(t, kw, "$seen", true, "read")
	}
	t.Logf("read batch applied: %d ids", len(rcpt.Applied))

	// --- star, confirmed by an independent IMAP session, then undone ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageStar, IDs: ids})
	if err != nil {
		t.Fatalf("Triage(star): %v", err)
	}
	kw, _, _ := serverEmailState(t, c, ctx, ids[0])
	assertKeyword(t, kw, "$flagged", true, "star")
	m2IMAP(t, "flagged", gateName, m2GateSubjectA) // second client sees \Flagged
	t.Log("star visible to an independent IMAP client")

	if rcpt.Undo == nil {
		t.Fatal("star produced no undo spec")
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(star undo): %v", err)
	}
	kw, _, _ = serverEmailState(t, c, ctx, ids[0])
	assertKeyword(t, kw, "$flagged", false, "star undo")
	assertKeyword(t, kw, "$seen", true, "star undo keeps read state")
	m2IMAP(t, "unflagged", gateName, m2GateSubjectA)
	t.Log("star undo visible to an independent IMAP client")

	// --- move + undo ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: mail.ID(destID)})
	if err != nil {
		t.Fatalf("Triage(move): %v", err)
	}
	_, mbs, _ := serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(destID), true, "move")
	assertMembership(t, mbs, mail.ID(gateID), false, "move")
	m2IMAP(t, "in", destName, m2GateSubjectA)
	m2IMAP(t, "absent", gateName, m2GateSubjectA)
	t.Log("move visible to an independent IMAP client")

	if rcpt.Undo == nil {
		t.Fatal("move produced no undo spec")
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(move undo): %v", err)
	}
	_, mbs, _ = serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(gateID), true, "move undo")
	assertMembership(t, mbs, mail.ID(destID), false, "move undo")
	m2IMAP(t, "in", gateName, m2GateSubjectA)
	t.Log("move undo visible to an independent IMAP client")

	// --- copy + undo ---
	rcpt, err = e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageCopy, IDs: ids, Mailbox: mail.ID(destID)})
	if err != nil {
		t.Fatalf("Triage(copy): %v", err)
	}
	_, mbs, _ = serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(destID), true, "copy")
	assertMembership(t, mbs, mail.ID(gateID), true, "copy")
	m2IMAP(t, "in", destName, m2GateSubjectA)
	m2IMAP(t, "in", gateName, m2GateSubjectA)
	if rcpt.Undo == nil {
		t.Fatal("copy produced no undo spec")
	}
	if _, err := e.Triage(ctx, *rcpt.Undo); err != nil {
		t.Fatalf("Triage(copy undo): %v", err)
	}
	_, mbs, _ = serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(destID), false, "copy undo")
	m2IMAP(t, "absent", destName, m2GateSubjectA)
	t.Log("copy + undo visible to an independent IMAP client")

	// --- archive: the app resolves role-archive, here by hand ---
	if archiveID == "" {
		t.Fatal("no role-archive mailbox on the bridged account")
	}
	if _, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: mail.ID(archiveID)}); err != nil {
		t.Fatalf("Triage(archive): %v", err)
	}
	_, mbs, _ = serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(archiveID), true, "archive")
	m2IMAP(t, "in", "Archive", m2GateSubjectA)
	m2IMAP(t, "absent", gateName, m2GateSubjectA)
	t.Log("archive visible to an independent IMAP client")

	// --- delete = move to role-trash, then permanent destroy ---
	if trashID == "" {
		t.Fatal("no role-trash mailbox on the bridged account")
	}
	if _, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageMove, IDs: ids, Mailbox: mail.ID(trashID)}); err != nil {
		t.Fatalf("Triage(delete): %v", err)
	}
	_, mbs, _ = serverEmailState(t, c, ctx, ids[0])
	assertMembership(t, mbs, mail.ID(trashID), true, "delete")
	m2IMAP(t, "in", "Trash", m2GateSubjectA)
	t.Log("delete (move to trash) visible to an independent IMAP client")

	if _, err := e.Triage(ctx, sync.TriageSpec{Kind: sync.TriageDestroy, IDs: []mail.ID{ids[0]}}); err != nil {
		t.Fatalf("Triage(destroy): %v", err)
	}
	if _, _, found := serverEmailState(t, c, ctx, ids[0]); found {
		t.Fatal("destroyed message still resolvable through the bridge")
	}
	m2IMAP(t, "absent", "Trash", m2GateSubjectA)
	t.Log("destroy expunged on the backend and gone through the bridge")

	// The second message is cleaned up by m2Seed's purge.
}
