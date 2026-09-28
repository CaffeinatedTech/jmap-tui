package ui

// Audit probes for the terminal escape corpus.
// Strategy: render frames whose server-controlled fields carry hostile
// control sequences, then assert the rendered frame contains NO C0/C1
// controls other than '\n', and no OSC sequences at all. The legitimate
// style vocabulary (verified against golden frames) is CSI SGR only:
// ESC [ … m — so any other escape in the output is attacker-controlled.
// A FAIL means attacker-controlled escapes reached that sink.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hostile payloads: CSI screen/scroll/control, OSC title, OSC-8 hyperlink,
// OSC-52 clipboard, CR/BS/BEL line rewrites, C1 CSI, bidi overrides.
var auditPayloads = []struct{ name, payload string }{
	{"csi-clear-screen", "\x1b[2J"},
	{"csi-cursor-home", "\x1b[1;1H"},
	{"csi-alt-screen", "\x1b[?1049h"},
	{"csi-save-cursor", "\x1b[s"},
	{"osc-title", "\x1b]0;owned by sender\x07"},
	{"osc8-hyperlink", "\x1b]8;;https://evil.test/phish\x07CLICK HERE\x1b]8;;\x07"},
	{"osc52-clipboard", "\x1b]52;c;cmVxdWVzdGVk\x07"},
	{"cr-line-rewrite", "trusted\rEVIL LINE"},
	{"bs-line-rewrite", "trusted\bEVIL"},
	{"bel-flood", "ding\x07\x07\x07"},
	{"c1-csi", "\x9b2J"},
	{"csi-k-reset", "\x1b[K"},
	{"csi-sgr-spoof", "\x1b[38;2;255;0;0mFAKE ERROR"},
	{"bidi-override", "invoice\u202egnp.exe"},
	{"zero-width", "pass\u200bword"},
	{"nul-byte", "a\x00b"},
}

// auditCheckFrame asserts the frame carries no attacker-controllable
// control bytes: allowed output controls are '\n' (row separator) and the
// SGR sequences lipgloss emits for chrome. Everything else — bare ESC,
// other CSI finals, OSC, C0 (minus \n), C1, NUL — is a finding.
func auditCheckFrame(t *testing.T, sink, frame string) {
	t.Helper()
	if !utf8.ValidString(frame) {
		t.Errorf("[%s] frame is not valid UTF-8 (truncation splits runes)", sink)
	}
	for i := 0; i < len(frame); {
		b := frame[i]
		if b < utf8.RuneSelf { // ASCII byte
			switch {
			case b == '\n' || b == '\t':
				i++
			case b == '\x1b':
				// Must be CSI SGR: ESC [ … m; returns index past it.
				n, ok := auditSGRLen(frame[i:])
				if !ok {
					t.Errorf("[%s] non-SGR escape reached the frame at offset %d: %q…", sink, i, safeSnippet(frame, i))
					return
				}
				i += n
			case b < 0x20 || b == 0x7f:
				t.Errorf("[%s] control byte %#x reached the frame at offset %d: %q…", sink, b, i, safeSnippet(frame, i))
				return
			default:
				i++
			}
			continue
		}
		// Multi-byte rune: decode it so UTF-8 continuation bytes are not
		// misread as C1 controls.
		r, size := utf8.DecodeRuneInString(frame[i:])
		if r == utf8.RuneError && size == 1 {
			t.Errorf("[%s] invalid UTF-8 rune at offset %d", sink, i)
			return
		}
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("[%s] C1 control %#x reached the frame at offset %d", sink, r, i)
			return
		}
		// Sanitize also strips bidi controls and zero-width characters.
		if isAuditFormatControl(r) {
			t.Errorf("[%s] format control %#x reached the frame at offset %d: %q…", sink, r, i, safeSnippet(frame, i))
			return
		}
		i += size
	}
}

// auditSGRLen reports the byte length of the CSI SGR sequence starting at
// s[0] == ESC, and whether s is exactly that (ESC [ … m).
func auditSGRLen(s string) (int, bool) {
	if len(s) < 3 || s[1] != '[' {
		return 0, false
	}
	for j := 2; j < len(s); j++ {
		c := s[j]
		if (c >= '0' && c <= '9') || c == ';' || c == '?' {
			continue
		}
		if c == 'm' {
			return j + 1, true
		}
		return 0, false
	}
	return 0, false
}

// isAuditFormatControl reports r as a character Sanitize strips:
// bidi embedding/override/isolate, zero-width chars, BOM, LRM/RLM.
func isAuditFormatControl(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, // bidi embedding/override
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r >= 0x200b && r <= 0x200f, // zero-width + LRM/RLM
		r == 0xfeff,                // BOM/zero-width no-break
		r == 0x061c:                // Arabic letter mark
		return true
	}
	return false
}

