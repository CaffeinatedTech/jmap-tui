package ui

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
	"github.com/CaffeinatedTech/jmap-tui/internal/sync"
)

var update = flag.Bool("update", false, "regenerate golden files")

// fixtureSnapshot exercises every row variant: unread bold row, selected
// row, a collapsed thread that can expand (▸), a single-message thread
// that cannot, expanded thread header + member, replied flag, attachment.
func fixtureSnapshot() sync.Snapshot {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	sum := func(id, threadID, from, subject string, at time.Time, kw ...string) mail.EmailSummary {
		kws := mail.Keywords{}
		for _, k := range kw {
			kws[k] = struct{}{}
		}
		return mail.EmailSummary{
			ID: mail.ID(id), ThreadID: mail.ID(threadID),
			MailboxIDs: []mail.ID{"mb-inbox"},
			Keywords:   kws,
			From:       []mail.Address{{Name: from, Email: "from@example.test"}},
			To:         []mail.Address{{Email: "me@example.test"}},
			Subject:    subject, ReceivedAt: at, Size: 4096,
			Preview: "preview text",
		}
	}
	rows := []sync.Row{
		{ID: "e4", Summary: sum("e4", "t4", "Dana Ops", "Deploy pipeline is green", now.Add(-35*time.Minute), "$seen", "$flagged"), Fresh: true},
		{ID: "e3", Summary: sum("e3", "t3", "Eve Security", "Quarterly audit report attached", now.Add(-3*time.Hour), "$seen", "$answered")},
		{ID: "e2", Summary: sum("e2", "t1", "Bob Thread", "Re: planning sync", now.Add(-26*time.Hour), "$seen")},
		{ID: "e1", Summary: sum("e1", "t1", "Alice Root", "planning sync", now.Add(-27*time.Hour))},
	}
	// thread t1 is expanded: e2 is the header (newest member), e1 the member
	rows[2].ThreadHeader = true
	rows[3].ThreadMember = true
	// Member counts (FR-D1): t4 has replies beneath it (▸), t3 is a
	// single message, t1 is the open two-member thread.
	rows[0].ThreadSize = 3
	rows[1].ThreadSize = 1
	rows[2].ThreadSize = 2
	rows[3].ThreadSize = 2

	snap := sync.Snapshot{
		Version: 7,
		// Pre-order walk, as the sync engine emits it: parent before
		// children (FR-C6 folding reads depth runs).
		Mailboxes: []sync.MailboxNode{
			{Mailbox: mail.Mailbox{ID: "mb-inbox", Name: "Inbox", Role: mail.RoleInbox, TotalEmails: 14, UnreadEmails: 3}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-sent", Name: "Sent Items", Role: mail.RoleSent}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-archive", Name: "Archive", Role: mail.RoleArchive}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive"}, Depth: 1},
		},
		Rows:          rows,
		Cursor:        1,
		Total:         1432,
		Start:         0,
		ActiveMailbox: "mb-inbox",
		LoadForward:   true,
		Fresh:         []mail.ID{"e4"},
		Status: sync.Status{
			Mode:     sync.ModePush,
			LastSync: now,
		},
		Body: &sync.BodyView{
			ID:   "e3",
			Text: "Hello,\n\nFind the quarterly audit report attached.\n\nRegards,\nEve",
			Attachments: []mail.Attachment{
				{BlobID: "b1", Name: "audit-q3.pdf", Type: "application/pdf", Size: 248320},
			},
		},
	}
	return snap
}

type frame struct {
	name string
	w, h int
	st   func() State
}

// sidebarRowsFor builds one account's sidebar rows for a fixture: the
// account header (FR-C5) followed by the snapshot's mailbox tree, with
// the fold flags the app derives from the pre-order walk (FR-C6).
func sidebarRowsFor(acctID, name string, snap sync.Snapshot, tint int, active bool) []SidebarRow {
	rows := []SidebarRow{{
		Kind: SidebarAccount, Key: acctID, AccountID: acctID,
		Name: name, Tint: tint, Active: active,
		HasChildren: len(snap.Mailboxes) > 0,
	}}
	for i, n := range snap.Mailboxes {
		rows = append(rows, SidebarRow{
			Kind: SidebarMailbox, Key: acctID + "\x00" + string(n.Mailbox.ID),
			AccountID: acctID, MailboxID: n.Mailbox.ID,
			Name: n.Mailbox.Name, Depth: n.Depth, Unread: n.Mailbox.UnreadEmails,
			Active: active && n.Mailbox.ID == snap.ActiveMailbox,
			HasChildren: i+1 < len(snap.Mailboxes) &&
				snap.Mailboxes[i+1].Depth > n.Depth,
		})
	}
	return rows
}

