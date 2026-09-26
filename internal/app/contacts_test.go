package app

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
	"github.com/CaffeinatedTech/jmap-tui/test/mockjmap"
)

// newTestModelAgainst builds a fresh model against an already-running
// mock (so capability changes made after boot are picked up).
func newTestModelAgainst(t *testing.T, srv *mockjmap.Server) *Model {
	t.Helper()
	c := jmapclient.New(jmapclient.Options{ServerURL: srv.URL(), Username: "tester@example.com", Password: "correct-horse"})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	km, err := ui.NewKeyMap(nil)
	if err != nil {
		t.Fatalf("KeyMap: %v", err)
	}
	m := New(Options{Provider: c, Keys: km, Theme: ui.NewTheme(ui.DarkTheme())})
	m.width, m.height = 120, 40
	return m
}

// contactsBoot wires a compose-ready test model (draft roles + shrunk
// timers, composeTestModel's setup) and seeds three contacts before any
// contact load, so the first ensure picks them up.
func contactsBoot(t *testing.T, extra ...mockjmap.Contact) (*Model, *mockjmap.Server) {
	t.Helper()
	oldAuto, oldTick, oldBlink := composeAutosaveDelay, uploadProgressTick, runCursorBlink
	composeAutosaveDelay = time.Millisecond
	uploadProgressTick = time.Millisecond
	runCursorBlink = false
	t.Cleanup(func() {
		composeAutosaveDelay, uploadProgressTick, runCursorBlink = oldAuto, oldTick, oldBlink
	})

	m, srv := newTestModelWith(t, []mockjmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 0, TotalEmails: 2, UnreadEmails: 1},
		{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 3},
		{ID: "mb-archive", Name: "Archive", Role: "archive", SortOrder: 4},
		{ID: "mb-drafts", Name: "Drafts", Role: "drafts", SortOrder: 5},
		{ID: "mb-sent", Name: "Sent", Role: "sent", SortOrder: 6},
	})
	_, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m, cmd)
	}
	pump(t, m, m.loadAccountCmd())
	seeds := []mockjmap.Contact{
		{
			ID: "ct1", Given: "Ada", Surname: "Lovelace", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "ada@example.com", Label: "work"}},
		},
		{
			ID: "ct2", Given: "Alan", Surname: "Turing", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "alan@example.com"}},
		},
		{
			ID: "ct3", Given: "Zoe", Surname: "Zebra", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "zoe@example.com"}},
		},
	}
	srv.SetContacts(append(seeds, extra...))
	return m, srv
}

func press(t *testing.T, m *Model, msg tea.KeyPressMsg) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(msg)
	return cmd
}

// contactExists reports whether the active account's store holds id.
func contactExists(m *Model, id string) bool {
	for _, c := range m.contactSnaps[m.activeID].Contacts {
		if string(c.ID) == id {
			return true
		}
	}
	return false
}

// TestContactsScreenOpensAndCloses: c toggles the full-screen view (FR-L1).
func TestContactsScreenOpensAndCloses(t *testing.T) {
	m, _ := contactsBoot(t)
	if m.contacts != nil {
		t.Fatal("screen starts closed")
	}
	pump(t, m, press(t, m, key("c")))
	if m.contacts == nil {
		t.Fatal("c did not open the contacts screen")
	}
	pump(t, m, press(t, m, key("c")))
	if m.contacts != nil {
		t.Fatal("c did not close the contacts screen")
	}
}

