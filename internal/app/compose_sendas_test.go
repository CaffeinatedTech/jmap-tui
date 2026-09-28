package app

import (
	"context"
	"strings"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// issue #6: the composer's From could only ever name the account it
// opened on, so a multi-account user had no way to change the sending
// address. The picker spans every connected account; picking one moves
// the composer — draft and attachments included.

// sendAsTestModel wires two connected accounts, "work" (active) and
// "personal", each with Drafts and Sent and exactly one identity. Both
// servers deliberately serve the SAME identity id: JMAP ids are unique
// per account only (FR-A5), so the picker has to qualify them.
func sendAsTestModel(t *testing.T) (*Model, *mockjmap.Server, *mockjmap.Server) {
	t.Helper()
	shrinkComposeTimers(t)

	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 1, UnreadEmails: 1},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 3},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 4},
		{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 5},
		{ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 6},
	}
	srvWork := mockjmap.New("work@example.com", "pw", mailboxes)
	srvWork.SetIdentities([]mockjmap.Identity{{ID: "id-1", Name: "Work", Email: "work@example.test"}})
	srvWork.SetEmails([]mockjmap.Email{{
		ID: "e1", ThreadID: "t1", MailboxIDs: []string{"mb-inbox"},
		From:     []mockjmap.Address{{Name: "Alice", Email: "alice@example.test"}},
		Subject:  "thread starter",
		TextBody: "hello\n",
	}})
	t.Cleanup(srvWork.Close)
	srvPersonal := mockjmap.New("personal@example.com", "pw", mailboxes)
	srvPersonal.SetIdentities([]mockjmap.Identity{{ID: "id-1", Name: "Personal", Email: "personal@example.test"}})
	t.Cleanup(srvPersonal.Close)

	connect := func(srv *mockjmap.Server, user string) *jmapclient.Client {
		c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: user, Password: "pw"})
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
			{ID: "work", Name: "Work", Provider: connect(srvWork, "work@example.com"), Connected: true},
			{ID: "personal", Name: "Personal", Provider: connect(srvPersonal, "personal@example.com"), Connected: true},
		},
		Keys:  km,
		Theme: ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	loadAll(t, m)
	return m, srvWork, srvPersonal
}

// TestSendAsPickerSpansEveryAccount: with one identity per account the
// composer still offers a From choice, the rows name their account, and
// the current From is preselected.
func TestSendAsPickerSpansEveryAccount(t *testing.T) {
	m, _, _ := sendAsTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	c := m.compose
	if c == nil {
		t.Fatal("n did not open the composer")
	}
	if c.acct != "work" {
		t.Fatalf("acct = %q, want work (the active account)", c.acct)
	}
	if !m.canPickSendAs() {
		t.Fatal("ctrl+i offered no choice across two accounts")
	}

	m.composeKey(keyCtrl('i'))
	if m.picker == nil {
		t.Fatal("ctrl+i did not open the From picker")
	}
	if got := len(m.picker.all); got != 2 {
		t.Fatalf("picker rows = %d, want 2 (one identity per account)", got)
	}
	for _, it := range m.picker.all {
		if !strings.Contains(it.Label, " · ") {
			t.Errorf("row not account-qualified: %q", it.Label)
		}
	}
	if got := m.picker.all[m.picker.sel].Label; !strings.Contains(got, "work@example.test") {
		t.Errorf("preselected row = %q, want the current From", got)
	}

	// Pick the other account's identity (the second row).
	m.pickerKey("down")
	cmd, ok := m.pickerKey("enter")
	if !ok {
		t.Fatal("enter was not consumed by the picker")
	}
	pump(t, m, cmd)
	if m.picker != nil {
		t.Fatal("picker stayed open after enter")
	}
	if c.acct != "personal" {
		t.Errorf("acct = %q, want personal", c.acct)
	}
	if c.identity.Email != "personal@example.test" {
		t.Errorf("identity = %q, want personal@example.test", c.identity.Email)
	}
	if m.activeID != "work" {
		t.Errorf("activeID = %q — a From pick must not move the reader", m.activeID)
	}
	view := m.composeView()
	if !strings.Contains(view.From, "personal@example.test") || !strings.Contains(view.From, "· Personal") {
		t.Errorf("From row = %q, want the address and its account", view.From)
	}
	if !strings.Contains(stripANSI(view.Hint), "ctrl+i identity") {
		t.Errorf("hint = %q, want the picker offered", stripANSI(view.Hint))
	}
}

