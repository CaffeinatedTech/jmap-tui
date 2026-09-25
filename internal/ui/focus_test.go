package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// ruleSeg is one column's horizontal extent inside the rule row: content
// width and which pane it belongs to.
type ruleSeg struct {
	focus Pane
	x, w  int
}

// ruleLayout resolves the pane segments of the rule row (line 1 of the
// frame) the same way Render composes them: pane content width minus the
// separator allowance, separators advancing x by the full pane width.
func ruleLayout(w, h int, st State) []ruleSeg {
	l := ComputeLayout(w, h, st)
	sidebar, list, preview := shownPanes(w, st)
	var segs []ruleSeg
	x := 0
	if l.SidebarW > 0 && sidebar {
		segs = append(segs, ruleSeg{PaneSidebar, x, max(l.SidebarW-1, 0)})
		x += l.SidebarW
	}
	if l.ListW > 0 && list {
		segs = append(segs, ruleSeg{PaneList, x, max(l.ListW-1, 0)})
		x += l.ListW
	}
	if l.PreviewW > 0 && preview {
		segs = append(segs, ruleSeg{PanePreview, x, max(l.PreviewW-1, 0)})
	}
	return segs
}

func baseState() State {
	return State{
		Theme:          NewTheme(DarkTheme()),
		Snap:           fixtureSnapshot(),
		SidebarVisible: true,
		Account:        "Work",
		SidebarRows:    fixtureSidebarRows(),
		SidebarSel:     1, // the open Inbox, under Work's header (FR-C5)
		Now:            time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
		VpView:         vpBody,
	}
}

// TestColumnRulesFollowFocus: the top rule of the focused column renders
// heavy (━) in the accent, every other column keeps the hairline (─).
// The glyph is asserted, not just the colour, so the signal survives
// terminals without colour.
func TestColumnRulesFollowFocus(t *testing.T) {
	cases := []struct {
		name  string
		w, h  int
		focus Pane
		fsk   func(*State) // optional state tweak
	}{
		{name: "wide-sidebar", w: 120, h: 40, focus: PaneSidebar},
		{name: "wide-list", w: 120, h: 40, focus: PaneList},
		{name: "wide-preview", w: 120, h: 40, focus: PanePreview},
		{name: "medium-list", w: 99, h: 35, focus: PaneList},
		{name: "medium-preview", w: 99, h: 35, focus: PanePreview},
		{name: "compact-list", w: 59, h: 25, focus: PaneList},
		{name: "compact-sidebar", w: 59, h: 25, focus: PaneSidebar},
		{name: "fullscreen-preview", w: 120, h: 40, focus: PanePreview, fsk: func(st *State) {
			st.Fullscreen = true
		}},
		{name: "search-bar", w: 120, h: 40, focus: PaneList, fsk: func(st *State) {
			st.Search = &SearchView{Query: "audit", Scope: "Inbox"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := baseState()
			st.Focus = tc.focus
			if tc.fsk != nil {
				tc.fsk(&st)
			}
			lines := strings.Split(Render(tc.w, tc.h, st), "\n")
			if len(lines) < 2 {
				t.Fatalf("frame has %d lines, want header + rule row", len(lines))
			}
			rule := ansi.Strip(lines[1])
			cells := []rune(rule)
			segs := ruleLayout(tc.w, tc.h, st)
			if len(segs) == 0 {
				t.Fatal("no panes rendered")
			}
			for _, seg := range segs {
				if seg.x+seg.w > len(cells) {
					t.Fatalf("segment %v beyond rule row width %d", seg, len(cells))
				}
				got := cells[seg.x : seg.x+seg.w]
				heavy, hair := '━', '─'
				want := hair
				if seg.focus == tc.focus {
					want = heavy
				}
				for i, r := range got {
					if r != want {
						t.Errorf("pane %v cell %d: got %q, want %q (rule row %q)", seg.focus, i, r, want, rule)
					}
				}
			}
		})
	}
}

// TestRuleActiveIsAccent: the heavy rule also carries the accent colour —
// colour is the glance, the glyph the fallback.
func TestRuleActiveIsAccent(t *testing.T) {
	th := NewTheme(DarkTheme())
	active := topRule(8, true, th)
	if !strings.Contains(active, "━") {
		t.Errorf("active rule %q missing heavy glyph", active)
	}
	if !strings.Contains(active, "\x1b[38;2;130;170;255m") { // accent #82aaff
		t.Errorf("active rule not accent-coloured: %q", active)
	}
	inactive := topRule(8, false, th)
	if strings.Contains(inactive, "━") {
		t.Errorf("inactive rule %q must stay a hairline", inactive)
	}
	if strings.Contains(inactive, "130;170;255") {
		t.Errorf("inactive rule must not use the accent: %q", inactive)
	}
}

// TestSidebarAccountLabel: the folder column leads with an account
// header — the name bracketed by end-caps filled with that account's
// unified-inbox tint (FR-C5: the header wears the same colour as the
// account's rows in the unified view), in every account mode. The header
// is a real row now, so an empty name still holds its line.
func TestSidebarAccountLabel(t *testing.T) {
	const w, h = 120, 40
	rose := "\x1b[48;2;247;118;142m" // dark palette tint 0
	cases := []struct {
		name, account string
		unified       bool
	}{
		{name: "single", account: "Work"},
		{name: "unified", account: "Work", unified: true},
		{name: "empty-name", account: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := baseState()
			st.Focus = PaneList
			st.Account = tc.account
			var rows []SidebarRow
			if tc.unified {
				st.Snap = fixtureUnifiedSnapshot()
				st.Unified = true
				st.AccountNames = map[string]string{"work": "Work", "personal": "Personal"}
				st.AccountIndex = map[string]int{"work": 0, "personal": 1}
				rows = fixtureMultiSidebarRows()
			} else {
				rows = sidebarRowsFor("work", tc.account, fixtureSnapshot(), 0, true)
			}
			st.SidebarRows = rows
			frame := Render(w, h, st)
			lines := strings.Split(frame, "\n")
			if len(lines) < 3 {
				t.Fatalf("frame has %d lines, want header + rule + label", len(lines))
			}
			// Line 2 leads with the sidebar: strip the pane separator to
			// isolate the header cell. The fold slot (FR-C6) leads the
			// name when the account has folders to fold.
			sidebarCell := strings.SplitN(ansi.Strip(lines[2]), "│", 2)[0]
			want := strings.TrimSpace(foldSlot(rows[0]) + tc.account)
			if got := strings.TrimSpace(sidebarCell); got != want {
				t.Errorf("label = %q, want %q", got, want)
			}
			raw := strings.SplitN(lines[2], "│", 2)[0]
			if !strings.Contains(raw, rose) {
				t.Errorf("header missing tint-0 end-cap background: %q", raw)
			}
		})
	}
}

