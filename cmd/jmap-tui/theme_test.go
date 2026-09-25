package main

import (
	"fmt"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/ui"
)

// M7 themes gate: the flag/config setting resolves to the right palette
// and "auto" falls back to the dark default (safe for SSH).
func TestResolvePalette(t *testing.T) {
	if got := resolvePalette("light"); !samePalette(got, ui.LightTheme()) {
		t.Error("light did not resolve to the light palette")
	}
	if got := resolvePalette("dark"); !samePalette(got, ui.DarkTheme()) {
		t.Error("dark did not resolve to the dark palette")
	}
	if got := resolvePalette(""); !samePalette(got, ui.DarkTheme()) {
		t.Error("auto default should be dark (no terminal hints)")
	}
	t.Setenv("COLORFGBG", "0;15")
	if got := resolvePalette("auto"); !samePalette(got, ui.LightTheme()) {
		t.Error("COLORFGBG light background not detected")
	}
	t.Setenv("COLORFGBG", "15;0")
	if got := resolvePalette("auto"); !samePalette(got, ui.DarkTheme()) {
		t.Error("COLORFGBG dark background not detected")
	}
}

func samePalette(a, b ui.Palette) bool {
	return fmt.Sprint(a.Accent) == fmt.Sprint(b.Accent) &&
		fmt.Sprint(a.BodyFg) == fmt.Sprint(b.BodyFg) &&
		fmt.Sprint(a.Muted) == fmt.Sprint(b.Muted)
}
