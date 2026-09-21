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
// row, expanded thread header + member, replied flag, attachment.
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

	snap := sync.Snapshot{
		Version: 7,
		Mailboxes: []sync.MailboxNode{
			{Mailbox: mail.Mailbox{ID: "mb-inbox", Name: "Inbox", Role: mail.RoleInbox, TotalEmails: 14, UnreadEmails: 3}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-sent", Name: "Sent Items", Role: mail.RoleSent}, Depth: 0},
			{Mailbox: mail.Mailbox{ID: "mb-agent", Name: "agent-test", ParentID: "mb-archive"}, Depth: 1},
			{Mailbox: mail.Mailbox{ID: "mb-archive", Name: "Archive", Role: mail.RoleArchive}, Depth: 0},
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
