package ui

// Contacts render states (M9, FR-L): the full-frame contacts screen, the
// contact form modal, and the recipient suggestion dropdown that hangs
// under a focused composer header field. Pure render types — the app
// builds them; goldens construct them directly.

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// ContactColumn is one focusable column of the contacts screen. Focus
// drives the heavy top rule (FR-I9) and the responsive swap below 100
// columns (FR-I1 rules applied to contacts).
type ContactColumn int

// Contacts screen columns, in Tab order.
const (
	ContactBooks ContactColumn = iota
	ContactList
	ContactDetail
	contactColumnCount
)

// Next cycles the column focus forward (tab).
func (c ContactColumn) Next() ContactColumn { return (c + 1) % contactColumnCount }

// Prev cycles the column focus backward (shift+tab).
func (c ContactColumn) Prev() ContactColumn { return (c + contactColumnCount - 1) % contactColumnCount }

// ContactBookRow is one left-column entry: an account header (Depth 0,
// with its tint as an end-cap) or one of its address books (Depth 1).
// "All contacts" scopes arrive as Depth 0 with Account set and All true.
type ContactBookRow struct {
	ID      mail.ID // book id; empty on account/"all" rows
	Label   string
	Depth   int
	Count   int
	Account string // owning account id, for the tint and name lookup
	All     bool   // the account-wide "all contacts" scope row
}

// ContactRow is one middle-column contact: display identity plus the owner
// for the tint bar (JMAP ids are per-account, FR-A5).
type ContactRow struct {
	Name    string
	Email   string
	Account string
}

// ContactDetailView is the right column: one contact's form-visible fields.
type ContactDetailView struct {
	Name    string
	Account string
	Emails  []string // "label: addr" or plain addr
	Phones  []string
	Org     string
	Title   string
	Note    string
}

// ContactsView is the full-frame contacts screen render state (FR-L1).
// Non-nil while the screen is open; it takes the frame like the composer.
type ContactsView struct {
	Scope string // "all accounts" or the account name

	Filter    string // type-to-filter substring ("" = show all)
	Filtering bool   // the / prompt is capturing keys

	Books   []ContactBookRow
	BookSel int

	Rows   []ContactRow
	RowSel int

	Detail *ContactDetailView // nil when nothing is selected

	Focus   ContactColumn
	Loading bool
	Err     string
}

// ContactFormView is the new/edit contact modal (FR-L2): a fielded form
// over the contact form's own fields, styled like the advanced-search box.
// The create-only target book arrives as the last field row.
type ContactFormView struct {
	Title  string
	Fields []AdvField
	Err    string
	Hint   string
}

// ContactSuggestItem is one row of the recipient dropdown: the inserted
// address plus a muted account suffix so merged accounts stay legible.
type ContactSuggestItem struct {
	Name   string
	Email  string
	Suffix string
}

// ContactSuggestView is the autocomplete dropdown under the focused To/Cc/
// Bcc field (FR-L3). Items are one row per email address; Loading shows a
// single muted row until the lazily-loaded store is warm.
type ContactSuggestView struct {
	Field   string // "To" / "Cc" / "Bcc" — which field it hangs off
	Items   []ContactSuggestItem
	Sel     int
	Loading bool
}

