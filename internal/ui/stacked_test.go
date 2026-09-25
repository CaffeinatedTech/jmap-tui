package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestStackedGeometry: side-by-side splits horizontally, stacked splits the
// shared column down the middle (50/50, FR-I10) — and at medium widths the
// stacked layout shows the list and preview together instead of swapping.
func TestStackedGeometry(t *testing.T) {
	st := baseState()
	st.Focus = PaneList
	st.Stacked = true

	l := ComputeLayout(120, 40, st)
	if l.SidebarW != sidebarWidth {
		t.Errorf("SidebarW = %d, want %d", l.SidebarW, sidebarWidth)
	}
	if l.ListW != 120-sidebarWidth || l.PreviewW != 120-sidebarWidth {
		t.Errorf("stacked widths = %d/%d, want %d each", l.ListW, l.PreviewW, 120-sidebarWidth)
	}
	contentH := frameContentHeight(40, st)
	if l.ListH+l.PreviewH != contentH {
		t.Errorf("split %d+%d != content height %d", l.ListH, l.PreviewH, contentH)
	}
	if l.ListH != contentH/2 || l.PreviewH != contentH-contentH/2 {
		t.Errorf("split %d/%d, want 50/50 of %d", l.ListH, l.PreviewH, contentH)
	}
	if want := l.PreviewH - previewChrome(st); l.BodyH != want {
		t.Errorf("BodyH = %d, want %d (PreviewH - chrome)", l.BodyH, want)
	}

	// Medium width: stacked shows both panes regardless of focus.
	sb, li, pv := shownPanes(99, st)
	if !sb || !li || !pv {
		t.Errorf("medium stacked panes = %v %v %v, want all true", sb, li, pv)
	}
	// Side-by-side keeps the focus swap at the same width.
	st.Stacked = false
	st.Focus = PaneSidebar
	if _, li, pv := shownPanes(99, st); !li || pv {
		t.Errorf("medium side-by-side with sidebar focus = list %v preview %v, want list only", li, pv)
	}
}

// TestStackedRules: the preview's top rule lands exactly at the split line
// and carries the focus accent only when the preview is focused — the
// focus signal works without relying on which pane is horizontally first.
func TestStackedRules(t *testing.T) {
	for _, tc := range []struct {
		focus Pane
		heavy bool
	}{
		{focus: PaneList, heavy: false},
		{focus: PanePreview, heavy: true},
	} {
		st := baseState()
		st.Stacked = true
		st.Focus = tc.focus
		h := 40
		l := ComputeLayout(120, h, st)
		lines := strings.Split(Render(120, h, st), "\n")
		if len(lines) != h {
			t.Fatalf("focus %v: frame has %d lines, want %d", tc.focus, len(lines), h)
		}
		// Line 1: sidebar rule | list rule.
		top := ansi.Strip(lines[1])
		if tc.focus == PaneList && !strings.Contains(top, "━") {
			t.Errorf("list focus: top rule missing heavy list segment: %q", top)
		}
		// Split line: sidebar column | preview rule.
		split := ansi.Strip(lines[1+l.ListH])
		if tc.heavy {
			if !strings.Contains(split, "━") {
				t.Errorf("preview focus: split rule not heavy: %q", split)
			}
		} else if strings.Contains(split, "━") {
			t.Errorf("list focus: split rule must stay a hairline: %q", split)
		}
	}
}
