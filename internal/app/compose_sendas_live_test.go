package app

import (
	"context"
	"testing"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// issue #6 live gate: the composer can change its sending account.
//
// Drafts only — no sends, no inbox mutation (AGENTS.md rules of
// engagement): one draft is written on the owning account, moved to the
// second account by a From pick, read back independently on both, and
// destroyed. Env-gated on the second account (unset ⇒ skip).
func TestLiveSwitchSendAccountGate(t *testing.T) {
	u1, r1, p1 := liveCreds(t)
	u2, r2, p2 := liveCredsWith(t, "_2")

	oldAuto, oldBlink := composeAutosaveDelay, runCursorBlink
	composeAutosaveDelay = 250 * time.Millisecond
	runCursorBlink = false
	t.Cleanup(func() {
		composeAutosaveDelay, runCursorBlink = oldAuto, oldBlink
	})

	connect := func(url, user, pass, auth string) *jmapclient.Client {
		c := jmapclient.New(jmapclient.Options{ServerURL: url, Username: user, Password: pass, Auth: auth})
		if err := c.Connect(context.Background()); err != nil {
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
	t.Cleanup(m.Cancel)
	loadAll(t, m)

	idents := map[string]mail.Identity{}
	for _, id := range []string{"one", "two"} {
		eng, ok := m.engineFor(id)
		if !ok {
			t.Fatalf("account %q not connected", id)
		}
		list := eng.Identities()
		if len(list) == 0 {
			t.Skipf("account %q publishes no sending identity; nothing to switch", id)
		}
		idents[id] = list[0]
	}
	creds := map[string]struct{ url, user, pass, auth string }{
		"one": {u1, r1, p1, liveAuth()},
		"two": {u2, r2, p2, liveAuthWith("_2")},
	}
	subject := "issue#6 gate " + time.Now().UTC().Format("20060102-150405")

	// Whatever draft id the composer holds, an aborting run still cleans
	// it up — on both accounts, since the draft moves mid-test.
	drafts := map[string]mail.ID{}
	t.Cleanup(func() {
		for acct, id := range drafts {
			if id == "" {
				continue
			}
			eng, ok := m.engineFor(acct)
			if !ok {
				continue
			}
			if _, err := eng.Triage(m.ctx, sync.TriageSpec{Kind: sync.TriageDestroy, IDs: []mail.ID{id}}); err != nil {
				t.Logf("cleanup: destroy %s on %s: %v", id, acct, err)
			}
		}
	})

	draftOn := func(acct string) bool {
		c2 := creds[acct]
		for id, sum := range freshReads(t, c2.url, c2.user, c2.pass, c2.auth, mail.RoleDrafts) {
			if sum.Subject == subject {
				t.Logf("%s holds draft %s", acct, id)
				return true
			}
		}
		return false
	}

	// Compose on the active account.
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	c := m.compose
	if c == nil {
		t.Fatal("n did not open the composer")
	}
	from, to := c.acct, "two"
	if from == "two" {
		to = "one"
	}
	composeType(t, m, ui.ZoneTo, creds[to].user)
	composeType(t, m, ui.ZoneSubject, subject)
	pump(t, m, m.saveDraftCmd())
	if c.draftID == "" {
		t.Fatalf("draft not saved on %s: %q", from, c.status)
	}
	drafts[from] = c.draftID
	if !draftOn(from) {
		t.Fatalf("draft missing from %s's Drafts", from)
	}
	if !m.canPickSendAs() {
		t.Fatal("no From choice offered across two accounts")
	}

	// Pick the other account's identity: the draft follows it.
	pump(t, m, m.chooseIdentity(sendAsKey(to, idents[to].ID)))
	if c.acct != to {
		t.Fatalf("acct = %q, want %q", c.acct, to)
	}
	if m.activeID != from {
		t.Errorf("activeID = %q — a From pick must not move the reader", m.activeID)
	}
	pump(t, m, m.saveDraftCmd())
	if c.draftID == "" {
		t.Fatalf("no draft on %s: %q", to, c.status)
	}
	drafts[from], drafts[to] = "", c.draftID

	if draftOn(from) {
		t.Errorf("the old copy is still in %s's Drafts", from)
	}
	if !draftOn(to) {
		t.Fatalf("the draft never reached %s's Drafts", to)
	}
	t.Logf("gate: draft moved %s → %s (subject %q)", from, to, subject)

	// Leave nothing behind.
	pump(t, m, m.closeComposer(true))
	drafts[to] = ""
	if draftOn(to) {
		t.Error("discarded draft left in Drafts")
	}
}