// TestSwitchSendAccountMovesTheDraft: a draft already saved on the old
// account follows the composer — destroyed there, recreated with the
// same content on the account now pinned.
func TestSwitchSendAccountMovesTheDraft(t *testing.T) {
	m, srvWork, srvPersonal := sendAsTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	c := m.compose
	c.to.SetValue("alice@example.test")
	c.subject.SetValue("cross account")
	pump(t, m, m.markDirty())
	if c.draftID == "" {
		t.Fatal("draft never saved on the first account")
	}
	if got := srvWork.CountIn("mb-drafts"); got != 1 {
		t.Fatalf("work drafts = %d, want 1", got)
	}

	pump(t, m, m.chooseIdentity(sendAsKey("personal", "id-1")))

	if c.acct != "personal" || c.identity.Email != "personal@example.test" {
		t.Fatalf("acct/identity = %q/%q", c.acct, c.identity.Email)
	}
	if c.draftID == "" {
		t.Fatal("no draft recreated on the new account")
	}
	if got := srvWork.CountIn("mb-drafts"); got != 0 {
		t.Errorf("old account still holds %d draft(s); the draft must follow", got)
	}
	if got := srvPersonal.CountIn("mb-drafts"); got != 1 {
		t.Errorf("new account drafts = %d, want 1", got)
	}
	if c.subject.Value() != "cross account" || c.to.Value() != "alice@example.test" {
		t.Errorf("content did not survive the move: to=%q subject=%q", c.to.Value(), c.subject.Value())
	}
	if strings.Contains(c.status, "failed") {
		t.Errorf("status = %q", c.status)
	}
}

// TestSwitchSendAccountReuploadsAttachments: an uploaded blob id belongs
// to the account it went up on, so switching restarts the transfer — and
// the draft that references it saves cleanly on the new account.
func TestSwitchSendAccountReuploadsAttachments(t *testing.T) {
	m, _, _ := sendAsTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)
	path := writeAttachFile(t)

	pump(t, m, m.uploadAttachmentCmd(path))
	a := m.compose.atts[0]
	if a.state != attReady || a.att.BlobID == "" {
		t.Fatalf("first upload: state=%v err=%q", a.state, a.err)
	}

	pump(t, m, m.chooseIdentity(sendAsKey("personal", "id-1")))

	if len(m.compose.atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(m.compose.atts))
	}
	a = m.compose.atts[0]
	if a.state != attReady || a.att.BlobID == "" {
		t.Fatalf("re-upload did not finish: state=%v err=%q", a.state, a.err)
	}
	if m.compose.draftID == "" {
		t.Fatal("draft carrying the attachment was not saved on the new account")
	}
	// The save validates the blob against the new account's blob set, so
	// a carried-over id from the old one would fail here.
	if strings.Contains(m.compose.status, "failed") {
		t.Errorf("status = %q", m.compose.status)
	}
}

// TestSendAsPickerNeedsAChoice: one account, one identity — nothing to
// pick, so neither the hint nor the picker appears.
func TestSendAsPickerNeedsAChoice(t *testing.T) {
	m, _ := composeTestModel(t)
	_, cmd := m.handleKey(key("n"))
	pump(t, m, cmd)

	if m.canPickSendAs() {
		t.Fatal("ctrl+i offered a choice with a single identity")
	}
	m.composeKey(keyCtrl('i'))
	if m.picker != nil {
		t.Fatal("picker opened with nothing to pick")
	}
	view := m.composeView()
	if strings.Contains(stripANSI(view.Hint), "ctrl+i") {
		t.Errorf("hint offers a picker that cannot open: %q", stripANSI(view.Hint))
	}
	if strings.Contains(view.From, "ctrl+i") {
		t.Errorf("From offers a picker that cannot open: %q", view.From)
	}
}

// TestSendAsPickerSwitchesWithinAccount: the original FR-H1 behaviour —
// several identities on one account — still switches the From without
// touching the account, and a single-account picker stays unqualified.
func TestSendAsPickerSwitchesWithinAccount(t *testing.T) {
	m := twoIdentityModel(t)
	_, _ = m.openCompose(composeNew)
	c := m.compose
	if c == nil || c.identity.Email != "first@example.test" {
		t.Fatalf("opening identity = %+v", c)
	}

	m.openIdentityPicker()
	if m.picker == nil || len(m.picker.all) != 2 {
		t.Fatalf("picker = %+v, want both identities", m.picker)
	}
	for _, it := range m.picker.all {
		if strings.Contains(it.Label, " · ") {
			t.Errorf("single-account row must not be account-qualified: %q", it.Label)
		}
	}
	pump(t, m, m.chooseIdentity(m.picker.all[1].ID))

	if c.identity.Email != "second@example.test" {
		t.Errorf("identity = %q, want second@example.test", c.identity.Email)
	}
	if c.acct != "a" {
		t.Errorf("acct = %q — a same-account pick must not move it", c.acct)
	}
}

// twoIdentityModel is one enrolled account whose server offers two
// identities (FR-A1 default unset: the first wins on open).
func twoIdentityModel(t *testing.T) *Model {
	t.Helper()
	shrinkComposeTimers(t)
	mailboxes := []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0},
		{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 5},
	}
	srv := mockjmap.New("tester@example.com", "pw", mailboxes)
	srv.SetIdentities([]mockjmap.Identity{
		{ID: "id-1", Name: "First", Email: "first@example.test"},
		{ID: "id-2", Name: "Second", Email: "second@example.test"},
	})
	t.Cleanup(srv.Close)
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "pw"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{
		Accounts: []AccountOpt{{ID: "a", Name: "A", Provider: c, Connected: true}},
		Keys:     km,
		Theme:    ui.NewTheme(ui.DarkTheme()),
	})
	m.width, m.height = 120, 40
	loadAll(t, m)
	return m
}
