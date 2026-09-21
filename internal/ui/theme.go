// Package ui renders the jmap-tui interface: theme, keymap, the three panes
// (sidebar, list, preview), and the help overlay. Rendering is a pure
// function of state — nothing here touches the network or the engine, so
// golden tests can render any state deterministically (FR-D6).
package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette is one resolved colour scheme. The app picks dark or light once
// at startup (FR-I2); explicit palettes keep rendering deterministic and
// golden-testable.
type Palette struct {
	Accent     color.Color
	Unread     color.Color
	Muted      color.Color
	Danger     color.Color
	Success    color.Color
	Selected   color.Color // soft background wash for the selected row
	SelectedFg color.Color
	Rule       color.Color // hairline separators
	HeaderFg   color.Color
	BodyFg     color.Color
}

// DarkTheme is the default palette: one restrained blue accent on dark.
func DarkTheme() Palette {
	return Palette{
		Accent:     lipgloss.Color("#82aaff"),
		Unread:     lipgloss.Color("#82aaff"),
		Muted:      lipgloss.Color("#6b7280"),
		Danger:     lipgloss.Color("#e06c75"),
		Success:    lipgloss.Color("#98c379"),
		Selected:   lipgloss.Color("#24283b"),
		SelectedFg: lipgloss.Color("#c0caf5"),
		Rule:       lipgloss.Color("#3b4261"),
		HeaderFg:   lipgloss.Color("#a9b1d6"),
		BodyFg:     lipgloss.Color("#c0caf5"),
	}
}

// LightTheme mirrors DarkTheme for light terminals.
func LightTheme() Palette {
	return Palette{
		Accent:     lipgloss.Color("#2a5db0"),
		Unread:     lipgloss.Color("#2a5db0"),
		Muted:      lipgloss.Color("#9ca3af"),
		Danger:     lipgloss.Color("#c03538"),
		Success:    lipgloss.Color("#2e7d43"),
		Selected:   lipgloss.Color("#e2e8f0"),
		SelectedFg: lipgloss.Color("#1e293b"),
		Rule:       lipgloss.Color("#d1d5db"),
		HeaderFg:   lipgloss.Color("#374151"),
		BodyFg:     lipgloss.Color("#1f2937"),
	}
}

// Theme bundles resolved colours with the style rules derived from them.
type Theme struct {
	P Palette

	Row        lipgloss.Style
	RowSel     lipgloss.Style
	RowSelDim  lipgloss.Style
	Muted      lipgloss.Style
	Accent     lipgloss.Style
	Header     lipgloss.Style
	Rule       lipgloss.Style
	HelpKey    lipgloss.Style
	HelpDesc   lipgloss.Style
	Attachment lipgloss.Style
	Danger     lipgloss.Style
	Success    lipgloss.Style
	FreshRow   lipgloss.Style
}

// NewTheme derives the style set from a palette.
func NewTheme(p Palette) Theme {
	base := lipgloss.NewStyle().Foreground(p.BodyFg)
	return Theme{
		P:          p,
		Row:        base,
		RowSel:     base.Background(p.Selected).Foreground(p.SelectedFg),
		RowSelDim:  lipgloss.NewStyle().Foreground(p.Muted).Background(p.Selected),
		Muted:      lipgloss.NewStyle().Foreground(p.Muted),
		Accent:     lipgloss.NewStyle().Foreground(p.Accent),
		Header:     lipgloss.NewStyle().Foreground(p.HeaderFg),
		Rule:       lipgloss.NewStyle().Foreground(p.Rule),
		HelpKey:    lipgloss.NewStyle().Foreground(p.Accent),
		HelpDesc:   lipgloss.NewStyle().Foreground(p.BodyFg),
		Attachment: lipgloss.NewStyle().Foreground(p.Muted),
		Danger:     lipgloss.NewStyle().Foreground(p.Danger),
		Success:    lipgloss.NewStyle().Foreground(p.Success),
		FreshRow:   base.Background(p.Selected),
	}
}
