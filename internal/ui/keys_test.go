package ui

import "testing"

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
	// Unbound key misses.
	if _, ok := km.Match(PaneList, "z"); ok {
		t.Fatal("z should be unbound in the list")
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
