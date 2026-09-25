package ui

import (
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// TestAccountTintsDistinctAndCycle: every theme ships six pairwise
// distinct owner-bar tints (FR-A5), the index wraps for accounts past the
// palette, and negatives cannot panic the renderer.
func TestAccountTintsDistinctAndCycle(t *testing.T) {
	for _, pal := range []Palette{DarkTheme(), LightTheme()} {
		th := NewTheme(pal)
		if len(pal.Accounts) != 6 {
			t.Errorf("tints = %d, want 6", len(pal.Accounts))
		}
		seen := map[string]bool{}
		for i := range pal.Accounts {
			s := th.AccountTint(i).Render("  ")
			if s == "" || seen[s] {
				t.Errorf("tint %d is empty or a duplicate", i)
			}
			seen[s] = true
		}
		if got, want := th.AccountTint(len(pal.Accounts)).Render("  "), th.AccountTint(0).Render("  "); got != want {
			t.Error("index past the palette does not cycle")
		}
		if got, want := th.AccountTint(-1).Render("  "), th.AccountTint(len(pal.Accounts)-1).Render("  "); got != want {
			t.Error("negative index should wrap, not panic")
		}
	}
}

// TestUnifiedOwnerTint: unified rows resolve their bar from the frame's
// account index; an unknown owner falls back to a gap so the column never
// collapses (FR-A5).
func TestUnifiedOwnerTint(t *testing.T) {
	st := State{
		Theme:        NewTheme(DarkTheme()),
		Unified:      true,
		AccountIndex: map[string]int{"work": 0, "personal": 1},
	}
	if _, ok := st.tint(sync.Row{Account: "work"}); !ok {
		t.Error("work bar missing")
	}
	if _, ok := st.tint(sync.Row{Account: "ghost"}); ok {
		t.Error("unknown account must fall back to a gap")
	}
	if _, ok := st.tint(sync.Row{}); ok {
		t.Error("empty owner must fall back to a gap")
	}
}
