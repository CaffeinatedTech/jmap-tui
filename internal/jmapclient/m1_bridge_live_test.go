package jmapclient

// The jmap-bridge M1 gate's jmap-tui half: a real account browses
// read-only through the bridge, and a flag flipped by an independent
// IMAP session appears in the running engine within 2s (PLAN §12 M1).
// Skips unless both the bridge creds (JMAP_TUI_TEST_*) and the
// flip-target Dovecot creds (JMAP_TUI_TEST_FLIP_*) are present.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

const m1GateSubject = "M1 gate target"

func flipCreds(t *testing.T) (host, port, user, pass string) {
	t.Helper()
	host = os.Getenv("JMAP_TUI_TEST_FLIP_HOST")
	user = os.Getenv("JMAP_TUI_TEST_FLIP_USER")
	pass = os.Getenv("JMAP_TUI_TEST_FLIP_PASSWORD")
	if host == "" || user == "" || pass == "" {
		t.Skip("flip-target IMAP creds not set (JMAP_TUI_TEST_FLIP_*)")
	}
	port = os.Getenv("JMAP_TUI_TEST_FLIP_PORT")
	if port == "" {
		port = "1143"
	}
	return host, port, user, pass
}

// imapAct runs one python3/imaplib operation against the flip-target
// server: "seed" appends a uniquely-subjected message, "flag" sets
// \Flagged on it. IMAP is deliberately outside jmap-tui's dependency
// set; the second client must be a genuinely independent one anyway.
func imapAct(t *testing.T, mode, host, port, user, pass, subject string) {
	t.Helper()
	script := `
import imaplib, sys
mode, host, port, user, pw, subject = sys.argv[1:7]
m = imaplib.IMAP4(host, int(port))
m.login(user, pw)
m.select("INBOX")
if mode == "seed":
    msg = ("From: gate@example.test\r\nTo: gate@example.test\r\n"
           "Subject: " + subject + "\r\n"
           "Message-ID: <" + subject + "@gate.test>\r\n"
           "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"
           "gate target body\r\n")
    typ, data = m.append("INBOX", None, None, msg.encode())
    if typ != "OK":
        sys.exit("append failed: " + str(data))
else:
    typ, data = m.uid("search", None, "HEADER", "Subject", chr(34) + subject + chr(34))
    if typ != "OK" or not data[0]:
        sys.exit("seeded message not found")
    uid = data[0].split()[-1]
    typ, data = m.uid("store", uid, "+FLAGS", "(\\Flagged)")
    if typ != "OK":
        sys.exit("store failed: " + str(data))
m.logout()
`
	cmd := exec.Command("python3", "-c", script, mode, host, port, user, pass, subject)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("imap %s via python3: %v (%s)", mode, err, out)
	}
}

// TestLiveM1BridgeBrowseAndForeignFlag drives the M1 gate statement:
// open the bridge's inbox in the engine (read-only browse: Mailbox/get,
// Email/query, Email/get all served from the SQLite cache), then flip a
// flag on the backend from a second IMAP client and require the engine
// snapshot to show it within two seconds over the bridge's SSE.
func TestLiveM1BridgeBrowseAndForeignFlag(t *testing.T) {
	url, user, pass := liveCreds(t)
	host, port, flipUser, flipPass := flipCreds(t)

	c := New(Options{ServerURL: url, Username: user, Password: pass, Auth: liveAuth()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

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
		t.Fatal("no inbox role on the bridged account")
	}
	if err := e.OpenMailbox(ctx, inboxID); err != nil {
		t.Fatalf("OpenMailbox: %v", err)
	}
	waitForLiveMode(t, e, 15*time.Second)

	// Seed the target through the backend; the bridge's IDLE must carry
	// it to this engine (FR-S.7 first half: new mail appears).
	imapAct(t, "seed", host, port, flipUser, flipPass, m1GateSubject)
	waitForSnap(t, 30*time.Second, func() bool {
		for _, r := range e.Snapshot().Rows {
			if r.Summary.Subject == m1GateSubject {
				return true
			}
		}
		return false
	})

	// The gate: flag flipped by the second client, budget two seconds.
	t0 := time.Now()
	imapAct(t, "flag", host, port, flipUser, flipPass, m1GateSubject)
	waitForSnap(t, 2*time.Second, func() bool {
		for _, r := range e.Snapshot().Rows {
			if r.Summary.Subject == m1GateSubject && r.Summary.Keywords.Has("$flagged") {
				return true
			}
		}
		return false
	})
	t.Logf("foreign flag visible in jmap-tui engine after %v (budget 2s)",
		time.Since(t0).Round(time.Millisecond))
}
