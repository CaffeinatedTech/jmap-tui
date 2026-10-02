package ui

import (
	"fmt"
	"strings"
)

// Action is a stable identifier for a user command, used for key remapping
// in config ([keys] tables) and for help generation (FR-I3, FR-I4).
type Action string

// Actions, in milestone order. Triage actions (M3) follow the reader set.
const (
	ActListDown        Action = "list.down"
	ActListUp          Action = "list.up"
	ActListTop         Action = "list.top"
	ActListBottom      Action = "list.bottom"
	ActListPageDown    Action = "list.page_down"
	ActListPageUp      Action = "list.page_up"
	ActListHalfDown    Action = "list.half_down"
	ActListHalfUp      Action = "list.half_up"
	ActListNextUnread  Action = "list.next_unread"
	ActListPrevUnread  Action = "list.prev_unread"
	ActSort            Action = "list.sort"
	ActToggleThread    Action = "list.toggle_thread"
	ActToggleSize      Action = "list.toggle_size"
	ActToggleRead      Action = "list.toggle_read"
	ActToggleStar      Action = "list.toggle_star"
	ActToggleSelect    Action = "list.toggle_select"
	ActMove            Action = "list.move"
	ActCopy            Action = "list.copy"
	ActDelete          Action = "list.delete"
	ActArchive         Action = "list.archive"
	ActSidebarDown     Action = "sidebar.down"
	ActSidebarUp       Action = "sidebar.up"
	ActSidebarTop      Action = "sidebar.top"
	ActSidebarBottom   Action = "sidebar.bottom"
	ActOpenMailbox     Action = "sidebar.open"
	ActSidebarClose    Action = "sidebar.close"
	ActSidebarMoveUp   Action = "sidebar.move_account_up"
	ActSidebarMoveDown Action = "sidebar.move_account_down"
	ActSidebarCollapse Action = "sidebar.collapse"
	ActSidebarExpand   Action = "sidebar.expand"
	ActMarkFolderRead  Action = "sidebar.mark_read"
	ActPreviewDown     Action = "preview.down"
	ActPreviewUp       Action = "preview.up"
	ActPreviewHalf     Action = "preview.half_down"
	ActPreviewHalfUp   Action = "preview.half_up"
	ActPreviewTop      Action = "preview.top"
	ActPreviewBottom   Action = "preview.bottom"
	ActSaveAttach      Action = "preview.save_attachment"
	ActCompose         Action = "ui.compose"
	ActReply           Action = "list.reply"
	ActReplyAll        Action = "list.reply_all"
	ActForward         Action = "list.forward"
	ActSearch          Action = "ui.search"
	ActSearchAdv       Action = "ui.search_advanced"
	ActSearchClear     Action = "ui.search_clear"
	ActContacts        Action = "ui.contacts"
	ActContactNew      Action = "contacts.new"
	ActAccountSwitch   Action = "account.switch"
	ActAccountManage   Action = "account.manage"
	ActUnified         Action = "unified.toggle"
	ActFullscreen      Action = "preview.fullscreen"
	ActPreviewPageDown Action = "preview.page_down"
	ActPreviewPageUp   Action = "preview.page_up"
	ActCyclePane       Action = "pane.cycle"
	ActCyclePaneRev    Action = "pane.cycle_reverse"
	ActToggleSidebar   Action = "pane.toggle_sidebar"
	ActToggleLayout    Action = "pane.toggle_layout"
	ActUndo            Action = "ui.undo"
	ActHelp            Action = "ui.help"
	ActQuit            Action = "ui.quit"
)

// Binding is one key → action mapping. Help carries the human description
// shown by the overlay (FR-I4).
type Binding struct {
	Key  string // keystroke notation: "j", "ctrl+c", "shift+tab"
	Act  Action
	Help string
	Pane Pane // which pane the action belongs to (for context-sensitive help)
}

