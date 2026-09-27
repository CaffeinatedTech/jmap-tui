package app

// Contacts (M9, FR-L): the full-screen contacts view, the contact form
// modal, and the plumbing that keeps both fed from the per-account sync
// stores. Design record: CONTACTS_PLAN.md. The screen and the form follow
// the repo's pointer-field/nil-closed overlay pattern; the keyboard never
// reaches the keymap while either owns it.

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// --- messages ---

// contactMsg carries a contact snapshot from one account (the contacts
// twin of snapMsg): direct from an operation, or live from the engine's
// contact broadcast.
type contactMsg struct {
	acct string
	snap sync.ContactSnapshot
	live bool
}

// contactOpMsg reports a finished contact mutation. seq matches the form
// that asked for it (0 = no form: delete commits), so a result lands
// wherever it belongs; done is the success receipt for formless flows.
type contactOpMsg struct {
	seq  int
	acct string
	op   string
	done string
	snap sync.ContactSnapshot
	err  error
}

// contactDeleteCommitMsg fires when a held contact destroy's undo window
// closes (FR-L2: destroy is irreversible, so it waits out the toast).
type contactDeleteCommitMsg struct {
	seq int
}

// --- screen state ---

// contactsState is the open contacts screen (FR-L1): scope (all accounts
// or one), the selected book, the filter, and a key-anchored cursor — the
// §4.1 id-tracking rule applied here too, so live changes re-anchor
// instead of stranding the selection.
type contactsState struct {
	acct string // scope: "" = all accounts (unified), else one account id
	// bookKey/rowKey anchor the cursors across rebuilds; the indices are
	// re-derived every rebuild from them.
	bookKey   string
	rowKey    string
	filter    string
	filtering bool
	focus     ui.ContactColumn

	// Derived per rebuild (the ui types are cheap to hold directly, the
	// picker's []ui.PickerItem precedent).
	books    []ui.ContactBookRow
	bookSel  int
	rows     []ui.ContactRow
	rowIDs   []mail.ID
	rowAccts []string
	rowSel   int
	detail   *ui.ContactDetailView
}

// contactFormState is the new/edit contact modal (FR-L2).
type contactFormState struct {
	title  string
	fields []textinput.Model
	sel    int
	seq    int
	acct   string

	// editID empty = create. current holds the loaded card an update
	// patches against (labels and first-entry keys ride along).
	editID  mail.ID
	current mail.Contact

	// Create-only target book: cycled with ←/→ on the book row. Empty
	// books mean "server default" (resolved engine-side).
	books   []mail.ID
	bookIdx int

	err    string
	saving bool
}

// contactFormFieldNames is the form's fixed field order.
var contactFormFieldNames = []string{"first", "last", "emails", "phones", "org", "title", "note"}

// pendingContactDelete holds a contact destroy through its undo window.
type pendingContactDelete struct {
	seq  int
	acct string
	id   mail.ID
	name string
}

// --- open / close ---

// toggleContactsScreen opens the full-screen contacts view (FR-L1) or
// closes it. Opening warms every supporting account's store (FR-L3's
// suggestion source needs the same data, so one load serves both).
func (m *Model) toggleContactsScreen() (tea.Model, tea.Cmd) {
	if m.contacts != nil {
		m.contacts = nil
		return m, nil
	}
	if !m.anyContactsSupported() {
		m.err = "this server has no contacts support (urn:ietf:params:jmap:contacts)"
		return m, nil
	}
	// Default scope: unified when more than one account supports
	// contacts, else the active one.
	acct := m.activeID
	supported := m.contactsAccounts()
	if len(supported) > 1 {
		acct = ""
	} else if len(supported) == 1 {
		acct = supported[0]
	}
	m.contacts = &contactsState{
		acct:    acct,
		focus:   ui.ContactList,
		bookKey: "",
		rowKey:  "",
	}
	m.rebuildContacts()
	return m, m.ensureContacts()
}

// closeContactForm dismisses the modal without saving.
func (m *Model) closeContactForm() {
	m.contactForm = nil
}

// anyContactsSupported reports whether at least one enrolled account can
// do contacts (FR-L6).
func (m *Model) anyContactsSupported() bool {
	return len(m.contactsAccounts()) > 0
}

// contactsAccounts lists the accounts whose engine has the contacts seam,
// in enrollment order.
func (m *Model) contactsAccounts() []string {
	var out []string
	for _, a := range m.accounts {
		if eng := m.hub.Engine(a.ID); eng != nil && eng.ContactsSupported() {
			out = append(out, a.ID)
		}
	}
	return out
}