// fixtureSidebarRows is the single-account column: Work's tree, active.
func fixtureSidebarRows() []SidebarRow {
	return sidebarRowsFor("work", "Work", fixtureSnapshot(), 0, true)
}

// fixtureMultiSidebarRows is the M8 column (FR-C5): every account in
// display order — Work's tree, Personal's, and a still-connecting account
// that has a header but no tree yet.
func fixtureMultiSidebarRows() []SidebarRow {
	rows := sidebarRowsFor("work", "Work", fixtureSnapshot(), 0, true)
	personal := sync.Snapshot{
		Mailboxes: []sync.MailboxNode{
			{Mailbox: mail.Mailbox{ID: "mb-p-inbox", Name: "Inbox", Role: mail.RoleInbox, UnreadEmails: 12}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-p-arch", Name: "Archive", Role: mail.RoleArchive}, Depth: 0},
		},
		ActiveMailbox: "mb-p-inbox",
	}
	rows = append(rows, sidebarRowsFor("personal", "Personal", personal, 1, false)...)
	rows = append(rows, SidebarRow{
		Kind: SidebarAccount, Key: "laptop", AccountID: "laptop",
		Name: "Old laptop", Tint: 2,
	})
	return rows
}

// fixtureFoldedSidebarRows is the fold view (FR-C6): Work's Archive
// subtree folded shut (▸, agent-test hidden) and Personal's whole account
// folded under its header — the two fold shapes in one column.
func fixtureFoldedSidebarRows() []SidebarRow {
	out := make([]SidebarRow, 0, 8)
	for _, r := range sidebarRowsFor("work", "Work", fixtureSnapshot(), 0, true) {
		if r.MailboxID == "mb-agent" {
			continue // hidden under folded Archive
		}
		if r.MailboxID == "mb-archive" {
			r.Collapsed = true
		}
		out = append(out, r)
	}
	out = append(out, SidebarRow{
		Kind: SidebarAccount, Key: "personal", AccountID: "personal",
		Name: "Personal", Tint: 1, HasChildren: true, Collapsed: true,
	})
	return out
}

func goldenFrames() []frame {
	mk := func(dark bool) func() State {
		pal := DarkTheme()
		if !dark {
			pal = LightTheme()
		}
		th := NewTheme(pal)
		return func() State {
			return State{
				Theme:          th,
				Snap:           fixtureSnapshot(),
				Focus:          uiFocus,
				SidebarVisible: sidebarOn,
				Account:        "Work",
				SidebarRows:    fixtureSidebarRows(),
				SidebarSel:     1, // the open Inbox, under Work's header (FR-C5)
				Now:            time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
				ShowSize:       showSize,
				VpView:         vpBody,
			}
		}
	}
	return []frame{
		{name: "wide-three-pane", w: 120, h: 40, st: mk(true)},
		{name: "wide-three-pane-light", w: 120, h: 40, st: mk(false)},
		{name: "medium-two-pane", w: 99, h: 35, st: mk(true)},
		{name: "medium-two-pane-light", w: 99, h: 35, st: mk(false)},
		{name: "medium-two-pane-preview", w: 99, h: 35, st: func() State {
			uiFocus = uiFocusPreview
			defer func() { uiFocus = PaneList }()
			return mk(true)()
		}},
		{name: "compact-single", w: 59, h: 25, st: mk(true)},
		{name: "compact-single-light", w: 59, h: 25, st: mk(false)},
		{name: "no-sidebar", w: 120, h: 40, st: func() State {
			sidebarOn = false
			defer func() { sidebarOn = true }()
			return mk(true)()
		}},
		{name: "loading", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Snap = sync.Snapshot{Total: -1}
			// No tree yet — the account header still holds its place.
			st.SidebarRows = sidebarRowsFor("work", "Work", sync.Snapshot{}, 0, true)
			return st
		}},
		{name: "status-polling", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Snap.Status = sync.Status{Mode: sync.ModePoll, LastSync: fixtureTime}
			return st
		}},
		{name: "status-error", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Snap.Status = sync.Status{
				Mode:      sync.ModePoll,
				LastSync:  fixtureTime,
				LastError: "push stream lost, reconnecting",
				Attempts:  3,
			}
			return st
		}},
		{name: "new-above-hint", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Snap.NewAbove = true
			return st
		}},
		{name: "selection", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Selected = map[mail.ID]bool{"e3": true, "e4": true}
			return st
		}},
		{name: "toast", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Toast = "Marked 1 message read"
			st.ToastHint = "ctrl+z undo"
			return st
		}},
		{name: "picker", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Picker = &PickerView{
				Title: "Move to mailbox",
				Items: []PickerItem{
					{ID: "mb-inbox", Label: "Inbox"},
					{ID: "mb-sent", Label: "Sent Items"},
					{ID: "mb-agent", Label: "agent-test", Depth: 1},
					{ID: "mb-archive", Label: "Archive"},
				},
				Sel: 2,
			}
			return st
		}},
		{name: "save-attachments", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.FilePick = &FilePickView{
				Title: "Save attachments to…",
				Path:  "/home/tester/Downloads",
				Hint:  "j/k move · l open · h back · enter save · esc cancel",
				View:  "  drwxr-xr-x  agent-test\n  drwxr-xr-x  invoices\n",
			}
			return st
		}},
		{name: "help", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.HelpOpen = true
			st.HelpSec = testKeyMap.Help(PaneList)
			return st
		}},
		// M4 search (FR-F1..F3) and full-screen view (FR-E5).
		{name: "search-query", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Search = &SearchView{Query: "invoice", Scope: "Inbox"}
			return st
		}},
		{name: "search-query-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Search = &SearchView{Query: "invoice", Scope: "Inbox"}
			return st
		}},
		{name: "search-all-scope", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Search = &SearchView{
				Query:  "audit",
				Tokens: []string{"from:ceo@example.test", "attachments"},
				Scope:  "all mailboxes",
			}
			return st
		}},
		{name: "search-narrow", w: 59, h: 25, st: func() State {
			st := mk(true)()
			st.Search = &SearchView{
				Query: "quarterly audit report attached",
				Scope: "all mailboxes",
			}
			return st
		}},
		{name: "search-scanning", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Search = &SearchView{
				Query:     "synth",
				Scope:     "all mailboxes",
				Scanning:  true,
				Scanned:   4500,
				ScanTotal: 12000,
			}
			return st
		}},
		{name: "search-advanced", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.AdvSearch = &AdvSearchView{
				Title: "Advanced search",
				Fields: []AdvField{
					{Name: "text", View: "audit", Focused: true},
					{Name: "from", View: "ceo@example.test"},
					{Name: "to", View: ""},
					{Name: "subject", View: "quarterly"},
					{Name: "after", View: "2026-01-01"},
					{Name: "before", View: ""},
					{Name: "keyword", View: ""},
				},
				Attach: true,
			}
			return st
		}},
		{name: "search-advanced-error", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.AdvSearch = &AdvSearchView{
				Title: "Advanced search",
				Fields: []AdvField{
					{Name: "text", View: "audit", Focused: false},
					{Name: "from", View: ""},
					{Name: "to", View: ""},
					{Name: "subject", View: ""},
					{Name: "after", View: "31-12-2026"},
					{Name: "before", View: ""},
					{Name: "keyword", View: ""},
				},
				Err: "dates want 2006-01-02",
			}
			return st
		}},
		{name: "fullscreen", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Fullscreen = true
			st.Focus = PanePreview // the app forces preview focus (FR-E5)
			return st
		}},
		{name: "fullscreen-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Fullscreen = true
			st.Focus = PanePreview
			return st
		}},
		// M5 composer (FR-H1..H5).
		{name: "compose", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Compose = composeFixture(st.Theme)
			return st
		}},
		{name: "compose-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Compose = composeFixture(st.Theme)
			return st
		}},
		{name: "compose-compact", w: 59, h: 25, st: func() State {
			st := mk(true)()
			st.Compose = composeFixture(st.Theme)
			return st
		}},
		{name: "compose-uploading", w: 99, h: 35, st: func() State {
			st := mk(true)()
			c := composeFixture(st.Theme)
			c.Focus = ZoneAttach
			c.Attachments = []ComposeAttachment{
				{Label: "audit-q3.pdf (248.3KiB)", Progress: "uploading 62%"},
				{Label: "notes.txt (1.2KiB)", Failed: "permission denied"},
			}
			c.Status = "uploading audit-q3.pdf"
			st.Compose = c
			return st
		}},
		{name: "compose-discard", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Compose = composeFixture(st.Theme)
			st.Compose.Discard = &DiscardConfirm{
				Title: "Discard this draft?",
				Hint:  "y discard · n keep in Drafts · esc keep editing",
			}
			return st
		}},
		// M6 multi-account: switcher modal (FR-A4) and the unified inbox
		// with owner colour bars + per-account footer status (FR-A5, FR-I5).
		{name: "switcher", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountSwitch = &SwitchView{Accounts: fixtureAccounts(), Sel: 1}
			return st
		}},
		{name: "switcher-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountSwitch = &SwitchView{Accounts: fixtureAccounts(), Sel: 1}
			return st
		}},
		{name: "unified", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Snap = fixtureUnifiedSnapshot()
			st.Unified = true
			st.Account = "Work"
			st.Accounts = fixtureAccounts()
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal"}
			st.AccountIndex = map[string]int{"work": 0, "personal": 1}
			// The folder column lists every account in unified mode too
			// (FR-C5 — the merge changes the list, not the tree).
			st.SidebarRows = fixtureMultiSidebarRows()
			return st
		}},
		{name: "unified-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Snap = fixtureUnifiedSnapshot()
			st.Unified = true
			st.Account = "Work"
			st.Accounts = fixtureAccounts()
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal"}
			st.AccountIndex = map[string]int{"work": 0, "personal": 1}
			// The folder column lists every account in unified mode too
			// (FR-C5 — the merge changes the list, not the tree).
			st.SidebarRows = fixtureMultiSidebarRows()
			return st
		}},
		{name: "unified-compact", w: 59, h: 25, st: func() State {
			st := mk(true)()
			st.Snap = fixtureUnifiedSnapshot()
			st.Unified = true
			st.Account = "Work"
			st.Accounts = fixtureAccounts()
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal"}
			st.AccountIndex = map[string]int{"work": 0, "personal": 1}
			// The folder column lists every account in unified mode too
			// (FR-C5 — the merge changes the list, not the tree).
			st.SidebarRows = fixtureMultiSidebarRows()
			return st
		}},
		// M8 multi-account folder column (FR-C5): every account's tree
		// under its tinted header, cursor parked on the second account's
		// header to show the selection wash over chrome.
		{name: "multi-sidebar", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountIndex = map[string]int{"work": 0, "personal": 1, "laptop": 2}
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal", "laptop": "Old laptop"}
			st.SidebarRows = fixtureMultiSidebarRows()
			st.SidebarSel = 5 // Personal's header
			st.Focus = PaneSidebar
			return st
		}},
		{name: "multi-sidebar-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountIndex = map[string]int{"work": 0, "personal": 1, "laptop": 2}
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal", "laptop": "Old laptop"}
			st.SidebarRows = fixtureMultiSidebarRows()
			st.SidebarSel = 5
			st.Focus = PaneSidebar
			return st
		}},
		// FR-C6 folding: Archive's subtree folded (▸, child hidden) and
		// a whole account folded under its header — both fold shapes,
		// with the wash parked on the folded folder.
		{name: "folded-sidebar", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountIndex = map[string]int{"work": 0, "personal": 1, "laptop": 2}
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal", "laptop": "Old laptop"}
			st.SidebarRows = fixtureFoldedSidebarRows()
			st.SidebarSel = 3 // folded Archive
			st.Focus = PaneSidebar
			return st
		}},
		{name: "folded-sidebar-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Accounts = fixtureAccounts()
			st.Account = "Work"
			st.AccountIndex = map[string]int{"work": 0, "personal": 1, "laptop": 2}
			st.AccountNames = map[string]string{"work": "Work", "personal": "Personal", "laptop": "Old laptop"}
			st.SidebarRows = fixtureFoldedSidebarRows()
			st.SidebarSel = 3
			st.Focus = PaneSidebar
			return st
		}},
		// FR-I10 stacked layout: list above preview, sidebar unchanged.
		{name: "stacked", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Stacked = true
			return st
		}},
		{name: "stacked-light", w: 120, h: 40, st: func() State {
			st := mk(false)()
			st.Stacked = true
			return st
		}},
		{name: "stacked-medium", w: 99, h: 35, st: func() State {
			st := mk(true)()
			st.Stacked = true
			return st
		}},
		{name: "stacked-preview-focus", w: 120, h: 40, st: func() State {
			st := mk(true)()
			st.Stacked = true
			st.Focus = PanePreview
			return st
		}},
	}
}

