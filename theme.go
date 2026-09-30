package main

import (
	"os"
	"strings"
)

// palette holds the dashboard's colour tokens, named by the role they play
// rather than by the colour they happen to be. Keeping the mapping in one
// place means the theme can change without touching the render code.
//
// Every field is an empty string when colour is disabled, so call sites stay
// plain concatenation with no conditionals.
type palette struct {
	reset   string
	bold    string
	heading string // file name, section titles
	accent  string // primary figures the eye should land on first
	success string // completed work, download direction
	warning string // in-progress or attention states
	danger  string // failures
	muted   string // labels and chrome, deliberately recessive
}

func new_palette(enabled bool) palette {
	if !enabled {
		return palette{}
	}
	return palette{
		reset:   "\033[0m",
		bold:    "\033[1m",
		heading: "\033[1;97m", // bold bright white
		accent:  "\033[96m",   // bright cyan
		success: "\033[92m",   // bright green
		warning: "\033[93m",   // bright yellow
		danger:  "\033[91m",   // bright red
		muted:   "\033[90m",   // grey
	}
}

// colour_enabled honours the NO_COLOR convention and stays off when stdout is
// not a terminal, so redirected or piped output holds no escape codes.
func colour_enabled(is_terminal bool) bool {
	if !is_terminal {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	return os.Getenv("TERM") != "dumb"
}

// paint wraps s in a colour token. A blank token returns s untouched, which is
// what makes the disabled palette a no-op.
func (p palette) paint(token, s string) string {
	// an empty string needs no wrapper: without this an absent optional field
	// still emits a colour-then-reset pair into every frame
	if token == "" || s == "" {
		return s
	}
	return token + s + p.reset
}

// paint_runs writes s with each character coloured by token_for, emitting one
// escape sequence per run of same-coloured characters rather than per
// character. A 40-cell bar is usually two runs, so a frame carries a handful
// of escape codes instead of hundreds.
func (p palette) paint_runs(s string, token_for func(rune) string) string {
	if p.reset == "" {
		return s
	}

	var b strings.Builder
	current := ""
	for _, r := range s {
		token := token_for(r)
		if token != current {
			if current != "" {
				b.WriteString(p.reset)
			}
			b.WriteString(token)
			current = token
		}
		b.WriteRune(r)
	}
	if current != "" {
		b.WriteString(p.reset)
	}
	return b.String()
}

// colour_bar tints a progress bar built by make_progress_bar: filled cells in
// done, the remaining track recessive so the boundary reads at a glance.
//
// The bar is generated uncoloured and tinted here so make_progress_bar stays
// a pure string function that is cheap to test.
func (p palette) colour_bar(bar, done string) string {
	return p.paint_runs(bar, func(r rune) string {
		if r == '█' {
			return done
		}
		return p.muted // unfilled track and the enclosing brackets
	})
}

// colour_piece_map tints the piece map: verified pieces green, partly filled
// cells amber, missing ones recessive grey.
func (p palette) colour_piece_map(m string) string {
	return p.paint_runs(m, func(r rune) string {
		switch r {
		case '█':
			return p.success
		case '▒':
			return p.warning
		default:
			return p.muted
		}
	})
}

// status_token picks the colour that matches a status line's meaning, so the
// state is readable from the colour alone before the text is parsed.
func (p palette) status_token(status string) string {
	switch {
	case strings.HasPrefix(status, "Complete"):
		return p.success
	case strings.HasPrefix(status, "Seeding"):
		return p.accent
	case strings.HasPrefix(status, "Waiting"):
		return p.warning
	default:
		return p.heading
	}
}