// defaultBindings is the canonical keymap (README). Keys are single
// strings; remaps replace by action.
//
// Note on case: paneKey lowercases both the binding and the keystroke, so
// "c" and "C" can never be distinct bindings in one pane — and a terminal
// delivers a capital letter as the keystroke "shift+c", not "C" (M5 audit:
// the old `{Key: "C"}` copy binding never matched in a real terminal).
// Distinct bindings therefore use distinct letters, and any capital is
// written as "shift+<letter>" the way "shift+g" already is.
func defaultBindings() []Binding {
	return []Binding{
		{Key: "j", Act: ActListDown, Help: "next message", Pane: PaneList},
		{Key: "down", Act: ActListDown, Help: "next message", Pane: PaneList},
		{Key: "k", Act: ActListUp, Help: "previous message", Pane: PaneList},
		{Key: "up", Act: ActListUp, Help: "previous message", Pane: PaneList},
		{Key: "g", Act: ActListTop, Help: "first message", Pane: PaneList},
		{Key: "shift+g", Act: ActListBottom, Help: "last message", Pane: PaneList},
		{Key: "ctrl+f", Act: ActListPageDown, Help: "page down", Pane: PaneList},
		{Key: "ctrl+b", Act: ActListPageUp, Help: "page up", Pane: PaneList},
		{Key: "ctrl+d", Act: ActListHalfDown, Help: "half page down", Pane: PaneList},
		{Key: "ctrl+u", Act: ActListHalfUp, Help: "half page up", Pane: PaneList},
		{Key: "space", Act: ActListPageDown, Help: "page down", Pane: PaneList},
		{Key: "shift+j", Act: ActListNextUnread, Help: "next unread", Pane: PaneList},
		{Key: "shift+k", Act: ActListPrevUnread, Help: "previous unread", Pane: PaneList},
		{Key: "enter", Act: ActToggleThread, Help: "expand/collapse thread", Pane: PaneList},
		{Key: "shift+s", Act: ActToggleSize, Help: "show/hide sizes", Pane: PaneList},
		{Key: "u", Act: ActToggleRead, Help: "toggle read/unread", Pane: PaneList},
		{Key: "*", Act: ActToggleStar, Help: "toggle star", Pane: PaneList},
		{Key: "x", Act: ActToggleSelect, Help: "select for batch action", Pane: PaneList},
		{Key: "m", Act: ActMove, Help: "move to mailbox…", Pane: PaneList},
		{Key: "y", Act: ActCopy, Help: "copy to mailbox…", Pane: PaneList},
		{Key: "e", Act: ActArchive, Help: "archive", Pane: PaneList},
		{Key: "d", Act: ActDelete, Help: "delete (to trash)", Pane: PaneList},
		{Key: "#", Act: ActDelete, Help: "delete (to trash)", Pane: PaneList},
		{Key: "s", Act: ActSort, Help: "sort by…", Pane: PaneList},
		{Key: "o", Act: ActSort, Help: "sort by…", Pane: PaneList},
		{Key: "j", Act: ActSidebarDown, Help: "next mailbox", Pane: PaneSidebar},
		{Key: "down", Act: ActSidebarDown, Help: "next mailbox", Pane: PaneSidebar},
		{Key: "k", Act: ActSidebarUp, Help: "previous mailbox", Pane: PaneSidebar},
		{Key: "up", Act: ActSidebarUp, Help: "previous mailbox", Pane: PaneSidebar},
		{Key: "g", Act: ActSidebarTop, Help: "first mailbox", Pane: PaneSidebar},
		{Key: "shift+g", Act: ActSidebarBottom, Help: "last mailbox", Pane: PaneSidebar},
		{Key: "enter", Act: ActOpenMailbox, Help: "open mailbox", Pane: PaneSidebar},
		// Folding (FR-C6): h/l are the tree's left/right (ranger/lf),
		// arrow keys alias them. l only ever expands — Enter is the
		// sole key that opens a mailbox or activates an account
		// (FR-C2, FR-C5 amended).
		{Key: "h", Act: ActSidebarCollapse, Help: "collapse tree", Pane: PaneSidebar},
		{Key: "left", Act: ActSidebarCollapse, Help: "collapse tree", Pane: PaneSidebar},
		{Key: "l", Act: ActSidebarExpand, Help: "expand tree", Pane: PaneSidebar},
		{Key: "right", Act: ActSidebarExpand, Help: "expand tree", Pane: PaneSidebar},
		// Mark every unread message in the cursor's folder read (FR-C7).
		{Key: "ctrl+r", Act: ActMarkFolderRead, Help: "mark folder read", Pane: PaneSidebar},
		// Account block reorder (FR-C5): moves the whole block of the
		// account under the cursor; a no-op at either end.
		{Key: "ctrl+up", Act: ActSidebarMoveUp, Help: "move account up", Pane: PaneSidebar},
		{Key: "ctrl+down", Act: ActSidebarMoveDown, Help: "move account down", Pane: PaneSidebar},
		// sidebar.close keeps its action id for [keys] remap stability
		// but ships unbound: "[" (pane.toggle_sidebar)
		// is now the sole show/hide key, since h folds the tree (FR-C6).
		// An empty key matches no keystroke; Help skips it until a
		// remap gives it one.
		{Key: "", Act: ActSidebarClose, Help: "hide sidebar", Pane: PaneSidebar},
		{Key: "j", Act: ActPreviewDown, Help: "scroll down", Pane: PanePreview},
		{Key: "down", Act: ActPreviewDown, Help: "scroll down", Pane: PanePreview},
		{Key: "k", Act: ActPreviewUp, Help: "scroll up", Pane: PanePreview},
		{Key: "up", Act: ActPreviewUp, Help: "scroll up", Pane: PanePreview},
		{Key: "d", Act: ActPreviewHalf, Help: "half page down", Pane: PanePreview},
		{Key: "u", Act: ActPreviewHalfUp, Help: "half page up", Pane: PanePreview},
		{Key: "ctrl+f", Act: ActPreviewPageDown, Help: "page down", Pane: PanePreview},
		{Key: "ctrl+b", Act: ActPreviewPageUp, Help: "page up", Pane: PanePreview},
		{Key: "g", Act: ActPreviewTop, Help: "top of message", Pane: PanePreview},
		{Key: "shift+g", Act: ActPreviewBottom, Help: "bottom of message", Pane: PanePreview},
		{Key: "s", Act: ActSaveAttach, Help: "save attachments…", Pane: PanePreview},
		// Preview paging from any pane (FR-E3): the pane-scoped keys
		// above stay as they are while the preview is focused.
		{Key: "pgdown", Act: ActPreviewPageDown, Help: "preview page down", Pane: PaneAny},
		{Key: "pgup", Act: ActPreviewPageUp, Help: "preview page up", Pane: PaneAny},
		{Key: "v", Act: ActFullscreen, Help: "full-screen message", Pane: PaneAny},
		{Key: "n", Act: ActCompose, Help: "compose a message", Pane: PaneAny},
		{Key: "r", Act: ActReply, Help: "reply", Pane: PaneAny},
		{Key: "a", Act: ActReplyAll, Help: "reply to all", Pane: PaneAny},
		{Key: "f", Act: ActForward, Help: "forward", Pane: PaneAny},
		{Key: "/", Act: ActSearch, Help: "search", Pane: PaneAny},
		{Key: "ctrl+s", Act: ActSearchAdv, Help: "advanced search", Pane: PaneAny},
		{Key: "esc", Act: ActSearchClear, Help: "clear search", Pane: PaneAny},
		// Contacts (M9, FR-L): the screen plus the add-contact modal.
		// From a message, shift+n prefills from the sender; with no
		// message it opens blank.
		{Key: "c", Act: ActContacts, Help: "contacts", Pane: PaneAny},
		{Key: "shift+n", Act: ActContactNew, Help: "add contact", Pane: PaneAny},
		{Key: "shift+a", Act: ActAccountSwitch, Help: "switch account", Pane: PaneAny},
		{Key: "ctrl+a", Act: ActAccountManage, Help: "add or edit an account", Pane: PaneAny},
		{Key: "i", Act: ActUnified, Help: "unified inbox", Pane: PaneAny},
		{Key: "tab", Act: ActCyclePane, Help: "next pane", Pane: PaneAny},
		{Key: "shift+tab", Act: ActCyclePaneRev, Help: "previous pane", Pane: PaneAny},
		{Key: "[", Act: ActToggleSidebar, Help: "show/hide sidebar", Pane: PaneAny},
		{Key: "z", Act: ActToggleLayout, Help: "stack/side-by-side layout", Pane: PaneAny},
		{Key: "ctrl+z", Act: ActUndo, Help: "undo last action", Pane: PaneAny},
		{Key: "?", Act: ActHelp, Help: "help", Pane: PaneAny},
		{Key: "q", Act: ActQuit, Help: "quit", Pane: PaneAny},
	}
}

