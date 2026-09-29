package ui

import (
	"slices"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/mailtext"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// Sanitize strips control characters from text on its way to the terminal
// (strip silently, single choke point). It delegates to mailtext.Sanitize
// — the canonical implementation lives below the UI so internal/sync and
// HTMLToText can reach it — and exists under this name as the
// display-layer API the render boundary, the smoke dumps, and the stderr
// printer share.
func Sanitize(s string) string { return mailtext.Sanitize(s) }

// SanitizeStyled is Sanitize for styled content: it passes well-formed
// SGR sequences through and strips every other control. The preview
// viewport's frame goes through it (below) because its content now
// carries the renderer's colour codes alongside the sender's text.
func SanitizeStyled(s string) string { return mailtext.SanitizeStyled(s) }

// sanitizeState returns a copy of st whose server- and sender-controlled
// strings are stripped of control characters before any of them can
// reach a frame. Render calls it first, so every slot — list rows, preview
// header, sidebar, footer errors, toasts, and every modal — is covered by
// one contract.
//
// Fields that hold *pre-rendered widget output* (Compose.Body, the
// AdvField/WizardField Value rows, FilePick.View, and app-themed hints)
// are deliberately not stripped here: their content is sanitized at its
// source instead, because blanket-stripping would also destroy the cursor
// and key-hint styling the widgets emit.
func sanitizeState(st State) State {
	// The body frame is styled: its content is the renderer's SGR plus
	// sender text, so it goes through the SGR-preserving policy.
	st.VpView = SanitizeStyled(st.VpView)
	st.Err = Sanitize(st.Err)
	st.Toast = Sanitize(st.Toast)
	st.ToastHint = Sanitize(st.ToastHint)
	st.Account = Sanitize(st.Account)

	st.SidebarRows, _ = cowMap(st.SidebarRows, func(r *SidebarRow) bool {
		return strip(&r.Name)
	})
	st.Accounts, _ = cowMap(st.Accounts, sanitizeAccountView)
	if st.AccountNames != nil {
		st.AccountNames, _ = sanitizeStringMap(st.AccountNames)
	}

	st.Snap = sanitizeSnapshot(st.Snap)

	if st.Picker != nil {
		p := *st.Picker
		p.Title = Sanitize(p.Title)
		p.Filter = Sanitize(p.Filter)
		p.Items, _ = cowMap(p.Items, func(it *PickerItem) bool {
			return strip(&it.Label)
		})
		st.Picker = &p
	}

	if st.FilePick != nil {
		f := *st.FilePick
		f.Title = Sanitize(f.Title)
		f.Path = Sanitize(f.Path)
		f.Hint = Sanitize(f.Hint)
		// f.View is the pre-rendered filepicker body; it browses the
		// local filesystem, not mail, and carries widget styling.
		st.FilePick = &f
	}

	if st.Search != nil {
		q := *st.Search
		q.Query = Sanitize(q.Query)
		q.Scope = Sanitize(q.Scope)
		q.Tokens, _ = cowMap(q.Tokens, strip)
		st.Search = &q
	}

	if st.AdvSearch != nil {
		a := *st.AdvSearch
		a.Title = Sanitize(a.Title)
		a.Err = Sanitize(a.Err)
		a.Fields, _ = cowMap(a.Fields, func(f *AdvField) bool {
			// f.View is the input widget with its cursor styling;
			// its content is the user's own query text.
			return strip(&f.Name)
		})
		st.AdvSearch = &a
	}

	if st.Compose != nil {
		c := *st.Compose
		c.Title = Sanitize(c.Title)
		c.From = Sanitize(c.From)
		c.To = Sanitize(c.To)
		c.Cc = Sanitize(c.Cc)
		c.Bcc = Sanitize(c.Bcc)
		c.Subject = Sanitize(c.Subject)
		c.Status = Sanitize(c.Status)
		c.Attachments, _ = cowMap(c.Attachments, func(a *ComposeAttachment) bool {
			changed := strip(&a.Label)
			if strip(&a.Progress) {
				changed = true
			}
			if strip(&a.Failed) {
				changed = true
			}
			return changed
		})
		if c.Discard != nil {
			d := *c.Discard
			d.Title = Sanitize(d.Title)
			d.Hint = Sanitize(d.Hint)
			c.Discard = &d
		}
		if c.Suggest != nil {
			s := *c.Suggest
			s.Items, _ = cowMap(s.Items, func(it *ContactSuggestItem) bool {
				changed := strip(&it.Name)
				if strip(&it.Email) {
					changed = true
				}
				if strip(&it.Suffix) {
					changed = true
				}
				return changed
			})
			c.Suggest = &s
		}
		// c.Body is the textarea's rendered view (cursor styling); the
		// quoted/draft text it displays is stripped when it is set
		// (app.fillReply / handleComposePrep). c.Hint is app chrome.
		st.Compose = &c
	}

	if st.AccountSwitch != nil {
		sw := *st.AccountSwitch
		sw.Accounts, _ = cowMap(sw.Accounts, sanitizeAccountView)
		st.AccountSwitch = &sw
	}

	if st.Contacts != nil {
		cv := *st.Contacts
		cv.Scope = Sanitize(cv.Scope)
		cv.Filter = Sanitize(cv.Filter)
		cv.Err = Sanitize(cv.Err)
		cv.Books, _ = cowMap(cv.Books, func(b *ContactBookRow) bool {
			return strip(&b.Label)
		})
		cv.Rows, _ = cowMap(cv.Rows, func(r *ContactRow) bool {
			changed := strip(&r.Name)
			if strip(&r.Email) {
				changed = true
			}
			return changed
		})
		if cv.Detail != nil {
			d := *cv.Detail
			d.Name = Sanitize(d.Name)
			d.Account = Sanitize(d.Account)
			d.Org = Sanitize(d.Org)
			d.Title = Sanitize(d.Title)
			d.Note = Sanitize(d.Note)
			d.Emails, _ = cowMap(d.Emails, strip)
			d.Phones, _ = cowMap(d.Phones, strip)
			cv.Detail = &d
		}
		st.Contacts = &cv
	}

	if st.ContactForm != nil {
		cf := *st.ContactForm
		cf.Title = Sanitize(cf.Title)
		cf.Err = Sanitize(cf.Err)
		cf.Hint = Sanitize(cf.Hint)
		cf.Fields, _ = cowMap(cf.Fields, func(f *AdvField) bool {
			return strip(&f.Name)
		})
		st.ContactForm = &cf
	}

	// HelpSec is generated from the keymap (app config and code), and
	// Theme is styles only — neither carries server text.
	return st
}

// sanitizeSnapshot strips the sync snapshot's server-controlled strings.
// The snapshot's slices are shared with the app (and were published by the
// engine), so every slice goes through cowMap and is only ever replaced,
// never edited in place. Snap.Body.Text is intentionally skipped: it is
// never rendered directly (the frame shows VpView, sanitized above) and
// can be megabytes — scanning it every frame would cost more than it
// protects.
func sanitizeSnapshot(s sync.Snapshot) sync.Snapshot {
	s.Mailboxes, _ = cowMap(s.Mailboxes, func(n *sync.MailboxNode) bool {
		changed := strip(&n.Mailbox.Name)
		if r := Sanitize(string(n.Mailbox.Role)); r != string(n.Mailbox.Role) {
			n.Mailbox.Role = mail.Role(r)
			changed = true
		}
		return changed
	})
	s.Rows, _ = cowMap(s.Rows, sanitizeRow)
	s.Status.LastError = Sanitize(s.Status.LastError)
	if s.Body != nil {
		b := *s.Body
		b.Attachments, _ = cowMap(b.Attachments, func(a *mail.Attachment) bool {
			changed := strip(&a.Name)
			if strip(&a.Type) {
				changed = true
			}
			return changed
		})
		s.Body = &b
	}
	return s
}

// sanitizeRow strips one message-list row's summary text. It edits only
// the copy it is given; nested slices are replaced via cowMap.
func sanitizeRow(r *sync.Row) (changed bool) {
	if s := Sanitize(r.Summary.Subject); s != r.Summary.Subject {
		r.Summary.Subject = s
		changed = true
	}
	if s := Sanitize(r.Summary.Preview); s != r.Summary.Preview {
		r.Summary.Preview = s
		changed = true
	}
	if from, ch := cowMap(r.Summary.From, sanitizeAddress); ch {
		r.Summary.From = from
		changed = true
	}
	if to, ch := cowMap(r.Summary.To, sanitizeAddress); ch {
		r.Summary.To = to
		changed = true
	}
	return changed
}

func sanitizeAddress(a *mail.Address) (changed bool) {
	if s := Sanitize(a.Name); s != a.Name {
		a.Name = s
		changed = true
	}
	if s := Sanitize(a.Email); s != a.Email {
		a.Email = s
		changed = true
	}
	return changed
}

func sanitizeAccountView(a *AccountView) (changed bool) {
	if s := Sanitize(a.Name); s != a.Name {
		a.Name = s
		changed = true
	}
	if s := Sanitize(a.LastError); s != a.LastError {
		a.LastError = s
		changed = true
	}
	return changed
}

// sanitizeWizard strips the wizard view's server-influenced raw fields
// (connection errors carry ServerError detail; picker items are mailbox
// names). WizardField.Value and Status embed input-cursor/spinner styling
// and are sanitized at their source instead (cmd layer).
func sanitizeWizard(v WizardView) WizardView {
	v.Title = Sanitize(v.Title)
	v.Step = Sanitize(v.Step)
	v.Err = Sanitize(v.Err)
	v.Hint = Sanitize(v.Hint)
	v.Lines, _ = cowMap(v.Lines, strip)
	v.Items, _ = cowMap(v.Items, func(it *PickerItem) bool {
		return strip(&it.Label)
	})
	v.Fields, _ = cowMap(v.Fields, func(f *WizardField) bool {
		changed := strip(&f.Label)
		if strip(&f.Placeholder) {
			changed = true
		}
		return changed
	})
	return v
}

// strip sanitizes the string s points at and reports whether anything
// changed.
func strip(s *string) bool {
	t := Sanitize(*s)
	if t == *s {
		return false
	}
	*s = t
	return true
}

// cowMap applies f to a copy of each element, returning a new slice only
// when f reports a change — the snapshot slices are shared immutables, so
// the common (control-free) path must not copy and the dirty path must
// never write through the original backing array.
func cowMap[T any](in []T, f func(*T) bool) ([]T, bool) {
	if len(in) == 0 {
		return in, false
	}
	out := in
	changed := false
	for i := range in {
		v := in[i]
		if f(&v) {
			if !changed {
				out = slices.Clone(in)
			}
			changed = true
			out[i] = v
		}
	}
	return out, changed
}

// sanitizeStringMap strips the values of a string map (account display
// names), cloning only when one needed fixing.
func sanitizeStringMap(in map[string]string) (map[string]string, bool) {
	out := in
	changed := false
	for k, v := range in {
		t := Sanitize(v)
		if t == v {
			continue
		}
		if !changed {
			out = make(map[string]string, len(in))
			for k2, v2 := range in {
				out[k2] = v2
			}
		}
		changed = true
		out[k] = t
	}
	return out, changed
}