// fixtureAccounts is the multi-account chrome: two healthy accounts (the
// active one first) plus one in connect-failure — the footer names every
// non-active error (FR-I5).
func fixtureAccounts() []AccountView {
	return []AccountView{
		{ID: "work", Name: "Work", Active: true, Mode: sync.ModePush, LastSync: fixtureTime, Unread: 3},
		{ID: "personal", Name: "Personal", Mode: sync.ModePush, LastSync: fixtureTime, Unread: 12},
		{ID: "laptop", Name: "Old laptop", Mode: sync.ModeConnecting, LastError: "connect: auth rejected"},
	}
}

// fixtureUnifiedSnapshot tags the fixture rows with owners: the thread
// block (e2 header + e1 member) belongs to one account — threads never
// span accounts — while e4/e3 alternate to exercise the tints.
func fixtureUnifiedSnapshot() sync.Snapshot {
	snap := fixtureSnapshot()
	snap.Rows[0].Account = "work"     // e4
	snap.Rows[1].Account = "personal" // e3
	snap.Rows[2].Account = "work"     // e2 (thread header)
	snap.Rows[3].Account = "work"     // e1 (thread member)
	snap.Total = 4
	snap.ActiveMailbox = "" // unified is not a mailbox (header says so)
	snap.ViewKey = "u"
	return snap
}