// KeyMap resolves keystrokes to actions with per-pane precedence: a binding
// registered to a specific pane shadows the same key bound to PaneAny, so
// "j" can mean next-message in the list and scroll-down in the preview
// without ambiguity (FR-I3: conflicts forbidden — same key+pane is a
// config error).
type KeyMap struct {
	byAction map[Action]string
	specific map[string]Action // pane-key → action (uniqueness enforced)
	keys     []Binding         // in definition order, for help
}

// Pane identifies a focusable region.
type Pane int

// Panes, in cycling order.
const (
	PaneSidebar Pane = iota
	PaneList
	PanePreview
	paneCount

	// PaneAny is a wildcard for global bindings.
	PaneAny Pane = -1
)

// NewKeyMap builds the keymap from defaults plus user remaps (config
// [keys].<action> = "key"). The first listed binding for an action is its
// primary key (shown in help); later bindings are aliases. A remap
// replacing a key already bound to a different action in the same pane is
// rejected as a conflict by Validate.
func NewKeyMap(remaps map[Action]string) (*KeyMap, error) {
	km := &KeyMap{
		byAction: map[Action]string{},
		specific: map[string]Action{},
		keys:     defaultBindings(),
	}
	for i := range km.keys {
		b := &km.keys[i]
		if _, exists := km.byAction[b.Act]; !exists {
			km.byAction[b.Act] = b.Key
		}
	}
	for act, key := range remaps {
		if _, ok := km.byAction[act]; !ok {
			return nil, fmt.Errorf("keys: unknown action %q", act)
		}
		if key == "" {
			return nil, fmt.Errorf("keys: empty key for action %q", act)
		}
		km.byAction[act] = key
	}
	// The primary binding takes the resolved key (possibly remapped);
	// aliases keep their own keys.
	primarySeen := map[Action]bool{}
	for i := range km.keys {
		b := &km.keys[i]
		if !primarySeen[b.Act] {
			b.Key = km.byAction[b.Act]
			primarySeen[b.Act] = true
		}
		km.specific[paneKey(b.Pane, b.Key)] = b.Act
	}
	return km, nil
}