// TestContactsScreenListsAndFilters: rows land name-sorted, the detail
// follows the selection, and / narrows with the esc chain (filter stop →
// clear → close).
func TestContactsScreenListsAndFilters(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("c")))

	if len(m.contacts.rows) != 3 {
		t.Fatalf("rows = %d, want 3 (%+v)", len(m.contacts.rows), m.contacts.rows)
	}
	if m.contacts.rows[0].Name != "Ada Lovelace" {
		t.Fatalf("first row = %+v, want Ada Lovelace", m.contacts.rows[0])
	}
	if m.contacts.detail == nil || m.contacts.detail.Name != "Ada Lovelace" {
		t.Fatalf("detail = %+v", m.contacts.detail)
	}
	if len(m.contacts.detail.Emails) != 1 || m.contacts.detail.Emails[0] != "work: ada@example.com" {
		t.Fatalf("detail emails = %+v (label should survive)", m.contacts.detail.Emails)
	}

	pump(t, m, press(t, m, key("/")))
	if !m.contacts.filtering {
		t.Fatal("/ did not start filtering")
	}
	pump(t, m, press(t, m, key("z")))
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(m.contacts.rows) != 1 || m.contacts.rows[0].Name != "Zoe Zebra" {
		t.Fatalf("filtered rows = %+v", m.contacts.rows)
	}
	// esc chain: first drops the filter, second closes the screen.
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEsc}))
	if m.contacts == nil {
		t.Fatal("first esc closed the screen instead of clearing the filter")
	}
	if m.contacts.filter != "" {
		t.Fatalf("first esc should clear the filter, got %q", m.contacts.filter)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEsc}))
	if m.contacts != nil {
		t.Fatal("second esc should close the screen")
	}
}

// TestContactsScreenDeleteHold: d holds the destroy behind the undo
// toast; ctrl+z cancels the never-sent delete, while the commit lands it
// (FR-L2 — destroy is irreversible, so it waits out the window).
func TestContactsScreenDeleteHold(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("c")))

	// tab cycles columns: list → detail → books → list.
	if m.contacts.focus != ui.ContactList {
		t.Fatalf("focus = %v, want list", m.contacts.focus)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyTab}))
	if m.contacts.focus != ui.ContactDetail {
		t.Fatalf("focus = %v, want detail", m.contacts.focus)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyTab}))
	if m.contacts.focus != ui.ContactBooks {
		t.Fatalf("focus = %v, want books", m.contacts.focus)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyTab}))

	target := string(m.contacts.rowIDs[0])
	// The hold's commit tick is deliberately not pumped: the test drives
	// the commit by hand, exactly as time would.
	_, _ = m.handleKey(key("d"))
	if m.pendingContactDelete == nil || string(m.pendingContactDelete.id) != target {
		t.Fatalf("delete not held: %+v", m.pendingContactDelete)
	}
	if m.toast == nil || m.toast.hint != "ctrl+z cancel" {
		t.Fatalf("toast = %+v", m.toast)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}))
	if m.pendingContactDelete != nil {
		t.Fatal("ctrl+z did not cancel the held delete")
	}
	if !contactExists(m, target) {
		t.Fatal("cancelled delete removed the contact")
	}

	_, _ = m.handleKey(key("d"))
	seq := m.pendingContactDelete.seq
	pump(t, m, func() tea.Msg { return contactDeleteCommitMsg{seq: seq} })
	if m.pendingContactDelete != nil {
		t.Fatal("commit did not clear the hold")
	}
	if contactExists(m, target) {
		t.Fatal("committed delete left the contact in the store")
	}
	if m.toast == nil || !strings.HasPrefix(m.toast.text, "Deleted") {
		t.Fatalf("receipt toast = %+v", m.toast)
	}
}

// TestContactFormCreateRoundTrip: the blank form (n on the screen) saves
// through ContactCard/set into the default book (FR-L2).
func TestContactFormCreateRoundTrip(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("c")))
	pump(t, m, press(t, m, key("n")))
	if m.contactForm == nil {
		t.Fatal("n did not open the form")
	}
	if m.contactForm.editID != "" {
		t.Fatal("screen n must open a blank create form")
	}
	m.contactForm.set(0, "Grace")
	m.contactForm.set(1, "Hopper")
	m.contactForm.set(2, "grace@example.com")
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))

	if m.contactForm != nil {
		t.Fatalf("form stayed open: %+v", m.contactForm)
	}
	if m.toast == nil || m.toast.text != "Contact added" {
		t.Fatalf("toast = %+v", m.toast)
	}
	var created bool
	for _, c := range m.contactSnaps[m.activeID].Contacts {
		if c.DisplayName == "Grace Hopper" {
			created = true
			if len(c.AddressBookIDs) == 0 {
				t.Fatal("create did not resolve the default address book")
			}
			if len(c.Emails) != 1 || c.Emails[0].Address != "grace@example.com" {
				t.Fatalf("emails = %+v", c.Emails)
			}
		}
	}
	if !created {
		t.Fatalf("created contact missing: %+v", m.contactSnaps[m.activeID].Contacts)
	}
}