// composeFixture builds a representative composer: a reply with a quoted
// body, one attachment, and the status/hint lines the app renders.
func composeFixture(th Theme) *ComposeView {
	return &ComposeView{
		Title:   "reply",
		From:    "Tester <tester@example.test>",
		To:      "Eve Security <eve@example.test>",
		Cc:      "dana@example.test",
		Bcc:     "",
		Subject: "Re: Quarterly audit report attached",
		Body: "On Mon, 21 Sep 2026 07:00 +0000, Eve Security <eve@example.test> wrote:\n" +
			"> Find the quarterly audit report attached.\n" +
			">\n" +
			"> Let me know if anything looks off.\n\n" +
			"Reviewed — numbers tie out.\n",
		Focus:  ZoneBody,
		Status: "saved",
		Attachments: []ComposeAttachment{
			{Label: "audit-q3.pdf (248.3KiB)"},
		},
		Hint: th.Accent.Render("ctrl+s send") + th.Muted.Render(" · ") +
			th.Muted.Render("ctrl+a attach") + th.Muted.Render(" · ") +
			th.Muted.Render("tab next") + th.Muted.Render(" · ") +
			th.Muted.Render("esc close"),
	}
}

var (
	uiFocus        = PaneList
	uiFocusPreview = PanePreview
	sidebarOn      = true
	showSize       = false
	vpBody         = "Hello,\n\nFind the quarterly audit report attached.\n\nRegards,\nEve"
	fixtureTime    = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	testKeyMap, _  = NewKeyMap(nil)
)

