package ui

import (
	"strings"
	"testing"
)

func TestKeyMapDefaultsResolve(t *testing.T) {
	km, err := NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Pane-specific shadowing: "j" in the list is list.down, in the preview
	// it is preview.down (FR-I3).
	if act, ok := km.Match(PaneList, "j"); !ok || act != ActListDown {
		t.Fatalf("list j = %v %v", act, ok)
	}
	if act, ok := km.Match(PanePreview, "j"); !ok || act != ActPreviewDown {
		t.Fatalf("preview j = %v %v", act, ok)
	}
	// Global binding reachable from any pane.
	if act, ok := km.Match(PaneSidebar, "?"); !ok || act != ActHelp {
		t.Fatalf("sidebar ? = %v %v", act, ok)
	}
	// Account-block reorder lives on ctrl+arrows in the sidebar (FR-C5).
	if act, ok := km.Match(PaneSidebar, "ctrl+up"); !ok || act != ActSidebarMoveUp {
		t.Fatalf("sidebar ctrl+up = %v %v", act, ok)
	}
	if act, ok := km.Match(PaneSidebar, "ctrl+down"); !ok || act != ActSidebarMoveDown {
		t.Fatalf("sidebar ctrl+down = %v %v", act, ok)
	}
	// Folding (FR-C6): h/l are the tree's left/right, arrows alias them.
	if act, ok := km.Match(PaneSidebar, "h"); !ok || act != ActSidebarCollapse {
		t.Fatalf("sidebar h = %v %v, want collapse", act, ok)
	}
	if act, ok := km.Match(PaneSidebar, "left"); !ok || act != ActSidebarCollapse {
		t.Fatalf("sidebar left = %v %v, want collapse", act, ok)
	}
	if act, ok := km.Match(PaneSidebar, "l"); !ok || act != ActSidebarExpand {
		t.Fatalf("sidebar l = %v %v, want expand", act, ok)
	}
	if act, ok := km.Match(PaneSidebar, "right"); !ok || act != ActSidebarExpand {
		t.Fatalf("sidebar right = %v %v, want expand", act, ok)
	}
	// "[" is the only show/hide key (FR-C4, FR-C6).
	if act, ok := km.Match(PaneSidebar, "["); !ok || act != ActToggleSidebar {
		t.Fatalf("sidebar [ = %v %v, want toggle_sidebar", act, ok)
	}
	// Unbound key misses.
	if _, ok := km.Match(PaneList, "w"); ok {
		t.Fatal("w should be unbound in the list")
	}
}

// TestKeyMapUnboundActionRemappable: an action that ships without a
// default key (sidebar.close — "[" took over, FR-C6) keeps its id
// remappable (KEYMAP_PLAN §6), stays out of the overlay until remapped,
// and validates like any other binding.
func TestKeyMapUnboundActionRemappable(t *testing.T) {
	km, err := NewKeyMap(map[Action]string{ActSidebarClose: "x"})
	if err != nil {
		t.Fatalf("NewKeyMap remap of unbound action: %v", err)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if act, ok := km.Match(PaneSidebar, "x"); !ok || act != ActSidebarClose {
		t.Fatalf("remapped sidebar.close = %v %v", act, ok)
	}
	sec := km.Help(PaneSidebar)
	found := false
	for _, b := range sec.Bindings {
		if b.Key == "" {
			t.Fatal("help lists a binding with no key")
		}
		if b.Act == ActSidebarClose {
			found = true
		}
	}
	if !found {
		t.Fatal("remapped sidebar.close missing from help")
	}

	// Unremapped, it contributes nothing to the overlay.
	def, err := NewKeyMap(nil)
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	for _, b := range def.Help(PaneSidebar).Bindings {
		if b.Act == ActSidebarClose {
			t.Fatal("unbound sidebar.close appears in help")
		}
	}
}

func TestKeyMapRemap(t *testing.T) {
	km, err := NewKeyMap(map[Action]string{ActListDown: "n"})
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if act, ok := km.Match(PaneList, "n"); !ok || act != ActListDown {
		t.Fatalf("remapped key missing: %v %v", act, ok)
	}
	// The alias (arrow) keeps working too.
	if act, ok := km.Match(PaneList, "down"); !ok || act != ActListDown {
		t.Fatalf("alias dropped by remap: %v %v", act, ok)
	}
	if km.Key(ActListDown) != "n" {
		t.Fatalf("Key() = %q, want n", km.Key(ActListDown))
	}
}

func TestKeyMapUnknownActionRejected(t *testing.T) {
	if _, err := NewKeyMap(map[Action]string{"nope": "x"}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, err := NewKeyMap(map[Action]string{ActQuit: ""}); err == nil {
		t.Fatal("empty remap accepted")
	}
}

func TestKeyMapHelpGroupsAliases(t *testing.T) {
	km, _ := NewKeyMap(nil)
	sec := km.Help(PaneList)
	var found string
	for _, b := range sec.Bindings {
		if b.Act == ActListDown {
			found = b.Key
		}
	}
	if found != "j/down" {
		t.Fatalf("alias group = %q, want j/down", found)
	}
}

// TestKeyMapRejectsShadowedGlobals: a global binding that a pane-specific
// binding reuses would never fire in that pane — ambiguity FR-I3 forbids
// (it shows up when a user remaps an action onto an existing key).
func TestKeyMapRejectsShadowedGlobals(t *testing.T) {
	// Defaults must be clean, or every startup would fail.
	km, err := NewKeyMap(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := km.Validate(); err != nil {
		t.Fatalf("defaults violate Validate: %v", err)
	}

	// Remapping quit onto a list key shadows the global in the list pane.
	bad, err := NewKeyMap(map[Action]string{ActQuit: "j"})
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("shadowed global accepted; want an error")
	} else if !strings.Contains(err.Error(), "quit") {
		t.Errorf("error %q should name the shadowed action", err)
	}

	// Two actions on one key in one pane is still the original error.
	clash, err := NewKeyMap(map[Action]string{ActListDown: "x"})
	if err != nil {
		t.Fatalf("NewKeyMap: %v", err)
	}
	if err := clash.Validate(); err == nil || !strings.Contains(err.Error(), "select") {
		t.Fatalf("err = %v, want the list.down/list.toggle_select conflict", err)
	}
}