func safeSnippet(s string, off int) string {
	start := off - 10
	if start < 0 {
		start = 0
	}
	end := off + 30
	if end > len(s) {
		end = len(s)
	}
	return strings.ReplaceAll(strings.ReplaceAll(s[start:end], "\x1b", "^["), "\x07", "^G")
}

// injectPayload returns a base state with the payload placed in the given
// server-controlled slot.
func auditState(slot, payload string) State {
	st := baseState()
	switch slot {
	case "subject":
		st.Snap.Rows[0].Summary.Subject = payload
	case "from-name":
		st.Snap.Rows[0].Summary.From[0].Name = payload
	case "from-email":
		st.Snap.Rows[0].Summary.From[0].Email = payload
	case "preview":
		st.Snap.Rows[0].Summary.Preview = payload
	case "mailbox-name":
		st.Snap.Mailboxes[0].Mailbox.Name = payload
		st.SidebarRows[1].Name = payload
	case "account-name":
		st.Account = payload
		st.SidebarRows[0].Name = payload
	case "body":
		st.VpView = payload + "\nsecond line"
	case "err":
		st.Err = payload
	case "toast":
		st.Toast = payload
		st.ToastHint = "z undo"
	case "search-query":
		st.Search = &SearchView{Query: payload, Scope: "Inbox"}
	}
	return st
}

var auditSlots = []string{
	"subject", "from-name", "from-email", "preview",
	"mailbox-name", "account-name", "body", "err", "toast", "search-query",
}

func TestAuditT1RenderBoundaryStripsControls(t *testing.T) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"wide", 120, 40},
		{"medium", 99, 35},
		{"compact", 59, 25},
	}
	for _, slot := range auditSlots {
		for _, p := range auditPayloads {
			for _, sz := range sizes {
				st := auditState(slot, p.payload)
				st.Focus = PaneList
				frame := Render(sz.w, sz.h, st)
				auditCheckFrame(t, slot+"/"+p.name+"/"+sz.name, frame)
			}
		}
	}
}

// Preview-focused frames render the full From/To/Subject header and the
// body viewport — the richest sink for hostile content.
func TestAuditT1PreviewPaneStripsControls(t *testing.T) {
	for _, slot := range []string{"subject", "from-name", "body", "err"} {
		for _, p := range auditPayloads {
			st := auditState(slot, p.payload)
			st.Focus = PanePreview
			frame := Render(120, 40, st)
			auditCheckFrame(t, "preview/"+slot+"/"+p.name, frame)
		}
	}
}

// Modal and overlay paths bypass shownPanes and render their own frames.
func TestAuditT1ModalsStripControls(t *testing.T) {
	for _, p := range auditPayloads {
		// Mailbox picker with hostile mailbox labels.
		st := baseState()
		st.Picker = &PickerView{
			Title: "move to",
			Items: []PickerItem{
				{ID: "mb1", Label: p.payload},
				{ID: "mb2", Label: "Inbox"},
			},
			Sel: 0,
		}
		auditCheckFrame(t, "picker/"+p.name, Render(120, 40, st))

		// Account switch overlay with hostile display names.
		st = baseState()
		st.AccountSwitch = &SwitchView{
			Accounts: []AccountView{{ID: "a1", Name: p.payload}},
			Sel:      0,
		}
		auditCheckFrame(t, "switch/"+p.name, Render(120, 40, st))

		// Compose view with hostile recipient/subject fields.
		st = baseState()
		st.Compose = &ComposeView{
			Title:   "new message",
			To:      p.payload,
			Subject: p.payload,
			Status:  p.payload,
		}
		auditCheckFrame(t, "compose/"+p.name, Render(120, 40, st))

		// Contacts list with hostile contact names.
		st = baseState()
		st.Contacts = &ContactsView{
			Rows:   []ContactRow{{Name: p.payload, Email: "a@b.test", Account: "work"}},
			RowSel: 0,
		}
		auditCheckFrame(t, "contacts/"+p.name, Render(120, 40, st))
	}
}

// Header/status-bar error segments: server error strings flow here through
// app.truncateErr (the old byte slicing split runes).
func TestAuditT7ErrorTruncationKeepsUTF8(t *testing.T) {
	// 120+ byte multi-byte string forces s[:117] to split a rune if the
	// truncation is byte-based (app.go:529). We assert the ui side: a
	// frame built from a rune-split error must still be valid UTF-8 once
	// it reaches Render — but the actual slicing lives in app, so this
	// probes the render boundary contract only.
	st := baseState()
	st.Err = strings.Repeat("é", 80) // 160 bytes, no controls
	frame := Render(120, 40, st)
	if !utf8.ValidString(frame) {
		t.Errorf("FINDING T-7: frame invalid UTF-8 — error/subject truncation splits runes")
	}
}
