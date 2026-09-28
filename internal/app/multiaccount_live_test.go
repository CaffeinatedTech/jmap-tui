package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// M6 live gate (REQUIREMENTS §7): "two accounts configured; switch is
// instant; unified inbox interleaves correctly; actions never cross
// accounts."
//
// Env-gated on the second account (AGENTS.md: unset ⇒ skip, never
// hardcode, never echo). Mutations are limited to the two designated
// test accounts — approved 2026-09-25: read/unread toggles plus at most
// two fixture sends between the test addresses, all cleaned up here.

// liveCredsWith returns one account slot's creds: "" is the primary test
// account, "_2" the second (M6).
func liveCredsWith(t *testing.T, suffix string) (url, user, pass string) {
	t.Helper()
	url = os.Getenv("JMAP_TUI_TEST_URL" + suffix)
	user = os.Getenv("JMAP_TUI_TEST_USER" + suffix)
	pass = os.Getenv("JMAP_TUI_TEST_PASSWORD" + suffix)
	if url == "" || user == "" || pass == "" {
		t.Skipf("live creds for slot %q unset (JMAP_TUI_TEST_URL%s …)", suffix, suffix)
	}
	return url, user, pass
}

// liveAuthWith returns one account slot's Authorization scheme ("" =
// basic, "bearer" = API token; unset ⇒ basic — AGENTS.md env rules
// apply: never hardcode, never echo).
func liveAuthWith(suffix string) string { return os.Getenv("JMAP_TUI_TEST_AUTH" + suffix) }

func liveAuth() string { return liveAuthWith("") }

// freshReads builds a brand-new client + engine for one account and reads
// a role mailbox — the independent verification path (the gate model's
// caches prove nothing about the server).
func freshReads(t *testing.T, url, user, pass, auth string, role mail.Role) map[mail.ID]mail.EmailSummary {
	t.Helper()
	ctx := context.Background()
	c := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: auth})
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("independent connect: %v", err)
	}
	e := sync.NewEngine(c, sync.Config{})
	if err := e.LoadMailboxes(ctx); err != nil {
		t.Fatalf("independent LoadMailboxes: %v", err)
	}
	inbox := mail.ID("")
	for _, mb := range e.Snapshot().Mailboxes {
		if mb.Mailbox.Role == role {
			inbox = mb.Mailbox.ID
			break
		}
	}
	if inbox == "" {
		t.Fatalf("no %s mailbox on the server", role)
	}
	if err := e.OpenMailbox(ctx, inbox); err != nil {
		t.Fatalf("independent OpenMailbox: %v", err)
	}
	out := map[mail.ID]mail.EmailSummary{}
	for _, r := range e.Snapshot().Rows {
		out[r.ID] = r.Summary
	}
	return out
}

// sendFixture sends one tiny message between the test addresses and
// returns its subject (unique, for later cleanup).
func sendFixture(t *testing.T, m *Model, from, toAddr, subject string) {
	t.Helper()
	eng, ok := m.engineFor(from)
	if !ok {
		t.Fatalf("sender %q not connected", from)
	}
	idents := eng.Identities()
	if len(idents) == 0 {
		t.Fatalf("no sending identity on %q", from)
	}
	d := mail.Draft{
		IdentityID: idents[0].ID,
		From:       []mail.Address{{Name: idents[0].Name, Email: idents[0].Email}},
		To:         []mail.Address{{Email: toAddr}},
		Subject:    subject,
		Text:       "jmap-tui M6 gate fixture — cleaned up by the test.\n",
	}
	if _, _, err := eng.Send(m.ctx, d); err != nil {
		t.Fatalf("send from %s: %v", from, err)
	}
}