// renderContacts draws the full-frame contacts screen: header, three
// columns with top rules (heavy under the focused column, FR-I9), and the
// key-hint footer. Responsive like the reader: three columns at ≥100, two
// with a focus swap at 60–99, one focused column below 60 (FR-I1).
func renderContacts(w, h int, st State) string {
	c := st.Contacts
	if c == nil {
		return ""
	}
	th := st.Theme
	if w <= 0 || h <= 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(renderContactsHeader(w, st))
	b.WriteString("\n")

	contentH := h - 3 // header + column rules + footer
	if contentH < 1 {
		contentH = 1
	}

	booksW, detailW := 24, 34
	showBooks, showList, showDetail := true, true, true
	switch {
	case w < 60:
		// The reader's sub-60 rule (FR-I1): one focused column.
		showBooks, showList, showDetail =
			c.Focus == ContactBooks, c.Focus == ContactList, c.Focus == ContactDetail
	case w < 100:
		// Two columns: the detail swaps in for the books when focused.
		showDetail = c.Focus == ContactDetail
		showBooks = !showDetail
	}

	var cols []string
	if showBooks {
		bw := booksW
		if !showList && !showDetail {
			bw = w // sole column takes the full frame
		}
		cols = append(cols, renderContactBooks(bw, contentH, c, st))
	}
	if showList {
		lw := w
		if showBooks {
			lw -= booksW
		}
		if showDetail {
			lw -= detailW
		}
		cols = append(cols, renderContactList(max(lw, 10), contentH, c, st))
	}
	if showDetail {
		dw := detailW
		if !showBooks && !showList {
			dw = w
		}
		cols = append(cols, renderContactDetail(dw, contentH, c, th))
	}
	b.WriteString(joinPanes(cols, th))
	b.WriteString("\n")
	b.WriteString(contactsFooter(w, st))
	return b.String()
}

// renderContactsHeader: breadcrumb + scope + filter + count/error (the
// reader's header line, contacts-flavoured).
func renderContactsHeader(w int, st State) string {
	th := st.Theme
	c := st.Contacts
	parts := []string{th.Accent.Render("jmap-tui"), th.Header.Render("contacts"), th.Muted.Render(c.Scope)}
	if c.Filtering || c.Filter != "" {
		parts = append(parts, th.Muted.Render("filter: "+c.Filter))
	}
	if !c.Loading {
		parts = append(parts, th.Muted.Render(itoa(len(c.Rows))))
	}
	line := strings.Join(parts, "  ")
	if c.Err != "" {
		gap := w - lipgloss.Width(line) - lipgloss.Width(c.Err) - 2
		if gap > 0 {
			line += strings.Repeat(" ", gap) + th.Danger.Render(c.Err)
		} else {
			line = th.Danger.Render(c.Err)
		}
	}
	return truncate(line, w)
}

// contactsFooter is the key-hint line for the screen.
func contactsFooter(w int, st State) string {
	c := st.Contacts
	hint := "n new · e edit · d delete · tab columns · / filter · esc back"
	if c.Filtering {
		hint = "type to filter · enter keep · esc stop filtering"
	}
	return truncate(st.Theme.Muted.Render(hint), w)
}

// ruleFor is the column's top rule: heavy accent under the focused column,
// hairline elsewhere (FR-I9).
func ruleFor(w int, focused bool, th Theme) string {
	style := th.Rule
	if focused {
		style = th.RuleActive
	}
	return style.Render(strings.Repeat("─", max(w-1, 1)))
}

