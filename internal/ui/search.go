package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// SearchView is the query-bar line state (FR-F1): what the header shows
// while a search view is open. The app renders the live cursor into Query
// via the textinput view; Tokens carry the advanced fields.
type SearchView struct {
	Query  string
	Tokens []string // advanced-field tokens, e.g. "from:x@y"
	Scope  string   // "Inbox" or "all mailboxes"
}

// AdvField is one labelled input row of the advanced-search modal (FR-F2).
type AdvField struct {
	Name    string
	View    string // pre-rendered input
	Focused bool
}

// AdvSearchView is the advanced-search modal's render state (FR-F2).
type AdvSearchView struct {
	Title  string
	Fields []AdvField
	Attach bool // has-attachment checkbox
	Err    string
}

// renderAdvSearch draws the fielded search form: one labelled row per
// field, an attachment toggle, and the enter/esc hint. The selection row
// gets the wash; focused inputs render their own cursor.
func renderAdvSearch(w, h int, st State) string {
	th := st.Theme
	av := st.AdvSearch

	boxW := 56
	boxH := len(av.Fields) + 5 + len(av.ErrLine())
	boxH = min(boxH, max(h-2, 6))

	var body strings.Builder
	body.WriteString(th.Accent.Render(truncate(av.Title, boxW-2)))
	body.WriteString("\n")
	body.WriteString(th.Rule.Render(strings.Repeat("─", boxW-2)))
	body.WriteString("\n")
	for _, f := range av.Fields {
		row := pad(f.Name+":", 9) + f.View
		if f.Focused {
			body.WriteString(th.RowSel.Render(pad(truncate(row, boxW-2), boxW-2)))
		} else {
			body.WriteString(truncate(row, boxW-2))
		}
		body.WriteString("\n")
	}
	check := " "
	if av.Attach {
		check = th.Accent.Render("×")
	}
	body.WriteString(pad("attachments:", 9) + "[" + check + "]  space toggles\n")
	if av.Err != "" {
		body.WriteString(th.Danger.Render(truncate(av.Err, boxW-2)))
		body.WriteString("\n")
	}
	body.WriteString(th.Muted.Render(truncate("up/down field · tab next · enter search · esc cancel", boxW-2)))

	block := lipgloss.NewStyle().Width(boxW).Height(boxH).Render(strings.TrimRight(body.String(), "\n"))
	return centerBlock(w, h, block)
}

// ErrLine keeps the box height math honest when an error line renders.
func (av *AdvSearchView) ErrLine() []string {
	if av.Err == "" {
		return nil
	}
	return []string{av.Err}
}