func paneKey(p Pane, key string) string {
	return fmt.Sprintf("%d\x00%s", p, strings.ToLower(key))
}

// Match resolves a keystroke in the given pane: pane-specific bindings win
// over PaneAny bindings.
func (km *KeyMap) Match(pane Pane, keystroke string) (Action, bool) {
	if act, ok := km.specific[paneKey(pane, keystroke)]; ok {
		return act, true
	}
	if act, ok := km.specific[paneKey(PaneAny, keystroke)]; ok {
		return act, true
	}
	return "", false
}

// Key returns the key bound to an action.
func (km *KeyMap) Key(act Action) string { return km.byAction[act] }

// HelpSection is a pane's bindings for the overlay (FR-I4).
type HelpSection struct {
	Title    string
	Bindings []Binding
}

// Help generates the overlay content for a pane: the pane's own bindings
// plus the global ones, in definition order, with aliases grouped
// ("j/down").
func (km *KeyMap) Help(pane Pane) HelpSection {
	sec := HelpSection{}
	seen := map[Action]bool{}
	for _, b := range km.keys {
		if b.Pane != pane && b.Pane != PaneAny {
			continue
		}
		if b.Key == "" {
			// An unbound action (kept only for remap stability) has
			// nothing to show until the user remaps it.
			continue
		}
		if seen[b.Act] {
			continue
		}
		seen[b.Act] = true
		keys := []string{}
		for _, cand := range km.keys {
			if cand.Act == b.Act && cand.Pane == b.Pane {
				keys = append(keys, cand.Key)
			}
		}
		grouped := b
		grouped.Key = strings.Join(keys, "/")
		sec.Bindings = append(sec.Bindings, grouped)
	}
	return sec
}

// Validate reports duplicate key bindings within the same pane (FR-I3).
// It also rejects a global binding shadowed by a pane-specific one: the
// pane binding wins on every keystroke, which would leave the global
// action silently unreachable there — ambiguity by another name.
func (km *KeyMap) Validate() error {
	seen := map[string]Action{}
	for _, b := range km.keys {
		pk := paneKey(b.Pane, b.Key)
		if prev, dup := seen[pk]; dup {
			return fmt.Errorf("keys: %q bound to both %s and %s in the same context", b.Key, prev, b.Act)
		}
		seen[pk] = b.Act
	}
	// Same pane+key duplicates already returned above, so every entry
	// here is a distinct pane sharing one keystroke.
	byKey := map[string][]Binding{}
	for _, b := range km.keys {
		k := strings.ToLower(b.Key)
		byKey[k] = append(byKey[k], b)
	}
	for k, binds := range byKey {
		var global *Binding
		for i := range binds {
			if binds[i].Pane == PaneAny {
				global = &binds[i]
				break
			}
		}
		if global == nil {
			continue
		}
		for _, b := range binds {
			if b.Pane == PaneAny || b.Act == global.Act {
				continue
			}
			return fmt.Errorf("keys: %q is bound to %s everywhere and %s in the %s pane — the global binding would never fire there",
				k, global.Act, b.Act, paneTitle(b.Pane))
		}
	}
	return nil
}