func TestGoldenFrames(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "golden")
	for _, f := range goldenFrames() {
		name := f.name + ".golden"
		path := filepath.Join(dir, name)
		got := Render(f.w, f.h, f.st())
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read golden %s: %v (run with -update to generate)", path, err)
			continue
		}
		if got != strings.TrimRight(string(want), "\n") {
			t.Errorf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
		}
	}
}

func TestRenderIsStableAcrossRuns(t *testing.T) {
	f := goldenFrames()[0]
	a := Render(f.w, f.h, f.st())
	b := Render(f.w, f.h, f.st())
	if a != b {
		t.Fatal("render is not deterministic")
	}
	if lipgloss.Width(strings.Split(a, "\n")[0]) > f.w {
		t.Fatal("header overflows width")
	}
}

// M7 wizard (FR-I8): the setup flow renders through its own entry point,
// so it gets its own frame set — form (dark/light), connection test,
// mailbox pick (both densities), the keyring-failure screen, and the
// saved summary.
func wizardFrames() []struct {
	name string
	w, h int
	dark bool
	v    WizardView
} {
	form := WizardView{
		Title: "Add an account",
		Step:  "1 of 4 · details",
		Fields: []WizardField{
			{Label: "Server URL", Value: "https://mail.example.com"},
			{Label: "Username", Value: "me@example.com"},
			{Label: "Password", Value: strings.Repeat("•", 8) + "▌", Focused: true},
			{Label: "Account name", Placeholder: "defaults to the account id"},
		},
		Hint: "tab next · enter continue · esc quit",
	}
	conn := form
	conn.Step = "2 of 4 · connection"
	conn.Fields = []WizardField{
		{Label: "Server URL", Value: "https://mail.example.com"},
		{Label: "Username", Value: "me@example.com"},
		{Label: "Password", Value: strings.Repeat("•", 8)},
		{Label: "Account name", Placeholder: "defaults to the account id"},
	}
	conn.Hint = "esc cancel"
	conn.Err = "connect to https://mail.example.com: 401 unauthorized — check the app password or the server URL"
	testing := conn
	testing.Err = ""
	testing.Status = "⣽ testing connection…"
	mailboxes := WizardView{
		Title: "Add an account",
		Step:  "3 of 4 · opening mailbox",
		Items: []PickerItem{
			{ID: "mb-inbox", Label: "Inbox"},
			{ID: "mb-sent", Label: "Sent Items"},
			{ID: "mb-archive", Label: "Archive"},
			{ID: "mb-agent", Label: "agent-test", Depth: 1},
			{ID: "mb-2026", Label: "2026", Depth: 1},
		},
		Sel:    2,
		Status: "connected — 5 mailboxes",
		Hint:   "j/k choose · enter continue · esc back",
	}
	save := WizardView{
		Title: "Add an account",
		Step:  "4 of 4 · save",
		Fields: []WizardField{
			{Label: "Server URL", Value: "https://mail.example.com"},
			{Label: "Username", Value: "me@example.com"},
			{Label: "Password", Value: strings.Repeat("•", 8)},
			{Label: "Account name", Value: "mail-example-com"},
		},
		Err:  "store secret: no secret service",
		Hint: "r retry keyring · f use password file · esc quit",
	}
	accounts := WizardView{
		Title: "Accounts",
		Step:  "1 of 5 · account",
		Items: []PickerItem{
			{Label: "+ add a new account"},
			{Label: "Work — me@work.example.com"},
			{Label: "Personal — me@example.com"},
		},
		Sel:  1,
		Hint: "j/k choose · enter select · esc quit",
	}
	edit := WizardView{
		Title: "Edit account · work",
		Step:  "2 of 5 · details",
		Fields: []WizardField{
			{Label: "Server URL", Value: "https://mail.example.com"},
			{Label: "Username", Value: "me@work.example.com"},
			{Label: "Password", Placeholder: "leave empty to keep the current password", Focused: true},
			{Label: "Account name", Value: "Work"},
		},
		Hint: "tab next · enter continue · esc back",
	}
	done := WizardView{
		Title: "Add an account",
		Step:  "",
		Lines: []string{
			"account\tmail-example-com",
			"name\tmail-example-com",
			"config\t/home/tester/.config/jmap-tui/config.toml",
			"mailbox\tInbox",
			"secret\tOS keyring",
		},
		Hint: "enter continue — jmap-tui starts next",
	}
	return []struct {
		name string
		w, h int
		dark bool
		v    WizardView
	}{
		{name: "wizard-form", w: 120, h: 40, dark: true, v: form},
		{name: "wizard-accounts", w: 120, h: 40, dark: true, v: accounts},
		{name: "wizard-accounts-light", w: 120, h: 40, dark: false, v: accounts},
		{name: "wizard-edit", w: 120, h: 40, dark: true, v: edit},
		{name: "wizard-form-light", w: 120, h: 40, dark: false, v: form},
		{name: "wizard-connection-error", w: 120, h: 40, dark: true, v: conn},
		{name: "wizard-testing", w: 120, h: 40, dark: true, v: testing},
		{name: "wizard-mailboxes", w: 120, h: 40, dark: true, v: mailboxes},
		{name: "wizard-mailboxes-compact", w: 59, h: 25, dark: true, v: mailboxes},
		{name: "wizard-save-keyring-failed", w: 120, h: 40, dark: true, v: save},
		{name: "wizard-done", w: 120, h: 40, dark: true, v: done},
		{name: "wizard-done-light", w: 120, h: 40, dark: false, v: done},
	}
}

func TestGoldenWizardFrames(t *testing.T) {
	dir := filepath.Join("..", "..", "test", "golden")
	for _, f := range wizardFrames() {
		pal := DarkTheme()
		if !f.dark {
			pal = LightTheme()
		}
		got := RenderWizard(f.w, f.h, NewTheme(pal), f.v)
		path := filepath.Join(dir, f.name+".golden")
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read golden %s: %v (run with -update to generate)", path, err)
			continue
		}
		if got != strings.TrimRight(string(want), "\n") {
			t.Errorf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", f.name, want, got)
		}
	}
}