// renderContactBooks is the left column: account headers with tint
// end-caps, their address books indented, counts right-aligned.
func renderContactBooks(w, h int, c *ContactsView, st State) string {
	th := st.Theme
	var b strings.Builder
	b.WriteString(ruleFor(w, c.Focus == ContactBooks, th))
	b.WriteString("\n")
	avail := h - 1
	if avail < 1 {
		return b.String()
	}
	// Scroll the selection into view (same windowing as the mail list).
	start := 0
	if c.BookSel >= avail {
		start = c.BookSel - avail + 1
	}
	for i := start; i < len(c.Books) && i-start < avail; i++ {
		row := c.Books[i]
		indent := strings.Repeat("  ", row.Depth)
		label := indent + row.Label
		if row.Depth == 0 {
			// Account header: bracketed in its tint like the sidebar
			// (FR-C5), name in the header style.
			cap := th.Muted.Render("⟨")
			if idx, ok := st.AccountIndex[row.Account]; ok {
				cap = th.AccountTint(idx).Foreground(th.P.HeaderFg).Render("⟨")
			}
			label = cap + th.SidebarLabel.Render(truncate(row.Label, max(w-4, 1))) + th.Muted.Render("⟩")
		}
		count := th.Muted.Render(pad(itoa(row.Count), 4))
		w6 := max(w-6, 1)
		line := truncate(pad(label, w6), w6) + count
		if i == c.BookSel {
			b.WriteString(th.RowSel.Render(pad(stripStyles(line), w-1)))
		} else {
			b.WriteString(th.Row.Render(line))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderContactList is the middle column: tint bar + name + email, with
// the selection wash and a scroll window around the cursor.
func renderContactList(w, h int, c *ContactsView, st State) string {
	th := st.Theme
	var b strings.Builder
	b.WriteString(ruleFor(w, c.Focus == ContactList, th))
	b.WriteString("\n")
	avail := h - 1
	if avail < 1 {
		return b.String()
	}

	if c.Loading && len(c.Rows) == 0 {
		b.WriteString(th.Muted.Render(truncate("loading contacts…", w-2)))
		b.WriteString("\n")
		return b.String()
	}
	if len(c.Rows) == 0 {
		empty := "no contacts"
		if c.Filter != "" {
			empty = "no matches"
		}
		b.WriteString(th.Muted.Render(truncate(empty+" — n adds one", w-2)))
		b.WriteString("\n")
		return b.String()
	}

	start := 0
	if c.RowSel >= avail {
		start = c.RowSel - avail + 1
	}
	nameW := max((w-3)/2, 8)
	for i := start; i < len(c.Rows) && i-start < avail; i++ {
		r := c.Rows[i]
		var line strings.Builder
		if tint, ok := contactTint(th, st, r.Account); ok {
			line.WriteString(tint.Render(" "))
		} else {
			line.WriteString(" ")
		}
		line.WriteString(pad(r.Name, nameW))
		line.WriteString(truncate(r.Email, max(w-2-nameW, 1)))
		if i == c.RowSel {
			b.WriteString(th.RowSel.Render(pad(stripStyles(line.String()), w-1)))
		} else {
			b.WriteString(th.Row.Render(line.String()))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderContactDetail is the right column: the selected contact's fields.
func renderContactDetail(w, h int, c *ContactsView, th Theme) string {
	var b strings.Builder
	b.WriteString(ruleFor(w, c.Focus == ContactDetail, th))
	b.WriteString("\n")
	avail := h - 1
	if avail < 1 {
		return b.String()
	}
	d := c.Detail
	if d == nil {
		b.WriteString(th.Muted.Render(truncate("select a contact", w-2)))
		b.WriteString("\n")
		return b.String()
	}
	write := func(label, value string) {
		if value == "" {
			return
		}
		b.WriteString(th.Muted.Render(pad(label, 8)))
		b.WriteString(truncate(value, max(w-9, 1)))
		b.WriteString("\n")
	}
	b.WriteString(th.Header.Render(truncate(d.Name, w-2)))
	b.WriteString("\n")
	for _, e := range d.Emails {
		write("email", e)
	}
	for _, p := range d.Phones {
		write("phone", p)
	}
	write("org", d.Org)
	write("title", d.Title)
	if d.Note != "" {
		b.WriteString(th.Muted.Render("note"))
		b.WriteString("\n")
		b.WriteString(truncate(d.Note, max(w-2, 1)))
		b.WriteString("\n")
	}
	if d.Account != "" {
		b.WriteString("\n" + th.Muted.Render(truncate("· "+d.Account, w-2)))
		b.WriteString("\n")
	}
	return b.String()
}

// contactTint resolves the owner-bar style for a contact row (FR-A5
// wording reused: tint = account identity).
func contactTint(th Theme, st State, acct string) (lipgloss.Style, bool) {
	if acct == "" {
		return lipgloss.Style{}, false
	}
	i, ok := st.AccountIndex[acct]
	if !ok {
		return lipgloss.Style{}, false
	}
	return th.AccountTint(i), true
}

// renderContactForm draws the new/edit modal: labelled field rows with a
// wash on the focused one, the target-book row on create, error and hint.
func renderContactForm(w, h int, st State) string {
	th := st.Theme
	f := st.ContactForm
	if f == nil {
		return ""
	}
	boxW := 62
	rows := len(f.Fields) + 1 // + hint
	if f.Err != "" {
		rows++
	}
	boxH := rows + 3 // title + rule + hint is folded into rows
	boxH = min(boxH, max(h-2, 6))

	var body strings.Builder
	body.WriteString(th.Accent.Render(truncate(f.Title, boxW-2)))
	body.WriteString("\n")
	body.WriteString(th.Rule.Render(strings.Repeat("─", boxW-2)))
	body.WriteString("\n")
	for _, fld := range f.Fields {
		row := pad(fld.Name+":", 11) + fld.View
		if fld.Focused {
			body.WriteString(th.RowSel.Render(pad(truncate(row, boxW-2), boxW-2)))
		} else {
			body.WriteString(truncate(row, boxW-2))
		}
		body.WriteString("\n")
	}
	if f.Err != "" {
		body.WriteString(th.Danger.Render(truncate(f.Err, boxW-2)))
		body.WriteString("\n")
	}
	body.WriteString(th.Muted.Render(truncate(f.Hint, boxW-2)))

	block := lipgloss.NewStyle().Width(boxW).Height(boxH).Render(strings.TrimRight(body.String(), "\n"))
	return centerBlock(w, h, block)
}

// renderContactSuggest draws the recipient dropdown: a small box aligned
// under the focused composer field (FR-L3). The compose frame passes the
// row the box's first line should replace.
func renderContactSuggest(frame string, w, h int, row int, v *ContactSuggestView, th Theme) string {
	boxW := 44
	var body strings.Builder
	if v.Loading {
		body.WriteString(th.Muted.Render(truncate("loading contacts…", boxW-2)))
	} else {
		// Layout: name · email · owner suffix (merged accounts, FR-L3).
		// The suffix only appears when one account needs disambiguating.
		nameW, emailW := 14, 16
		if v.Items[0].Suffix == "" {
			nameW, emailW = 16, boxW-3-16
		}
		for i, it := range v.Items {
			label := it.Name
			if label == "" {
				label = it.Email
			}
			plain := pad(truncate(label, nameW), nameW) + pad(truncate(it.Email, emailW), emailW)
			if it.Suffix != "" {
				plain += " " + truncate(it.Suffix, max(boxW-2-nameW-emailW-1, 1))
			}
			if i == v.Sel {
				body.WriteString(th.RowSel.Render(pad(plain, boxW-2)))
			} else {
				body.WriteString(th.Row.Render(plain))
			}
		}
	}
	box := lipgloss.NewStyle().Width(boxW).Render(strings.TrimRight(body.String(), "\n"))
	return overlayAt(frame, box, w, h, row, composeLabelW)
}

// overlayAt floats box over frame with its first line replacing row,
// left-aligned at padLeft (unlike overlayCenter, which centers both) —
// a dropdown hangs under its field instead of floating mid-screen.
func overlayAt(frame, box string, w, h, row, padLeft int) string {
	lines := strings.Split(padLines(frame, w, h), "\n")
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, ln := range boxLines {
		boxW = max(boxW, lipgloss.Width(ln))
	}
	for i, ln := range boxLines {
		boxLines[i] = ln + strings.Repeat(" ", max(boxW-lipgloss.Width(ln), 0))
	}
	left := max(padLeft, 0)
	for i := range boxLines {
		r := row + i
		if r < 0 || r >= len(lines) {
			continue
		}
		lines[r] = truncate(strings.Repeat(" ", left)+boxLines[i], w)
	}
	return strings.Join(lines, "\n")
}
