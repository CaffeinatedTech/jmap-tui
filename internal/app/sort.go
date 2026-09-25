package app

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// sortOrder is one list order (FR-D8): the picker id (doubling as the
// prefs key), its label, and the server-side sort it maps to.
type sortOrder struct {
	id    string
	label string
	crits []mail.SortCriterion
}

// sortOrders is the sort picker's catalogue. "newest" maps to nil — the
// RFC 8621 default (receivedAt descending) — so the default account
// sends no sort on the wire at all.
var sortOrders = []sortOrder{
	{id: "newest", label: "newest first"},
	{id: "oldest", label: "oldest first", crits: []mail.SortCriterion{{Property: "receivedAt"}}},
	{id: "sender", label: "by sender", crits: []mail.SortCriterion{{Property: "from"}}},
	{id: "subject", label: "by subject", crits: []mail.SortCriterion{{Property: "subject"}}},
	{id: "size", label: "by size (largest first)", crits: []mail.SortCriterion{{Property: "size", IsDescending: true}}},
}

// sortCrits maps a stored order id to its server sort; unknown ids fall
// back to the default (nil = newest first).
func sortCrits(id string) []mail.SortCriterion {
	for _, o := range sortOrders {
		if o.id == id {
			return o.crits
		}
	}
	return nil
}

func orderLabel(id string) string {
	for _, o := range sortOrders {
		if o.id == id {
			return o.label
		}
	}
	return sortOrders[0].label
}

// openSortPicker shows the order chooser over the list (FR-D8). Sort
// belongs to the per-account mailbox view: refused in unified mode (the
// merge stays date-ordered) and over search results (their order stays
// server-default).
func (m *Model) openSortPicker() (tea.Model, tea.Cmd) {
	if m.unified {
		m.err = "leave the unified view to sort"
		return m, nil
	}
	if m.snap.SearchActive {
		m.err = "sort applies to mailboxes, not search results"
		return m, nil
	}
	m.picker = &pickerState{mode: pickerSort}
	m.picker.all = make([]ui.PickerItem, 0, len(sortOrders))
	cur := m.sortByID[m.activeID]
	if cur == "" {
		cur = sortOrders[0].id
	}
	for i, o := range sortOrders {
		if o.id == cur {
			m.picker.sel = i
		}
		m.picker.all = append(m.picker.all, ui.PickerItem{ID: mail.ID(o.id), Label: o.label})
	}
	m.pickerRefilter()
	return m, nil
}

// chooseSort applies the picked order: engine re-query, remembered in
// prefs (per account), and an action receipt (FR-G5).
func (m *Model) chooseSort(order string) tea.Cmd {
	if m.sortByID == nil {
		m.sortByID = map[string]string{}
	}
	m.sortByID[m.activeID] = order
	if m.opts.Prefs != nil {
		m.opts.Prefs.SetSort(m.activeID, order)
		if err := savePrefs(m.opts, m.activeID); err != nil {
			m.err = truncateErr("prefs", err)
		}
	}
	label := orderLabel(order)
	return tea.Batch(
		m.showToast("Sorted "+label, "", nil, nil),
		m.opOn(m.activeID, "set-sort", func(ctx context.Context, eng *sync.Engine) (sync.Snapshot, error) {
			if err := eng.SetSort(ctx, sortCrits(order)); err != nil {
				return sync.Snapshot{}, err
			}
			return eng.Snapshot(), nil
		}),
	)
}
