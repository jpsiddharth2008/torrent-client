package main

import (
	"strings"
	"testing"
)

func TestDisabledPaletteIsAPlainNoOp(t *testing.T) {
	p := new_palette(false)

	if got := p.paint(p.accent, "hello"); got != "hello" {
		t.Errorf("paint on disabled palette = %q, want %q", got, "hello")
	}
	if got := p.colour_bar("[██░░]", p.success); got != "[██░░]" {
		t.Errorf("colour_bar on disabled palette = %q", got)
	}
	if got := p.colour_piece_map("█▒░"); got != "█▒░" {
		t.Errorf("colour_piece_map on disabled palette = %q", got)
	}
}

func TestEnabledPaletteWrapsAndResets(t *testing.T) {
	p := new_palette(true)

	got := p.paint(p.accent, "hello")
	if !strings.Contains(got, "hello") {
		t.Errorf("paint dropped the text: %q", got)
	}
	if !strings.HasSuffix(got, p.reset) {
		t.Errorf("paint did not reset: %q", got)
	}
}

// Colour must not break the plain text: the dashboard tests and any log
// scraping rely on figures staying contiguous inside the escape codes.
func TestColouredMapKeepsEveryCell(t *testing.T) {
	p := new_palette(true)
	const plain = "█▒░█▒░"

	coloured := p.colour_piece_map(plain)
	stripped := strings.NewReplacer(
		p.success, "", p.warning, "", p.muted, "", p.reset, "",
	).Replace(coloured)

	if stripped != plain {
		t.Errorf("stripping colour gave %q, want %q", stripped, plain)
	}
}

func TestColourDisabledWhenNotATerminal(t *testing.T) {
	if colour_enabled(false) {
		t.Error("colour_enabled(false) = true, want false for non-terminal output")
	}
}

func TestNoColorEnvDisablesColour(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colour_enabled(true) {
		t.Error("NO_COLOR is set but colour_enabled returned true")
	}
}

func TestDumbTerminalDisablesColour(t *testing.T) {
	t.Setenv("TERM", "dumb")
	if colour_enabled(true) {
		t.Error("TERM=dumb but colour_enabled returned true")
	}
}

func TestStatusTokenMatchesMeaning(t *testing.T) {
	p := new_palette(true)

	cases := map[string]string{
		"Complete":                    p.success,
		"Seeding (download complete)": p.accent,
		"Waiting for peers":           p.warning,
		"Downloading":                 p.heading,
	}
	for status, want := range cases {
		if got := p.status_token(status); got != want {
			t.Errorf("status_token(%q) = %q, want %q", status, got, want)
		}
	}
}
