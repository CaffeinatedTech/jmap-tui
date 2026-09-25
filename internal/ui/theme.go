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

	// Accounts are the account-identity tints: the unified-inbox owner
	// bar (FR-A5) and the sidebar account-header end-caps (FR-C5). They
	// are consumed in enrollment order and cycled past the end — semantic
	// like Danger and Success, the one deliberate widening of the
	// single-accent palette.
	Accounts []color.Color
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
		// Owner bars: six hues beside the blue accent, all distinguishable
		// as single-cell background strips on dark.
		Accounts: []color.Color{
			lipgloss.Color("#f7768e"), // rose
			lipgloss.Color("#9ece6a"), // green
			lipgloss.Color("#e0af68"), // amber
			lipgloss.Color("#bb9af7"), // violet
			lipgloss.Color("#7dcfff"), // cyan
			lipgloss.Color("#ff9e64"), // orange
		},
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
		// Same six hues, deepened so the bars read on light paper.
		Accounts: []color.Color{
			lipgloss.Color("#c2485d"),
			lipgloss.Color("#4e9e63"),
			lipgloss.Color("#b87a2e"),
			lipgloss.Color("#8457c6"),
			lipgloss.Color("#2e8ba3"),
			lipgloss.Color("#d1663a"),
		},
	}
}

// Theme bundles resolved colours with the style rules derived from them.
type Theme struct {
	P Palette

	Row       lipgloss.Style
	RowSel    lipgloss.Style
	RowSelDim lipgloss.Style
	Muted     lipgloss.Style
	Accent    lipgloss.Style
	Header    lipgloss.Style
	Rule      lipgloss.Style
	// RuleActive is the top rule of the focused column: heavy glyph plus
	// the accent, so focus reads without relying on colour alone.
	RuleActive lipgloss.Style
	// SidebarLabel styles an account header in the folder column — bold
	// header text; the end-caps are the account's own tint (FR-C5,
	// painted as background-filled cells by sidebarLabel).
	SidebarLabel lipgloss.Style
	HelpKey      lipgloss.Style
	HelpDesc     lipgloss.Style
	Attachment   lipgloss.Style
	Danger       lipgloss.Style
	Success      lipgloss.Style
	FreshRow     lipgloss.Style
}

// AccountTint is the owner-bar style for a unified row (FR-A5): a
// background-filled cell the selection wash cannot repaint. The index
// wraps, so accounts past the palette cycle onto earlier tints.
func (t Theme) AccountTint(i int) lipgloss.Style {
	n := len(t.P.Accounts)
	if n == 0 {
		return lipgloss.NewStyle()
	}
	i = ((i % n) + n) % n
	return lipgloss.NewStyle().Background(t.P.Accounts[i])
}

// NewTheme derives the style set from a palette.
func NewTheme(p Palette) Theme {
	base := lipgloss.NewStyle().Foreground(p.BodyFg)
	return Theme{
		P:            p,
		Row:          base,
		RowSel:       base.Background(p.Selected).Foreground(p.SelectedFg),
		RowSelDim:    lipgloss.NewStyle().Foreground(p.Muted).Background(p.Selected),
		Muted:        lipgloss.NewStyle().Foreground(p.Muted),
		Accent:       lipgloss.NewStyle().Foreground(p.Accent),
		Header:       lipgloss.NewStyle().Foreground(p.HeaderFg),
		Rule:         lipgloss.NewStyle().Foreground(p.Rule),
		RuleActive:   lipgloss.NewStyle().Foreground(p.Accent),
		SidebarLabel: lipgloss.NewStyle().Foreground(p.HeaderFg).Bold(true),
		HelpKey:      lipgloss.NewStyle().Foreground(p.Accent),
		HelpDesc:     lipgloss.NewStyle().Foreground(p.BodyFg),
		Attachment:   lipgloss.NewStyle().Foreground(p.Muted),
		Danger:       lipgloss.NewStyle().Foreground(p.Danger),
		Success:      lipgloss.NewStyle().Foreground(p.Success),
		FreshRow:     base.Background(p.Selected),
	}
}