// ensureContacts loads the store of every supporting account that is still
// cold; warm stores are no-ops. Returns nil when there is nothing to do.
func (m *Model) ensureContacts() tea.Cmd {
	var cmds []tea.Cmd
	for _, acct := range m.contactsAccounts() {
		eng, ok := m.engineFor(acct)
		if !ok {
			continue
		}
		if m.contactSnaps[acct].Loaded {
			continue
		}
		cmds = append(cmds, func() tea.Msg {
			if err := eng.LoadContacts(m.ctx); err != nil {
				return contactMsg{acct: acct, snap: sync.ContactSnapshot{Err: err.Error()}}
			}
			return contactMsg{acct: acct, snap: eng.Contacts()}
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// waitContactsFor consumes one account's contact broadcast; each delivery
// re-arms itself (the waitUpdatesFor pattern, contacts edition).
func (m *Model) waitContactsFor(acct string) tea.Cmd {
	eng := m.hub.Engine(acct)
	if eng == nil {
		return nil
	}
	return func() tea.Msg {
		snap, ok := <-eng.ContactUpdates()
		if !ok {
			return nil
		}
		return contactMsg{acct: acct, snap: snap, live: true}
	}
}

// applyContactSnap stores a newer snapshot and refreshes anything open.
func (m *Model) applyContactSnap(acct string, snap sync.ContactSnapshot) tea.Cmd {
	if snap.Version <= m.contactSnaps[acct].Version {
		return nil
	}
	m.contactSnaps[acct] = snap
	if m.contacts != nil {
		m.rebuildContacts()
	}
	// The composer's popup and the quick-pick both read the merged
	// stores: refresh them so a late load fills them in.
	if m.compose != nil && m.compose.suggest != nil {
		m.updateComposeSuggest()
	}
	if m.picker != nil && m.picker.mode == pickerContactQuick {
		m.rebuildQuickPick()
	}
	if m.contactForm != nil && len(m.contactForm.books) == 0 {
		// The form opened before the books arrived: adopt them now and
		// preselect the default book.
		if books := m.formBooks(acct); len(books) > 0 {
			m.contactForm.books = books
			m.contactForm.bookIdx = m.defaultBookIdx(acct)
		}
	}
	return nil
}

// --- row building ---

// contactScopeAccounts is the account list the current scope renders.
func (m *Model) contactScopeAccounts() []string {
	if m.contacts != nil && m.contacts.acct != "" {
		return []string{m.contacts.acct}
	}
	return m.contactsAccounts()
}

// rebuildContacts re-derives the screen's books/rows/detail from the
// snapshots, re-anchoring both cursors by key (never by index).
func (m *Model) rebuildContacts() {
	cs := m.contacts
	if cs == nil {
		return
	}
	accts := m.contactsAccounts()

	// --- books column ---
	var books []ui.ContactBookRow
	for _, acct := range accts {
		if cs.acct != "" && acct != cs.acct {
			continue
		}
		books = append(books, ui.ContactBookRow{
			Label: m.accountName(acct), Depth: 0, Account: acct, All: true,
			Count: len(m.contactSnaps[acct].Contacts),
		})
		for _, b := range m.contactSnaps[acct].Books {
			books = append(books, ui.ContactBookRow{
				ID: b.ID, Label: b.Name, Depth: 1, Account: acct,
				Count: m.contactBookCount(acct, b.ID),
			})
		}
	}
	cs.books = books
	cs.bookSel = indexOfBook(books, cs.bookKey)

	// --- rows ---
	filter := strings.ToLower(cs.filter)
	var rows []ui.ContactRow
	var ids []mail.ID
	var owners []string
	for _, acct := range m.contactScopeAccounts() {
		for _, c := range m.contactSnaps[acct].Contacts {
			if bookID := bookKeyID(cs.bookKey, acct); bookID != "" && bookID != "*" {
				if !contactInBook(c, mail.ID(bookID)) {
					continue
				}
			}
			if filter != "" && !contactMatches(c, filter) {
				continue
			}
			rows = append(rows, ui.ContactRow{
				Name: contactListName(c), Email: contactPrimaryEmail(c), Account: acct,
			})
			ids = append(ids, c.ID)
			owners = append(owners, acct)
		}
	}
	cs.rows = rows
	cs.rowIDs = ids
	cs.rowAccts = owners
	cs.rowSel = indexOfRow(ids, owners, cs.rowKey)
	if cs.rowSel < 0 {
		cs.rowSel = 0
	}
	if cs.rowSel >= len(rows) {
		cs.rowSel = len(rows) - 1
	}
	if cs.rowSel < 0 {
		cs.rowSel = 0
	}

	// --- detail ---
	if cs.rowSel >= 0 && cs.rowSel < len(rows) {
		if c, ok := m.contactByID(owners[cs.rowSel], ids[cs.rowSel]); ok {
			cs.detail = contactDetail(c, m.accountName(owners[cs.rowSel]))
		} else {
			cs.detail = nil
		}
	} else {
		cs.detail = nil
	}
}

// contactBookCount counts an account's contacts in one book.
func (m *Model) contactBookCount(acct string, book mail.ID) int {
	n := 0
	for _, c := range m.contactSnaps[acct].Contacts {
		if contactInBook(c, book) {
			n++
		}
	}
	return n
}

// contactByID resolves a contact from an account's snapshot.
func (m *Model) contactByID(acct string, id mail.ID) (mail.Contact, bool) {
	for _, c := range m.contactSnaps[acct].Contacts {
		if c.ID == id {
			return c, true
		}
	}
	return mail.Contact{}, false
}

func contactInBook(c mail.Contact, book mail.ID) bool {
	for _, id := range c.AddressBookIDs {
		if id == book {
			return true
		}
	}
	return false
}

// contactMatches is the type-to-filter rule: every token must appear in
// the name or an email (case-insensitive substring).
func contactMatches(c mail.Contact, lowerFilter string) bool {
	hay := strings.ToLower(c.DisplayName + " " + c.GivenName + " " + c.Surname)
	for _, e := range c.Emails {
		hay += " " + strings.ToLower(e.Address+" "+e.Label)
	}
	for _, p := range c.Phones {
		hay += " " + strings.ToLower(p.Number)
	}
	for _, tok := range strings.Fields(lowerFilter) {
		if !strings.Contains(hay, tok) {
			return false
		}
	}
	return true
}

// contactListName is the name column: display name, else the email.
func contactListName(c mail.Contact) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	if len(c.Emails) > 0 {
		return c.Emails[0].Address
	}
	return "—"
}

func contactPrimaryEmail(c mail.Contact) string {
	if len(c.Emails) > 0 {
		return c.Emails[0].Address
	}
	return ""
}

// contactDetail builds the right column for one contact.
func contactDetail(c mail.Contact, account string) *ui.ContactDetailView {
	d := &ui.ContactDetailView{Name: contactListName(c), Account: account}
	for _, e := range c.Emails {
		if e.Label != "" {
			d.Emails = append(d.Emails, e.Label+": "+e.Address)
		} else {
			d.Emails = append(d.Emails, e.Address)
		}
	}
	for _, p := range c.Phones {
		if p.Label != "" {
			d.Phones = append(d.Phones, p.Label+": "+p.Number)
		} else {
			d.Phones = append(d.Phones, p.Number)
		}
	}
	if len(c.Orgs) > 0 {
		d.Org = c.Orgs[0].Value
	}
	if len(c.Titles) > 0 {
		d.Title = c.Titles[0].Value
	}
	if len(c.Notes) > 0 {
		d.Note = c.Notes[0].Value
	}
	return d
}

// bookKeyID decodes a book row key into its book id: "" = scope-all,
// "*" = the account's all-contacts row, else the book id.
func bookKeyID(key, acct string) string {
	prefix := acct + "\x00"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return strings.TrimPrefix(key, prefix)
}

func indexOfBook(books []ui.ContactBookRow, key string) int {
	if key == "" {
		return 0
	}
	for i, b := range books {
		if bookRowKey(b) == key {
			return i
		}
	}
	return 0
}

func bookRowKey(b ui.ContactBookRow) string {
	kind := "*"
	if b.ID != "" {
		kind = string(b.ID)
	}
	return b.Account + "\x00" + kind
}

func indexOfRow(ids []mail.ID, accts []string, key string) int {
	if key == "" {
		return 0
	}
	for i := range ids {
		if accts[i]+"\x00"+string(ids[i]) == key {
			return i
		}
	}
	return -1
}

// --- screen keyboard ---

// contactsKey owns the keyboard while the contacts screen is open (FR-L1).
func (m *Model) contactsKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cs := m.contacts
	if cs == nil {
		return m, nil
	}
	key := msg.Keystroke()

	// Filter capture: typing lands in the filter until enter/esc.
	if cs.filtering {
		switch key {
		case "enter", "esc":
			cs.filtering = false
			m.rebuildContacts()
			return m, nil
		case "backspace":
			if r := []rune(cs.filter); len(r) > 0 {
				cs.filter = string(r[:len(r)-1])
				m.rebuildContacts()
			}
			return m, nil
		default:
			if key != "" && len([]rune(key)) == 1 && key[0] >= 32 {
				cs.filter += key
				m.rebuildContacts()
				return m, nil
			}
			return m, nil
		}
	}

	page := max(m.height-6, 1)
	moveBooks := func(delta int) {
		cs.bookSel = max(min(cs.bookSel+delta, len(cs.books)-1), 0)
		if cs.bookSel >= 0 && cs.bookSel < len(cs.books) {
			cs.bookKey = bookRowKey(cs.books[cs.bookSel])
			cs.rowKey = ""
			m.rebuildContacts()
			cs.bookSel = indexOfBook(cs.books, cs.bookKey)
		}
	}
	moveRows := func(delta int) {
		if len(cs.rows) == 0 {
			return
		}
		cs.rowSel = max(min(cs.rowSel+delta, len(cs.rows)-1), 0)
		cs.rowKey = cs.rowAccts[cs.rowSel] + "\x00" + string(cs.rowIDs[cs.rowSel])
		m.rebuildContacts()
		if cs.rowSel >= 0 && cs.rowSel < len(cs.rowIDs) {
			cs.rowKey = cs.rowAccts[cs.rowSel] + "\x00" + string(cs.rowIDs[cs.rowSel])
		}
	}

	switch key {
	case "esc":
		if cs.filter != "" && !cs.filtering {
			// First esc drops the filter, second closes the screen.
			cs.filter = ""
			m.rebuildContacts()
			return m, nil
		}
		m.contacts = nil
		return m, nil
	case "c":
		// The open action toggles from inside the screen too — c never
		// reaches the keymap while the screen owns the keyboard.
		m.contacts = nil
		return m, nil
	case "tab":
		cs.focus = cs.focus.Next()
		return m, nil
	case "shift+tab":
		cs.focus = cs.focus.Prev()
		return m, nil
	case "j", "down":
		if cs.focus == ui.ContactBooks {
			moveBooks(1)
		} else {
			moveRows(1)
		}
		return m, nil
	case "k", "up":
		if cs.focus == ui.ContactBooks {
			moveBooks(-1)
		} else {
			moveRows(-1)
		}
		return m, nil
	case "g":
		if cs.focus == ui.ContactBooks {
			moveBooks(-len(cs.books))
		} else {
			moveRows(-len(cs.rows))
		}
		return m, nil
	case "shift+g":
		if cs.focus == ui.ContactBooks {
			moveBooks(len(cs.books))
		} else {
			moveRows(len(cs.rows))
		}
		return m, nil
	case "ctrl+f", "space", "pgdown":
		if cs.focus == ui.ContactBooks {
			moveBooks(page)
		} else {
			moveRows(page)
		}
		return m, nil
	case "ctrl+b", "pgup":
		if cs.focus == ui.ContactBooks {
			moveBooks(-page)
		} else {
			moveRows(-page)
		}
		return m, nil
	case "enter":
		switch cs.focus {
		case ui.ContactBooks:
			cs.focus = ui.ContactList
		case ui.ContactList:
			cs.focus = ui.ContactDetail
		case ui.ContactDetail:
			return m.openContactEdit()
		}
		return m, nil
	case "/":
		cs.filtering = true
		return m, nil
	case "n":
		return m.openContactNew()
	case "e":
		return m.openContactEdit()
	case "d":
		return m, m.prepareContactDelete()
	}
	// A short whitelist of global actions stays live over the screen —
	// undo must reach a held delete (FR-L2), and help/quit/switcher are
	// never screen-local. Everything else stays screen-owned.
	if act, ok := m.opts.Keys.Match(ui.PaneAny, key); ok {
		switch act {
		case ui.ActUndo, ui.ActHelp, ui.ActQuit, ui.ActAccountSwitch, ui.ActAccountManage:
			return m.runAction(act)
		}
	}
	return m, nil
}

// --- form ---

// openContactNew opens a blank contact form on the given account (or the
// sensible default when acct is empty).
func (m *Model) openContactForm(acct string) {
	if acct == "" {
		acct = m.activeID
	}
	fields := make([]textinput.Model, 0, len(contactFormFieldNames))
	for range contactFormFieldNames {
		ti := textinput.New()
		ti.Prompt = ""
		ti.SetWidth(44)
		fields = append(fields, ti)
	}
	seq := m.seqNext()
	m.contactForm = &contactFormState{
		title:  "new contact",
		fields: fields,
		seq:    seq,
		acct:   acct,
		books:  m.formBooks(acct),
	}
	m.contactForm.bookIdx = m.defaultBookIdx(acct)
	m.contactFormRefocus()
}

// openContactNew starts the add flow (FR-L4): from a message the sender
// prefills the form; an existing contact with that address opens in edit
// mode instead of duplicating it. On the contacts screen it starts blank,
// scoped to the screen's account.
func (m *Model) openContactNew() (tea.Model, tea.Cmd) {
	if !m.anyContactsSupported() {
		m.err = "this server has no contacts support (urn:ietf:params:jmap:contacts)"
		return m, nil
	}
	cmd := m.ensureContacts()
	acct := m.activeID
	if m.contacts != nil {
		if m.contacts.acct != "" {
			acct = m.contacts.acct
		}
		m.openContactForm(acct)
		m.contactFormRefocus()
		return m, cmd
	}

	// From the reader: prefill from the message under the cursor.
	acct = m.ownerAccount()
	m.openContactForm(acct)
	if _, r, ok := m.cursorRef(); ok && len(r.Summary.From) > 0 {
		from := r.Summary.From[0]
		if existing, owner, found := m.findContactByEmail(from.Email); found {
			m.openContactFormFor(existing, owner)
			return m, tea.Batch(cmd, m.contactFormBookRefresh())
		}
		m.setFormField(0, firstNameOf(from.Name))
		m.setFormField(1, lastNameOf(from.Name))
		if from.Email != "" {
			m.setFormField(2, from.Email)
		}
	}
	m.contactFormRefocus()
	return m, cmd
}

// setFormField writes one form field before it is rendered.
func (m *Model) setFormField(i int, v string) {
	if m.contactForm != nil {
		m.contactForm.set(i, v)
	}
}

// openContactEdit opens the form for the selected contact (FR-L2).
func (m *Model) openContactEdit() (tea.Model, tea.Cmd) {
	cs := m.contacts
	if cs == nil || cs.rowSel < 0 || cs.rowSel >= len(cs.rows) {
		return m, nil
	}
	acct := cs.rowAccts[cs.rowSel]
	c, ok := m.contactByID(acct, cs.rowIDs[cs.rowSel])
	if !ok {
		return m, nil
	}
	m.openContactFormFor(c, acct)
	return m, m.contactFormBookRefresh()
}

// openContactFormFor prefills the modal from an existing contact.
func (m *Model) openContactFormFor(c mail.Contact, acct string) {
	m.openContactForm(acct)
	f := m.contactForm
	f.title = "edit contact"
	f.editID = c.ID
	f.current = c
	f.set(0, c.GivenName)
	f.set(1, c.Surname)
	addrs := make([]string, 0, len(c.Emails))
	for _, e := range c.Emails {
		addrs = append(addrs, e.Address)
	}
	f.set(2, strings.Join(addrs, ", "))
	nums := make([]string, 0, len(c.Phones))
	for _, p := range c.Phones {
		nums = append(nums, p.Number)
	}
	f.set(3, strings.Join(nums, ", "))
	if len(c.Orgs) > 0 {
		f.set(4, c.Orgs[0].Value)
	}
	if len(c.Titles) > 0 {
		f.set(5, c.Titles[0].Value)
	}
	if len(c.Notes) > 0 {
		f.set(6, c.Notes[0].Value)
	}
	// Pin the book row to the card's first book.
	if len(c.AddressBookIDs) > 0 {
		books := m.formBooks(acct)
		f.books = books
		f.bookIdx = 0
		for i, id := range books {
			if id == c.AddressBookIDs[0] {
				f.bookIdx = i
				break
			}
		}
	}
	m.contactFormRefocus()
}

// set stores a field value on the form.
// set stores a field value on the form. Prefilled values come from server
// contact data, and the form's fields render as a styled input widget the
// render boundary cannot strip later — so the strip happens here (D-3).
func (f *contactFormState) set(i int, v string) {
	if i >= 0 && i < len(f.fields) {
		f.fields[i].SetValue(ui.Sanitize(v))
	}
}

func (f *contactFormState) value(i int) string {
	if i >= 0 && i < len(f.fields) {
		return f.fields[i].Value()
	}
	return ""
}

// formBooks is the target-account's book ids for the create-time row.
func (m *Model) formBooks(acct string) []mail.ID {
	books := m.contactSnaps[acct].Books
	out := make([]mail.ID, 0, len(books))
	for _, b := range books {
		out = append(out, b.ID)
	}
	return out
}

// defaultBookIdx is the position of the default book in formBooks order.
func (m *Model) defaultBookIdx(acct string) int {
	books := m.contactSnaps[acct].Books
	for i, b := range books {
		if b.IsDefault {
			return i
		}
	}
	return 0
}

// contactFormBookRefresh fills the create-time book row if the modal
// opened before the books landed.
func (m *Model) contactFormBookRefresh() tea.Cmd {
	f := m.contactForm
	if f == nil || f.editID != "" || len(f.books) > 0 {
		return nil
	}
	if books := m.formBooks(f.acct); len(books) > 0 {
		f.books = books
		f.bookIdx = m.defaultBookIdx(f.acct)
	}
	return nil
}

// contactFormRefocus syncs textinput focus with the selected row.
func (m *Model) contactFormRefocus() {
	f := m.contactForm
	if f == nil {
		return
	}
	for i := range f.fields {
		if i == f.sel {
			f.fields[i].Focus()
		} else {
			f.fields[i].Blur()
		}
	}
}

// bookRowOn is the selectable book row's index (the adv modal's
// attachment-row trick): appended after the text fields on create.
func (f *contactFormState) bookRowOn() bool { return f.editID == "" }

func (f *contactFormState) maxSel() int {
	max := len(f.fields) - 1
	if f.bookRowOn() {
		max++
	}
	return max
}

// contactFormKey owns the keyboard while the modal is open.
func (m *Model) contactFormKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.contactForm
	if f == nil {
		return nil
	}
	key := msg.Keystroke()
	if f.saving {
		// One save in flight: swallow everything until it lands.
		return nil
	}
	switch key {
	case "esc":
		m.closeContactForm()
		return nil
	case "enter":
		return m.contactFormSave()
	case "up", "shift+tab":
		f.sel = max(f.sel-1, 0)
		f.err = ""
		m.contactFormRefocus()
		return nil
	case "down", "tab":
		f.sel = min(f.sel+1, f.maxSel())
		f.err = ""
		m.contactFormRefocus()
		return nil
	case "left", "right":
		if f.sel == len(f.fields) && f.bookRowOn() && len(f.books) > 1 {
			if key == "left" {
				f.bookIdx = (f.bookIdx - 1 + len(f.books)) % len(f.books)
			} else {
				f.bookIdx = (f.bookIdx + 1) % len(f.books)
			}
			return nil
		}
	default:
		if f.sel >= len(f.fields) {
			return nil // on the book row: only ←/→ act
		}
	}
	if f.sel < len(f.fields) {
		ti, cmd := f.fields[f.sel].Update(msg)
		f.fields[f.sel] = ti
		f.err = ""
		return cmd
	}
	return nil
}

// contactFormSave validates and dispatches the create/update (FR-L2).
func (m *Model) contactFormSave() tea.Cmd {
	f := m.contactForm
	if f == nil {
		return nil
	}
	given := strings.TrimSpace(f.value(0))
	surname := strings.TrimSpace(f.value(1))
	emails := splitAddrs(f.value(2))
	phones := splitAddrs(f.value(3))
	if given == "" && surname == "" && len(emails) == 0 {
		f.err = "need a name or an email address"
		return nil
	}
	// Labels the form does not edit survive by address (CONTACTS_PLAN
	// §8 preservation rule).
	draft := mail.ContactDraft{
		GivenName: given,
		Surname:   surname,
		Emails:    carryContactLabels(emails, f.current.Emails),
		Phones:    carryContactPhones(phones, f.current.Phones),
		Org:       strings.TrimSpace(f.value(4)),
		Title:     strings.TrimSpace(f.value(5)),
		Note:      strings.TrimSpace(f.value(6)),
	}
	mut := mail.ContactMutation{}
	if f.editID == "" {
		if len(f.books) > 0 {
			draft.AddressBookIDs = []mail.ID{f.books[min(f.bookIdx, len(f.books)-1)]}
		} // empty → engine resolves the default book
		mut.Create = map[string]mail.ContactDraft{"c1": draft}
	} else {
		mut.Update = map[mail.ID]mail.ContactUpdate{
			f.editID: {Draft: draft, Current: f.current},
		}
	}
	f.saving = true
	f.err = ""
	return m.contactSaveCmd(f.acct, mut, f.seq, "save contact", "")
}

// contactSaveCmd runs one mutation and reports back with the refreshed
// snapshot (FR-K4: one batched round-trip per user action). done, when
// set, is the success receipt shown by flows with no form attached (the
// held delete).
func (m *Model) contactSaveCmd(acct string, mut mail.ContactMutation, seq int, op, done string) tea.Cmd {
	eng, ok := m.engineFor(acct)
	return func() tea.Msg {
		if !ok {
			return contactOpMsg{seq: seq, acct: acct, op: op, err: fmt.Errorf("account %q is not connected", acct)}
		}
		res, err := eng.SetContact(m.ctx, mut)
		if err != nil {
			return contactOpMsg{seq: seq, acct: acct, op: op, err: err}
		}
		if e := firstSetError(res); e != nil {
			return contactOpMsg{seq: seq, acct: acct, op: op, err: e}
		}
		return contactOpMsg{seq: seq, acct: acct, op: op, done: done, snap: eng.Contacts()}
	}
}

// firstSetError surfaces the server's per-id rejection, if any.
func firstSetError(res mail.ContactMutationResult) error {
	for _, e := range res.NotCreated {
		return e
	}
	for _, e := range res.NotUpdated {
		return e
	}
	for _, e := range res.NotDestroyed {
		return e
	}
	return nil
}

// handleContactOp applies a finished mutation: the form closes on success
// (or keeps the error inline), the screen refreshes from the snapshot.
func (m *Model) handleContactOp(msg contactOpMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if msg.err != nil {
		if m.contactForm != nil && m.contactForm.seq == msg.seq && msg.seq != 0 {
			m.contactForm.saving = false
			m.contactForm.err = shortErr(msg.err)
			return m, nil
		}
		m.err = truncateErr(msg.op, msg.err)
		return m, nil
	}
	cmds = append(cmds, m.applyContactSnap(msg.acct, msg.snap))
	if m.contactForm != nil && m.contactForm.seq == msg.seq && msg.seq != 0 {
		verb := "Contact saved"
		if m.contactForm.editID == "" {
			verb = "Contact added"
		}
		m.contactForm = nil
		cmds = append(cmds, m.showToast(verb, "", nil, nil))
	} else if msg.done != "" {
		cmds = append(cmds, m.showToast(msg.done, "", nil, nil))
	}
	return m, tea.Batch(cmds...)
}

// prepareContactDelete holds the destroy behind the undo toast: a contact
// has no trash folder, so the /set waits out the window (FR-L2, the
// mail-side prepareDestroy pattern).
func (m *Model) prepareContactDelete() tea.Cmd {
	cs := m.contacts
	if cs == nil || cs.rowSel < 0 || cs.rowSel >= len(cs.rows) {
		return nil
	}
	acct := cs.rowAccts[cs.rowSel]
	id := cs.rowIDs[cs.rowSel]
	name := cs.rows[cs.rowSel].Name
	m.pendingContactDelete = &pendingContactDelete{seq: m.seqNext(), acct: acct, id: id, name: name}
	seq := m.pendingContactDelete.seq
	return tea.Batch(
		m.showToast(fmt.Sprintf("Deleting %s", name), "ctrl+z cancel", nil, nil),
		tea.Tick(toastTTL, func(time.Time) tea.Msg { return contactDeleteCommitMsg{seq: seq} }),
	)
}

// contactDeleteCommit runs the held destroy after its window closes.
func (m *Model) contactDeleteCommit(seq int) (tea.Model, tea.Cmd) {
	p := m.pendingContactDelete
	if p == nil || p.seq != seq {
		return m, nil
	}
	m.pendingContactDelete = nil
	m.toast = nil
	return m, m.contactSaveCmd(p.acct,
		mail.ContactMutation{Destroy: []mail.ID{p.id}}, 0, "delete contact",
		"Deleted "+p.name)
}

// undoContactDelete cancels a held contact destroy (ctrl+z window).
func (m *Model) undoContactDelete() bool {
	if m.pendingContactDelete == nil {
		return false
	}
	m.pendingContactDelete = nil
	m.toast = nil
	return true
}

// --- merged suggestion source (FR-L3) ---

// suggestCandidate is one insertable (contact, email) pair from the merged
// stores: deduped by address so two accounts holding the same person
// suggest once, first account (enrollment order) wins.
type suggestCandidate struct {
	Name   string
	Email  string
	Suffix string // account name, shown only when >1 account contributes
}

// contactCandidates builds the merged, deduped candidate list.
func (m *Model) contactCandidates() []suggestCandidate {
	seen := map[string]bool{}
	var out []suggestCandidate
	multi := len(m.contactsAccounts()) > 1
	for _, acct := range m.contactsAccounts() {
		suffix := ""
		if multi {
			suffix = m.accountName(acct)
		}
		for _, c := range m.contactSnaps[acct].Contacts {
			for _, e := range c.Emails {
				if e.Address == "" {
					continue
				}
				key := strings.ToLower(e.Address)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, suggestCandidate{
					Name: c.DisplayName, Email: e.Address, Suffix: suffix,
				})
			}
		}
	}
	return out
}