// waitForInboxSubject re-opens an account's inbox (no push loop in the
// headless gate) until subject shows up, or fails after 30s.
func waitForInboxSubject(t *testing.T, m *Model, acct, subject string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pump(t, m, m.opOn(acct, "reopen", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if err := eng.OpenMailbox(ctx, inboxID(m.snaps[acct].Mailboxes)); err != nil {
				return sync.Snapshot{}, err
			}
			return eng.Snapshot(), nil
		}))
		for _, r := range m.snaps[acct].Rows {
			if r.Summary.Subject == subject {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s never received %q", acct, subject)
}

// destroyBySubject removes every message with the subject from a role
// mailbox on one account (fixture cleanup).
func destroyBySubject(t *testing.T, m *Model, acct string, role mail.Role, subject string) {
	t.Helper()
	// Re-read the role mailbox on the gate engine.
	var ids []mail.ID
	box := mail.ID("")
	for _, mb := range m.snaps[acct].Mailboxes {
		if mb.Mailbox.Role == role {
			box = mb.Mailbox.ID
		}
	}
	if box == "" {
		return
	}
	pump(t, m, m.opOn(acct, "cleanup-read", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if err := eng.OpenMailbox(ctx, box); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	}))
	for _, r := range m.snaps[acct].Rows {
		if r.Summary.Subject == subject {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	pump(t, m, m.opOn(acct, "cleanup-destroy", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
		if _, err := eng.Triage(ctx, sync.TriageSpec{Kind: sync.TriageDestroy, IDs: ids}); err != nil {
			return sync.Snapshot{}, err
		}
		return eng.Snapshot(), nil
	}))
}

func TestLiveM6TwoAccountGate(t *testing.T) {
	u1, r1, p1 := liveCreds(t)
	u2, r2, p2 := liveCredsWith(t, "_2")

	connect := func(url, user, pass, auth string) *jmapclient.Client {
		c := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: auth})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		return c
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{
			{ID: "one", Name: "One", Provider: connect(u1, r1, p1, liveAuth()), Connected: true},
			{ID: "two", Name: "Two", Provider: connect(u2, r2, p2, liveAuthWith("_2")), Connected: true},
		},
		Keys:  km,
		Theme: ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	loadAll(t, m)

	// Gate 1: two accounts configured, each with a warm inbox window.
	for _, id := range []string{"one", "two"} {
		s := m.snaps[id]
		if len(s.Mailboxes) == 0 || s.ActiveMailbox == "" {
			t.Fatalf("account %q not loaded (mailboxes=%d active=%q)",
				id, len(s.Mailboxes), s.ActiveMailbox)
		}
	}
	t.Logf("gate: two accounts loaded (one: %d inbox rows, two: %d inbox rows)",
		len(m.snaps["one"].Rows), len(m.snaps["two"].Rows))

	// Gate 2: switching is instant — the stored snapshot renders with no
	// network in between (FR-A4).
	start := time.Now()
	_, cmd := m.switchAccount("two")
	switchDur := time.Since(start)
	if switchDur > 500*time.Millisecond {
		t.Errorf("switch took %s — must be instant", switchDur)
	}
	if len(m.snap.Mailboxes) == 0 || m.snap.ActiveMailbox == "" {
		t.Fatal("switch did not render the stored view immediately (re-fetch?)")
	}
	pump(t, m, cmd)
	if m.activeID != "two" {
		t.Fatalf("active after switch = %q, want two", m.activeID)
	}
	start = time.Now()
	_, cmd = m.switchAccount("one")
	switchBack := time.Since(start)
	pump(t, m, cmd)
	if m.activeID != "one" {
		t.Fatalf("active after switch back = %q, want one", m.activeID)
	}
	t.Logf("gate: switch instant (%s / %s)", switchDur, switchBack)

	// Populate any empty inbox so the interleave is real — at most two
	// fixture sends between the test addresses (approved), each tracked
	// for cleanup.
	mark := time.Now().UTC().Format("20060102-150405")
	subjects := map[string]bool{}
	if len(m.snaps["two"].Rows) == 0 {
		sub := fmt.Sprintf("jmap-tui M6 gate %s → two", mark)
		sendFixture(t, m, "one", r2, sub)
		subjects[sub] = true
		waitForInboxSubject(t, m, "two", sub)
	}
	if len(m.snaps["one"].Rows) == 0 {
		sub := fmt.Sprintf("jmap-tui M6 gate %s → one", mark)
		sendFixture(t, m, "two", r1, sub)
		subjects[sub] = true
		waitForInboxSubject(t, m, "one", sub)
	}
	// Sweep the fixtures even when a mid-gate assertion aborts the run:
	// debris would otherwise trip the leftover check in every later gate
	// (the inline cleanup below only runs on the happy path).
	t.Cleanup(func() {
		for sub := range subjects {
			for _, acct := range []string{"one", "two"} {
				for _, role := range []mail.Role{mail.RoleInbox, mail.RoleSent} {
					destroyBySubject(t, m, acct, role, sub)
				}
			}
		}
	})

	// Gate 3: the unified inbox interleaves correctly (FR-A5).
	_, cmd = m.handleKey(key("i"))
	pump(t, m, cmd)
	if !m.unified {
		t.Fatal("unified view did not open")
	}
	if len(m.snap.Rows) < 2 {
		t.Fatalf("merged rows = %d, want at least 2", len(m.snap.Rows))
	}
	owners := map[string]bool{}
	for i, r := range m.snap.Rows {
		owners[r.Account] = true
		if i > 0 && r.Summary.ReceivedAt.After(m.snap.Rows[i-1].Summary.ReceivedAt) {
			t.Fatalf("row %d (%s) newer than row %d — not interleaved", i, r.Account, i-1)
		}
	}
	if !owners["one"] || !owners["two"] {
		t.Fatalf("merged owners = %v, want both accounts represented", owners)
	}
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "unified inbox") {
		t.Fatalf("view missing unified header:\n%s", view)
	}
	// Ownership renders as the row colour bar plus the preview's text
	// line — the old per-row name badges are gone (FR-A5, M7 gate).
	if !strings.Contains(view, "Account: One") && !strings.Contains(view, "Account: Two") {
		t.Fatalf("view missing the preview owner line:\n%s", view)
	}
	if !strings.Contains(m.View().Content, "48;2;") {
		t.Fatal("unified rows missing owner colour bars")
	}
	t.Logf("gate: unified interleaved %d rows across both accounts", len(m.snap.Rows))

	// Gate 4: actions never cross accounts — toggle read on the cursor
	// row and prove only the owner's server changed (independent reads).
	owner, row0, ok := m.cursorRef()
	if !ok {
		t.Fatal("no cursor row in unified view")
	}
	other := "two"
	if owner == "two" {
		other = "one"
	}
	var otherRow sync.Row
	for _, r := range m.snap.Rows {
		if r.Account == other {
			otherRow = r
			break
		}
	}
	if otherRow.ID == "" {
		t.Fatalf("no row owned by %s in the merged view", other)
	}
	creds := map[string]struct{ url, user, pass, auth string }{
		"one": {u1, r1, p1, liveAuth()}, "two": {u2, r2, p2, liveAuthWith("_2")},
	}
	readSeen := func(acct string, id mail.ID) (bool, bool) {
		sums := freshReads(t, creds[acct].url, creds[acct].user, creds[acct].pass, creds[acct].auth, mail.RoleInbox)
		s, found := sums[id]
		return s.Keywords.Has("$seen"), found
	}
	ownerBefore, ownerFound := readSeen(owner, row0.ID)
	if !ownerFound {
		t.Fatalf("%s row %s not on the server", owner, row0.ID)
	}
	otherBefore, otherFound := readSeen(other, otherRow.ID)
	if !otherFound {
		t.Fatalf("%s row %s not on the server", other, otherRow.ID)
	}

	_, cmd = m.handleKey(key("u"))
	pump(t, m, cmd)

	ownerAfter, _ := readSeen(owner, row0.ID)
	if ownerAfter == ownerBefore {
		t.Fatalf("%s row %s: $seen stayed %v — the action did not apply", owner, row0.ID, ownerBefore)
	}
	otherAfter, _ := readSeen(other, otherRow.ID)
	if otherAfter != otherBefore {
		t.Fatalf("%s row %s changed ($seen %v → %v) — action crossed accounts",
			other, otherRow.ID, otherBefore, otherAfter)
	}
	t.Logf("gate: action routed to %s only ($seen %v → %v; %s untouched)",
		owner, ownerBefore, ownerAfter, other)

	// Cleanup: toggle back, then sweep every fixture subject out of both
	// accounts (Sent copy + recipient inbox; absent rows are a no-op).
	_, cmd = m.handleKey(key("u"))
	pump(t, m, cmd)
	ownerRestored, _ := readSeen(owner, row0.ID)
	if ownerRestored != ownerBefore {
		t.Errorf("%s row %s not restored: $seen %v, want %v", owner, row0.ID, ownerRestored, ownerBefore)
	}
	for _, sub := range sortedKeys(subjects) {
		for _, acct := range []string{"one", "two"} {
			for _, role := range []mail.Role{mail.RoleInbox, mail.RoleSent} {
				destroyBySubject(t, m, acct, role, sub)
			}
		}
	}
	// Prove the fixtures are gone from the servers (AGENTS.md cleanup
	// rule) — independent reads, both accounts, both roles.
	for _, acct := range []string{"one", "two"} {
		c := creds[acct]
		for _, role := range []mail.Role{mail.RoleInbox, mail.RoleSent} {
			for id, s := range freshReads(t, c.url, c.user, c.pass, c.auth, role) {
				if strings.HasPrefix(s.Subject, "jmap-tui M6 gate") {
					t.Errorf("fixture left behind: %s/%s %s (%s)", acct, role, id, s.Subject)
				}
			}
		}
	}
	if m.err != "" {
		t.Errorf("model error after the gate: %s", m.err)
	}
	t.Log("gate: M6 passed — two accounts, instant switch, interleaved unified inbox, owner-routed action")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
