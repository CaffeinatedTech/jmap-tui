package ui

import (
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

// TestThreadSlot pins FR-D1's mark: a collapsed thread with replies wears
// ▸ — that row is the one Enter expands — an open thread wears ▾, a member
// nothing (its indent nests it under the header), and a single-message
// thread, an unsized row, or a flat fuzzy-scan row nothing at all. Every
// slot is exactly two cells, so a chevron never moves the subject column.
func TestThreadSlot(t *testing.T) {
	cases := []struct {
		name string
		row  sync.Row
		want string
	}{
		{"collapsed thread with replies", sync.Row{ThreadSize: 3}, "▸ "},
		{"open thread header", sync.Row{ThreadSize: 2, ThreadHeader: true}, "▾ "},
		{"member of an open thread", sync.Row{ThreadSize: 2, ThreadMember: true}, "  "},
		{"single-message thread", sync.Row{ThreadSize: 1}, "  "},
		{"size not fetched yet", sync.Row{ThreadSize: 0}, "  "},
		{"flat fuzzy-scan row", sync.Row{ThreadSize: -1}, "  "},
	}
	for _, tc := range cases {
		got := threadSlot(tc.row)
		if got != tc.want {
			t.Errorf("%s: slot = %q, want %q", tc.name, got, tc.want)
		}
		if w := runewidth.StringWidth(got); w != 2 {
			t.Errorf("%s: slot width = %d, want 2", tc.name, w)
		}
	}
}