// TestContactFormValidation: an empty form refuses with an inline error
// and esc closes without saving (FR-L2).
func TestContactFormValidation(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("c")))
	pump(t, m, press(t, m, key("n")))
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if m.contactForm == nil || m.contactForm.err == "" {
		t.Fatal("empty form must refuse with an inline error")
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEsc}))
	if m.contactForm != nil {
		t.Fatal("esc did not close the form")
	}
}

// TestContactEditRoundTrip: e prefills the form from the selected card
// and an unchanged save sends nothing (the minimal-patch rule).
func TestContactEditRoundTrip(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("c")))
	pump(t, m, press(t, m, key("e")))
	if m.contactForm == nil || m.contactForm.editID == "" {
		t.Fatalf("form = %+v", m.contactForm)
	}
	if m.contactForm.value(0) != "Ada" || m.contactForm.value(2) != "ada@example.com" {
		t.Fatalf("prefill = %q / %q", m.contactForm.value(0), m.contactForm.value(2))
	}
	// Change the title, save, verify it lands.
	m.contactForm.set(5, "Countess")
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if m.contactForm != nil {
		t.Fatalf("form stayed open: %+v", m.contactForm.err)
	}
	for _, c := range m.contactSnaps[m.activeID].Contacts {
		if string(c.ID) == "ct1" {
			if len(c.Titles) != 1 || c.Titles[0].Value != "Countess" {
				t.Fatalf("titles = %+v", c.Titles)
			}
			if len(c.Emails) != 1 || c.Emails[0].Label != "work" {
				t.Fatalf("label lost on unrelated edit: %+v", c.Emails)
			}
		}
	}
}

// TestContactAddSenderPrefills: shift+n on a message prefills name and
// email from its sender (FR-L4). The fixture's newest row is from Bob,
// who is not in the address book.
func TestContactAddSenderPrefills(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, m.ensureContacts()) // warm, bob still unknown
	pump(t, m, press(t, m, keyShift('N')))
	if m.contactForm == nil {
		t.Fatal("form did not open")
	}
	if m.contactForm.editID != "" {
		t.Fatal("unknown sender must open a blank create, not edit")
	}
	if m.contactForm.value(0) != "Bob" {
		t.Fatalf("first = %q, want Bob", m.contactForm.value(0))
	}
	if m.contactForm.value(2) != "bob@example.test" {
		t.Fatalf("email = %q, want bob@example.test", m.contactForm.value(2))
	}
}

// TestContactAddSenderOpensEditWhenKnown: a sender the address book
// already holds opens edit mode instead of a duplicate (FR-L4).
func TestContactAddSenderOpensEditWhenKnown(t *testing.T) {
	m, srv := contactsBoot(t, mockjmap.Contact{
		ID: "ctBob", Given: "Bob", Surname: "Builder", AddressBookIDs: []string{"ab1"},
		Emails: []mockjmap.ContactValue{{Value: "bob@example.test"}},
	})
	pump(t, m, m.ensureContacts())
	pump(t, m, press(t, m, keyShift('N')))
	if m.contactForm == nil || m.contactForm.editID != "ctBob" {
		t.Fatalf("form = %+v (want edit ctBob)", m.contactForm)
	}
	if srv == nil {
		t.Fatal("unreachable")
	}
}

