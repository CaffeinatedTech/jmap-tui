package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// WizardView is the account wizard's render state (FR-I8): one screen per
// step — the credentials form, the connection test, the mailbox choice,
// and the saved summary. Like every ui view it is a pure value: the
// command layer owns the inputs and hands over rendered strings, so
// golden tests can draw any step deterministically.
type WizardView struct {
	// Title is the screen headline ("Add an account").
	Title string

	// Step is the muted breadcrumb under the title ("2 of 4 · connection").
	Step string

	// Fields renders the form step: one row per input, in tab order.
	Fields []WizardField

	// Items/Sel render the mailbox step as a picker list.
	Items []PickerItem
	Sel   int

	// Status is the async line during connection/save (spinner included);
	// Err, when set, renders in danger under the body.
	Status string
	Err    string

	// Lines render the summary step as label/value pairs.
	Lines []string

	// Hint is the muted key-hint footer.
	Hint string
}

// WizardField is one form row. Value is already the final rendered text —
// mask and cursor applied by the command layer's text input — so the view
// never touches input state.
type WizardField struct {
	Label string
	Value string
	// Placeholder renders when Value is empty (muted by the view).
	Placeholder string
	Focused     bool
}

// wizardBoxW is the wizard's fixed column count; the picker's 34 is too
// narrow for server URLs, and 58 still centers comfortably at the 59-col
// compact golden size.
const wizardBoxW = 58

// RenderWizard draws the wizard centered in the terminal. Server-influenced
// fields (connection errors, mailbox picker labels) are stripped of control
// characters first — the same render-boundary contract as Render (D-3).
func RenderWizard(w, h int, th Theme, v WizardView) string {
	v = sanitizeWizard(v)
	var body strings.Builder
	body.WriteString(th.Accent.Render(v.Title))
	body.WriteString("\n")
	if v.Step != "" {
		body.WriteString(th.Muted.Render(v.Step))
		body.WriteString("\n")
	}
	body.WriteString(th.Rule.Render(strings.Repeat("─", wizardBoxW-2)))
	body.WriteString("\n")

	switch {
	case len(v.Fields) > 0:
		writeWizardFields(&body, th, v.Fields)
	case len(v.Items) > 0:
		writeWizardItems(&body, th, v)
	case len(v.Lines) > 0:
		writeWizardLines(&body, th, v.Lines)
	}
	if v.Status != "" {
		body.WriteString("\n")
		body.WriteString(v.Status)
		body.WriteString("\n")
	}
	if v.Err != "" {
		// Connection errors carry the cause — wrap them to the box
		// instead of beheading the sentence with a truncation.
		body.WriteString("\n")
		body.WriteString(lipgloss.NewStyle().Foreground(th.P.Danger).
			Width(wizardBoxW - 2).Render(v.Err))
		body.WriteString("\n")
	}
	if v.Hint != "" {
		body.WriteString("\n")
		body.WriteString(th.Muted.Render(truncate(v.Hint, wizardBoxW-2)))
		body.WriteString("\n")
	}

	block := lipgloss.NewStyle().Width(wizardBoxW).
		Render(strings.TrimRight(body.String(), "\n"))
	return centerBlock(w, h, block)
}

// writeWizardFields renders the form: a padded label column and the input
// value, the focused row washed like a picker selection.
func writeWizardFields(b *strings.Builder, th Theme, fields []WizardField) {
	const labelW = 15
	for _, f := range fields {
		label := pad(truncate(f.Label, labelW-1), labelW)
		var val string
		switch {
		case f.Value != "":
			val = f.Value
		case f.Placeholder != "":
			val = th.Muted.Render(f.Placeholder)
		}
		if f.Focused {
			b.WriteString(th.RowSel.Render(pad(label+val, wizardBoxW-2)))
		} else {
			b.WriteString(th.Muted.Render(label) + val)
		}
		b.WriteString("\n")
	}
}

// writeWizardItems renders the mailbox step as the picker list with a
// selection wash and type-free navigation.
func writeWizardItems(b *strings.Builder, th Theme, v WizardView) {
	shown := 12
	start := 0
	if v.Sel >= shown {
		start = v.Sel - shown + 1
	}
	for i := start; i < len(v.Items) && i-start < shown; i++ {
		it := v.Items[i]
		line := truncate(strings.Repeat("  ", it.Depth)+it.Label, wizardBoxW-4)
		if i == v.Sel {
			b.WriteString(th.RowSel.Render(pad(line, wizardBoxW-2)))
		} else {
			b.WriteString(th.Row.Render(line))
		}
		b.WriteString("\n")
	}
}

// writeWizardLines renders the summary as a muted label column and value
// text.
func writeWizardLines(b *strings.Builder, th Theme, lines []string) {
	const labelW = 12
	for _, ln := range lines {
		key, val, _ := strings.Cut(ln, "\t")
		b.WriteString(th.Muted.Render(pad(truncate(key, labelW-1), labelW)))
		b.WriteString(th.Header.Render(truncate(val, wizardBoxW-2-labelW)))
		b.WriteString("\n")
	}
}