// TestSidebarHeadersCarryAccountTints: every account header's end-caps
// wear the tint of its unified-inbox row bar (FR-A5 ↔ FR-C5) — identity,
// not position — and the active account's name takes the accent while the
// others keep the header style.
func TestSidebarHeadersCarryAccountTints(t *testing.T) {
	st := baseState()
	st.Focus = PaneList
	st.SidebarRows = fixtureMultiSidebarRows()
	st.SidebarSel = 1 // Inbox: no header selected, styles show through

	lines := strings.Split(Render(120, 40, st), "\n")
	if len(lines) < 11 {
		t.Fatalf("frame has %d lines, want header rows through line 10", len(lines))
	}
	// Row order: Work hdr (line 2), Inbox, Sent, agent-test, Archive,
	// Personal hdr (line 7), its Inbox, Archive, Old laptop hdr (line 10).
	cases := []struct {
		line int
		tint string
		name string
	}{
		{line: 2, tint: "\x1b[48;2;247;118;142m", name: "Work"},        // tint 0 rose
		{line: 7, tint: "\x1b[48;2;158;206;106m", name: "Personal"},    // tint 1 green
		{line: 10, tint: "\x1b[48;2;224;175;104m", name: "Old laptop"}, // tint 2 amber
	}
	for _, tc := range cases {
		raw := strings.SplitN(lines[tc.line], "│", 2)[0]
		if !strings.Contains(raw, tc.tint) {
			t.Errorf("line %d (%s) missing tint %q", tc.line, tc.name, tc.tint)
		}
		if !strings.Contains(ansi.Strip(raw), tc.name) {
			t.Errorf("line %d = %q, want name %q", tc.line, ansi.Strip(raw), tc.name)
		}
	}
	// The active account's name is accent-coloured; a foreign one is not.
	active := strings.SplitN(lines[2], "│", 2)[0]
	if !strings.Contains(active, "\x1b[38;2;130;170;255m") {
		t.Errorf("active account header missing accent name: %q", active)
	}
	inactive := strings.SplitN(lines[7], "│", 2)[0]
	if strings.Contains(inactive, "\x1b[38;2;130;170;255m") {
		t.Errorf("foreign account header must not take the accent: %q", inactive)
	}
}