// TestComposeSuggestPopup: typing in To pops the dropdown, enter inserts
// the highlighted address, a no-match token pops nothing (FR-L3).
func TestComposeSuggestPopup(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("n"))) // compose; warms contacts
	if m.compose == nil || m.compose.focus != ui.ZoneTo {
		t.Fatalf("compose = %+v focus %v", m.compose, func() ui.ComposeZone {
			if m.compose != nil {
				return m.compose.focus
			}
			return ui.ZoneBody
		}())
	}

	for _, r := range "ad" {
		pump(t, m, press(t, m, tea.KeyPressMsg{Text: string(r), Code: r}))
	}
	s := m.compose.suggest
	if s == nil || len(s.items) == 0 {
		t.Fatalf("no suggestions for %q: %+v", "ad", s)
	}
	if s.items[0].Name != "Ada Lovelace" {
		t.Fatalf("first suggestion = %+v, want Ada", s.items[0])
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))
	got := m.compose.to.Value()
	if !strings.Contains(got, "Ada Lovelace <ada@example.com>") {
		t.Fatalf("insert = %q", got)
	}
	if m.compose.suggest != nil {
		t.Fatal("accept must close the popup")
	}

	// A token nobody matches pops nothing.
	m.compose.to.SetValue("zzz")
	m.compose.to.SetCursor(3)
	m.updateComposeSuggest()
	if m.compose.suggest != nil {
		t.Fatal("no-match token must not pop")
	}

	// esc dismisses an open popup without leaving the composer.
	m.compose.to.SetValue("al")
	m.compose.to.SetCursor(2)
	m.updateComposeSuggest()
	if m.compose.suggest == nil || len(m.compose.suggest.items) == 0 {
		t.Fatal("expected suggestions for al")
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEsc}))
	if m.compose != nil && m.compose.suggest != nil {
		t.Fatal("esc did not dismiss the popup")
	}
	if !m.compose.suggestOff {
		t.Fatal("dismiss must latch until the next edit")
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Text: "a", Code: 'a'}))
	if m.compose.suggest == nil {
		t.Fatal("a fresh keystroke must reopen the popup")
	}
}

// TestComposeQuickPickCtrlG: ctrl+g opens the type-to-filter picker over
// the merged set and enter inserts the pick (FR-L3).
func TestComposeQuickPickCtrlG(t *testing.T) {
	m, _ := contactsBoot(t)
	pump(t, m, press(t, m, key("n")))
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}))
	if m.picker == nil || m.picker.mode != pickerContactQuick {
		t.Fatalf("picker = %+v", m.picker)
	}
	if len(m.picker.all) != 3 {
		t.Fatalf("candidates = %d, want 3", len(m.picker.all))
	}
	// Type-to-filter, then pick.
	pump(t, m, press(t, m, key("z")))
	if len(m.picker.items) != 1 {
		t.Fatalf("filtered items = %+v", m.picker.items)
	}
	pump(t, m, press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if m.picker != nil {
		t.Fatal("pick did not close the picker")
	}
	if !strings.Contains(m.compose.to.Value(), "zoe@example.com") {
		t.Fatalf("insert = %q", m.compose.to.Value())
	}
}

// TestMatchContactsRanks: prefix beats substring, limit caps, empty
// token matches nothing (the popup's core rule).
func TestMatchContactsRanks(t *testing.T) {
	cands := []suggestCandidate{
		{Name: "Adam West", Email: "adam@example.com"},
		{Name: "Madam Queen", Email: "queen@example.com"},
		{Name: "Alan Turing", Email: "alan@example.com"},
	}
	got := matchContacts(cands, "ad", 5)
	// Adam West is a prefix hit; Madam Queen contains "ad" — prefix
	// first, substring after.
	if len(got) != 2 || got[0].Name != "Adam West" || got[1].Name != "Madam Queen" {
		t.Fatalf("prefix/substring rank = %+v", got)
	}
	got = matchContacts(cands, "a", 4)
	if len(got) != 3 || got[0].Name != "Adam West" || got[1].Name != "Alan Turing" {
		t.Fatalf("rank for a = %+v", got)
	}
	if matchContacts(cands, "", 5) != nil {
		t.Fatal("empty token must match nothing")
	}
	if len(matchContacts(cands, "a", 2)) != 2 {
		t.Fatal("limit must cap the popup")
	}
}

// TestContactsUnsupportedShowsError: a server without the capability
// keeps the screen closed with a non-fatal message (FR-L6).
func TestContactsUnsupportedShowsError(t *testing.T) {
	_, srv := contactsBoot(t)
	srv.DisableContacts()
	// Rebuild the model against the capability-less session.
	m2 := newTestModelAgainst(t, srv)
	_, cmd := m2.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m2, cmd)
	}
	pump(t, m2, m2.loadAccountCmd())
	pump(t, m2, press(t, m2, key("c")))
	if m2.contacts != nil {
		t.Fatal("screen must not open without the capability")
	}
	if !strings.Contains(m2.err, "contacts support") {
		t.Fatalf("err = %q", m2.err)
	}
}