// contactsWarmed reports whether every supporting account's store is warm
// (the suggestion popup shows loading… until then).
func (m *Model) contactsWarmed() bool {
	accts := m.contactsAccounts()
	if len(accts) == 0 {
		return false
	}
	for _, acct := range accts {
		if !m.contactSnaps[acct].Loaded {
			return false
		}
	}
	return true
}

// matchContacts ranks candidates for a typed token: whole-token prefix
// hits first, then substring hits, each in candidate order; limit caps the
// popup (top 5, CONTACTS_PLAN §3.4).
func matchContacts(cands []suggestCandidate, token string, limit int) []suggestCandidate {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return nil
	}
	var prefix, contains []suggestCandidate
	for _, c := range cands {
		name := strings.ToLower(c.Name)
		email := strings.ToLower(c.Email)
		switch {
		case strings.HasPrefix(name, token) || strings.HasPrefix(email, token):
			prefix = append(prefix, c)
		case strings.Contains(name, token) || strings.Contains(email, token):
			contains = append(contains, c)
		}
	}
	out := append(prefix, contains...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// --- helpers ---

// splitAddrs splits a comma/semicolon/newline separated form field.
func splitAddrs(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// carryContactLabels rebuilds the email list from form addresses, keeping
// each existing entry's label when the address is unchanged.
func carryContactLabels(addrs []string, current []mail.ContactEmail) []mail.ContactEmail {
	out := make([]mail.ContactEmail, 0, len(addrs))
	for _, a := range addrs {
		e := mail.ContactEmail{Address: a}
		for _, cur := range current {
			if strings.EqualFold(cur.Address, a) {
				e.Label = cur.Label
				break
			}
		}
		out = append(out, e)
	}
	return out
}

// carryContactPhones is carryContactLabels for phone numbers.
func carryContactPhones(nums []string, current []mail.ContactPhone) []mail.ContactPhone {
	out := make([]mail.ContactPhone, 0, len(nums))
	for _, n := range nums {
		p := mail.ContactPhone{Number: n}
		for _, cur := range current {
			if cur.Number == n {
				p.Label = cur.Label
				break
			}
		}
		out = append(out, p)
	}
	return out
}

// firstNameOf splits a display name into its first token.
func firstNameOf(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// lastNameOf splits a display name into everything after the first token.
func lastNameOf(name string) string {
	fields := strings.Fields(name)
	if len(fields) < 2 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

// findContactByEmail looks a merged contact up by address (FR-L4: the
// add-sender flow opens edit mode instead of a duplicate).
func (m *Model) findContactByEmail(email string) (mail.Contact, string, bool) {
	if email == "" {
		return mail.Contact{}, "", false
	}
	for _, acct := range m.contactsAccounts() {
		for _, c := range m.contactSnaps[acct].Contacts {
			for _, e := range c.Emails {
				if strings.EqualFold(e.Address, email) {
					return c, acct, true
				}
			}
		}
	}
	return mail.Contact{}, "", false
}

// --- render-state builders ---

// contactsView maps the screen state onto the render types.
func (m *Model) contactsView() *ui.ContactsView {
	cs := m.contacts
	if cs == nil {
		return nil
	}
	scope := "all accounts"
	if cs.acct != "" {
		scope = m.accountName(cs.acct)
	}
	loading := false
	errText := ""
	for _, acct := range m.contactsAccounts() {
		snap := m.contactSnaps[acct]
		if !snap.Loaded {
			loading = true
		}
		if snap.Err != "" && errText == "" {
			errText = snap.Err
		}
	}
	return &ui.ContactsView{
		Scope:     scope,
		Filter:    cs.filter,
		Filtering: cs.filtering,
		Books:     cs.books,
		BookSel:   cs.bookSel,
		Rows:      cs.rows,
		RowSel:    cs.rowSel,
		Detail:    cs.detail,
		Focus:     cs.focus,
		Loading:   loading,
		Err:       errText,
	}
}

// contactFormView maps the modal onto the render types: the fixed field
// rows plus the create-only book row (focused when selected).
func (m *Model) contactFormView() *ui.ContactFormView {
	f := m.contactForm
	if f == nil {
		return nil
	}
	out := &ui.ContactFormView{
		Title: f.title,
		Err:   f.err,
		Hint:  "enter save · esc cancel · tab fields",
	}
	for i, name := range contactFormFieldNames {
		out.Fields = append(out.Fields, ui.AdvField{
			Name:    name,
			View:    f.fields[i].View(),
			Focused: f.sel == i,
		})
	}
	if f.bookRowOn() {
		label := "(default)"
		if len(f.books) > 0 {
			idx := min(f.bookIdx, len(f.books)-1)
			label = m.bookName(f.acct, f.books[idx])
		}
		out.Fields = append(out.Fields, ui.AdvField{
			Name:    "book",
			View:    label + "  (←/→)",
			Focused: f.sel == len(f.fields),
		})
	}
	return out
}

// bookName resolves a book id to its display name for the form row.
func (m *Model) bookName(acct string, id mail.ID) string {
	for _, b := range m.contactSnaps[acct].Books {
		if b.ID == id {
			return b.Name
		}
	}
	return string(id)
}

// --- compose suggestions (FR-L3) ---

// suggestPopup is the open autocomplete dropdown's state: the ranked
// matches for the token under the cursor plus the highlighted row.
type suggestPopup struct {
	items []suggestCandidate
	sel   int
}

// isAddressZone reports whether the composer focus can take a recipient.
func isAddressZone(z ui.ComposeZone) bool {
	return z == ui.ZoneTo || z == ui.ZoneCc || z == ui.ZoneBcc
}

// addressInputFor returns the focused address field's input, or nil.
func addressInputFor(c *composeState, z ui.ComposeZone) *textinput.Model {
	switch z {
	case ui.ZoneTo:
		return &c.to
	case ui.ZoneCc:
		return &c.cc
	case ui.ZoneBcc:
		return &c.bcc
	}
	return nil
}

// tokenBefore is the recipient token being typed at pos: the text after
// the last separator, left-trimmed (leading spaces belong to the gap the
// insert lands after).
func tokenBefore(val string, pos int) string {
	if pos > len(val) {
		pos = len(val)
	}
	seg := val[:pos]
	if i := strings.LastIndexAny(seg, ",;\n"); i >= 0 {
		seg = seg[i+1:]
	}
	return strings.TrimLeft(seg, " \t")
}

// updateComposeSuggest recomputes the popup for the token under the
// cursor: no token (or another zone) closes it, a cold store shows the
// loading row, and zero matches close it silently (FR-L3).
func (m *Model) updateComposeSuggest() {
	c := m.compose
	if c == nil {
		return
	}
	if !isAddressZone(c.focus) {
		c.suggest = nil
		return
	}
	if c.suggestOff {
		return
	}
	in := addressInputFor(c, c.focus)
	if in == nil {
		return
	}
	if tokenBefore(in.Value(), in.Position()) == "" {
		c.suggest = nil
		return
	}
	if !m.anyContactsSupported() {
		// No capability anywhere: never show a loading row that can't
		// finish (FR-L6 — the popup simply doesn't exist).
		c.suggest = nil
		return
	}
	if !m.contactsWarmed() {
		c.suggest = &suggestPopup{}
		return
	}
	items := matchContacts(m.contactCandidates(), tokenBefore(in.Value(), in.Position()), 5)
	if len(items) == 0 {
		c.suggest = nil
		return
	}
	sel := 0
	if c.suggest != nil && c.suggest.sel < len(items) {
		sel = c.suggest.sel
	}
	c.suggest = &suggestPopup{items: items, sel: sel}
}

// acceptSuggest inserts the highlighted match (enter/tab while the popup
// shows, FR-L3) and closes it; focus stays in the field.
func (m *Model) acceptSuggest() tea.Cmd {
	c := m.compose
	if c == nil || c.suggest == nil || len(c.suggest.items) == 0 {
		return nil
	}
	in := addressInputFor(c, c.focus)
	if in == nil {
		return nil
	}
	it := c.suggest.items[min(c.suggest.sel, len(c.suggest.items)-1)]
	return m.insertAddressAt(in, suggestAddress(it))
}

// insertComposeAddress inserts a quick-picked address (FR-L3's ctrl+g
// picker) into the focused field.
func (m *Model) insertComposeAddress(label string) tea.Cmd {
	c := m.compose
	if c == nil || !isAddressZone(c.focus) {
		return nil
	}
	in := addressInputFor(c, c.focus)
	if in == nil {
		return nil
	}
	return m.insertAddressAt(in, label)
}

// insertAddressAt replaces the token before the cursor with addr (plus a
// trailing ", " when nothing follows) and leaves the cursor after it.
func (m *Model) insertAddressAt(in *textinput.Model, addr string) tea.Cmd {
	c := m.compose
	val := in.Value()
	pos := min(max(in.Position(), 0), len(val))
	tok := tokenBefore(val, pos)
	before := val[:pos-len(tok)]
	suffix := val[pos:]
	insert := addr
	if strings.TrimSpace(suffix) == "" {
		insert += ", "
	}
	in.SetValue(before + insert + suffix)
	in.SetCursor(len(before) + len(insert))
	c.suggest = nil
	c.suggestOff = false
	return m.markDirty()
}

// suggestAddress formats a candidate for insertion: "Name <email>", or
// the bare address when the name would break the header's comma parsing
// (no quoting exists on the way back in — FR-H1's parseAddressList).
func suggestAddress(it suggestCandidate) string {
	if it.Name == "" || strings.ContainsAny(it.Name, ",<>\n\"") {
		return it.Email
	}
	return it.Name + " <" + it.Email + ">"
}

// openContactQuickPick opens the ctrl+g picker over the merged set and
// warms a cold store so the list fills in behind it.
func (m *Model) openContactQuickPick() tea.Cmd {
	if !m.anyContactsSupported() {
		return nil
	}
	m.picker = &pickerState{mode: pickerContactQuick}
	m.rebuildQuickPick()
	return m.ensureContacts()
}

// rebuildQuickPick refills the quick-pick's items from the merged
// candidates, keeping the typed filter and selection (called when a
// contact snapshot lands while the picker is up).
func (m *Model) rebuildQuickPick() {
	if m.picker == nil || m.picker.mode != pickerContactQuick {
		return
	}
	filter := m.picker.filter
	all := make([]ui.PickerItem, 0, len(m.contactCandidates()))
	for _, cd := range m.contactCandidates() {
		all = append(all, ui.PickerItem{ID: mail.ID(cd.Email), Label: suggestAddress(cd)})
	}
	m.picker.all = all
	m.picker.filter = filter
	m.pickerRefilter()
}
