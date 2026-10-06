package app

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// linkOpenMsg reports the outcome of a browser launch back to the model.
// The launch runs off the UI thread as a tea.Cmd; only the failure needs
// surfacing (the browser itself is the success signal).
type linkOpenMsg struct {
	url string
	err error
}

// openableLink reports whether raw is a link the app will hand to the
// browser. Only the schemes a mail reader can usefully open are allowed:
// anything else (file:, javascript:, a bare relative path) is refused
// before it reaches the OS opener, so a sender-chosen URL can never ask
// the desktop to run an arbitrary handler.
func openableLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return true
	}
	return false
}

// openLinkPicker lists the installed body's footnote hrefs (FR-E2) so the
// reader can open one in the default browser. Refused with a status when
// the cursor's body is not the one on screen or carries no links.
func (m *Model) openLinkPicker() tea.Cmd {
	acct, row, ok := m.cursorRef()
	if !ok {
		return nil
	}
	if m.rowKey(acct, row.ID) != m.vpBodyID {
		m.err = "message still loading"
		return nil
	}
	if len(m.bodyLinks) == 0 {
		m.err = "no links in this message"
		return nil
	}
	items := make([]ui.PickerItem, len(m.bodyLinks))
	for i, u := range m.bodyLinks {
		// A styled (HTML) body numbers its links in the rendered
		// footnotes, so the rows carry the same number; a plain-text
		// body has no footnotes, so the URL stands alone.
		label := u
		if m.bodyStyled {
			label = fmt.Sprintf("[%d] %s", i+1, u)
		}
		items[i] = ui.PickerItem{ID: mail.ID(u), Label: label}
	}
	m.picker = &pickerState{mode: pickerLink, all: items}
	m.pickerRefilter()
	return nil
}

// openURL validates raw and launches the default browser for it as a
// tea.Cmd. The injected opener (Options.OpenURL) lets tests observe the
// URL without spawning anything; production uses the platform opener.
func (m *Model) openURL(raw string) tea.Cmd {
	if !openableLink(raw) {
		m.err = "can't open link: only http, https and mailto links"
		return nil
	}
	open := m.opts.OpenURL
	if open == nil {
		open = platformOpenURL
	}
	return func() tea.Msg {
		return linkOpenMsg{url: raw, err: open(raw)}
	}
}

// platformOpenURL hands raw to the desktop's default handler. The URL is
// passed as its own argv element (never through a shell), so it cannot be
// interpreted as a command. A nil Std* detaches the child from the TUI's
// terminal, and Wait runs on its own goroutine so the opener never
// becomes a zombie.
func platformOpenURL(raw string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", raw)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