// TestContactsUnifiedAcrossAccounts: two supporting accounts open as one
// scope with owner-qualified rows and both book trees, and the merged
// suggestion source dedupes an address both accounts hold (FR-L1, FR-L3).
func TestContactsUnifiedAcrossAccounts(t *testing.T) {
	m, srvW, srvP := newTwoAccountModel(t)
	loadAll(t, m)
	srvW.SetContacts([]mockjmap.Contact{
		{
			ID: "w1", Given: "Ada", Surname: "Lovelace", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "ada@work.example"}},
		},
		{
			ID: "w2", Given: "Shared", Surname: "Person", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "shared@example.com"}},
		},
	})
	srvP.SetContacts([]mockjmap.Contact{
		{
			ID: "p1", Given: "Grace", Surname: "Hopper", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "grace@personal.example"}},
		},
		{
			ID: "p2", Given: "Shared", Surname: "Person", AddressBookIDs: []string{"ab1"},
			Emails: []mockjmap.ContactValue{{Value: "shared@example.com"}},
		},
	})

	pump(t, m, press(t, m, key("c")))
	if m.contacts == nil {
		t.Fatal("screen did not open")
	}
	if m.contacts.acct != "" {
		t.Fatalf("scope = %q, want unified (two supporting accounts)", m.contacts.acct)
	}
	if len(m.contacts.rows) != 4 {
		t.Fatalf("rows = %d (%+v), want 4", len(m.contacts.rows), m.contacts.rows)
	}
	owners := map[string]bool{}
	headers := 0
	for _, r := range m.contacts.rows {
		owners[r.Account] = true
	}
	for _, b := range m.contacts.books {
		if b.Depth == 0 {
			headers++
		}
	}
	if len(owners) != 2 || !owners["work"] || !owners["personal"] {
		t.Fatalf("owners = %+v, want both accounts", owners)
	}
	if headers != 2 {
		t.Fatalf("book headers = %d, want 2", headers)
	}
	// The detail column resolves through the row's owner.
	m.contacts.focus = ui.ContactList
	idx := -1
	for i, r := range m.contacts.rows {
		if r.Name == "Grace Hopper" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("personal contact missing from the merge")
	}
	m.contacts.rowSel = idx
	m.contacts.rowKey = m.contacts.rowAccts[idx] + "\x00" + string(m.contacts.rowIDs[idx])
	m.rebuildContacts()
	if m.contacts.detail == nil || m.contacts.detail.Account != "Personal" {
		t.Fatalf("detail = %+v, want the personal card", m.contacts.detail)
	}

	// Merged suggestions: the shared address dedupes to one candidate.
	seen := 0
	for _, c := range m.contactCandidates() {
		if c.Email == "shared@example.com" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("shared address candidates = %d, want 1", seen)
	}
	if len(m.contactCandidates()) != 3 {
		t.Fatalf("candidates = %d, want 3 unique addresses", len(m.contactCandidates()))
	}
}

// TestComposeSuggestAbsentWithoutCapability: on a server without contacts
// the popup never appears — not even a loading row (FR-L6).
func TestComposeSuggestAbsentWithoutCapability(t *testing.T) {
	_, srv := contactsBoot(t)
	srv.DisableContacts()
	m2 := newTestModelAgainst(t, srv)
	_, cmd := m2.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if cmd != nil {
		pump(t, m2, cmd)
	}
	pump(t, m2, m2.loadAccountCmd())
	pump(t, m2, press(t, m2, key("n")))
	for _, r := range "al" {
		pump(t, m2, press(t, m2, tea.KeyPressMsg{Text: string(r), Code: r}))
	}
	if m2.compose == nil {
		t.Fatal("composer did not open")
	}
	if m2.compose.suggest != nil {
		t.Fatalf("popup = %+v, want nil without the capability", m2.compose.suggest)
	}
	// ctrl+g must not open an empty picker either.
	pump(t, m2, press(t, m2, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}))
	if m2.picker != nil {
		t.Fatal("quick pick opened without the capability")
	}
}
