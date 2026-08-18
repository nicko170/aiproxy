package tui

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

// colorMode is how much colour the terminal gets. The theme degrades
// explicitly — truecolor → 256 → 16 → none — rather than trusting a library
// to guess, because the gauge's plain-mode rendering is structural (shading
// characters), not just "the same thing minus colour", and that switch has
// to be this package's own decision.
type colorMode int

const (
	modeNone colorMode = iota
	mode16
	mode256
	modeTrue
)

// detectColorMode reads the conventional environment signals. NO_COLOR set
// and non-empty always wins (https://no-color.org); TERM=dumb is a terminal
// that wants no sequences at all; COLORTERM names truecolor support; a
// -256color TERM names 256; anything else gets the 16 ANSI colours every
// terminal since the VT510 understands.
func detectColorMode() colorMode {
	if os.Getenv("NO_COLOR") != "" {
		return modeNone
	}
	term := os.Getenv("TERM")
	if term == "dumb" {
		return modeNone
	}
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return modeTrue
	}
	if strings.Contains(term, "256color") {
		return mode256
	}
	return mode16
}

// isTerminal reports whether stdout is a real TTY. It is a var, not a
// direct call, so tests can force both branches without a real terminal
// attached to the test binary (which under `go test` there never is).
var isTerminal = func() bool { return term.IsTerminal(os.Stdout.Fd()) }

// detectHyperlinkSupport reports whether stdout can be trusted with an OSC 8
// hyperlink escape. It reuses detectColorMode's two environment checks
// (NO_COLOR, TERM=dumb) — the same signals mean the same thing here: the
// user or the environment has asked for no escape sequences — and adds the
// one thing colour degradation does not need: a real TTY, since OSC 8
// written into a pipe, a log file, or a redirected file is either inert or
// actively wrong.
func detectHyperlinkSupport() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal()
}

// color is one palette entry with an explicit value per colour mode. c16 is
// the ANSI index 0–15.
type color struct {
	r, g, b uint8
	c256    uint8
	c16     uint8
}

// The palette. Five names, each meaning one thing:
//
//	ok      healthy, under the redline
//	warn    approaching the redline; saved-but-needs-restart; drops observed
//	bad     errored, exhausted, past the redline, disabled by failure
//	accent  focus, selection, the editable thing, key hints' verbs
//	dim     chrome: labels, rules, units, timestamps
//
// Body text is the terminal's default colour on purpose — colour encodes
// state, identity, and focus, never decoration.
var (
	colOK     = color{0x58, 0xA6, 0x6F, 71, 2}
	colWarn   = color{0xD9, 0xA3, 0x4A, 179, 3}
	colBad    = color{0xC7, 0x54, 0x50, 167, 1}
	colAccent = color{0x6D, 0x9B, 0xC3, 74, 6}
	colDim    = color{0x7A, 0x7A, 0x7A, 245, 8}
)

// identityCycle colours accounts and series. Muted on purpose: identity has
// to be tellable-apart, not loud. The 16-colour fallbacks avoid red and
// plain yellow, which the severity colours own.
var identityCycle = []color{
	{0x7E, 0x9C, 0xD8, 110, 4},  // blue
	{0xB4, 0x8E, 0xAD, 139, 5},  // mauve
	{0x5F, 0xAF, 0xAF, 73, 6},   // teal
	{0x87, 0xB3, 0x79, 108, 2},  // moss
	{0xD3, 0xB6, 0x73, 180, 11}, // sand (bright yellow in 16-mode, distinct from warn's plain yellow)
	{0xA6, 0xA6, 0xC8, 146, 7},  // slate
}

// severity is the three-step health scale every coloured state maps onto.
type severity int

const (
	sevOK severity = iota
	sevWarn
	sevBad
)

// theme renders strings in the terminal's colour mode. It is a value, not a
// singleton, so tests construct one per mode and golden frames are
// deterministic.
type theme struct {
	mode       colorMode
	hyperlinks bool
}

func newTheme() theme { return theme{mode: detectColorMode(), hyperlinks: detectHyperlinkSupport()} }

// sgr wraps s in one SGR sequence. modeNone returns s untouched — under
// NO_COLOR the output carries no escape at all, not an empty one.
func (t theme) sgr(s string, codes ...string) string {
	if t.mode == modeNone || len(codes) == 0 || s == "" {
		return s
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + s + "\x1b[0m"
}

// fgCode is the foreground code for c in the current mode.
func (t theme) fgCode(c color) string {
	switch t.mode {
	case modeTrue:
		return fmt.Sprintf("38;2;%d;%d;%d", c.r, c.g, c.b)
	case mode256:
		return fmt.Sprintf("38;5;%d", c.c256)
	case mode16:
		if c.c16 < 8 {
			return fmt.Sprintf("%d", 30+c.c16)
		}
		return fmt.Sprintf("%d", 90+c.c16-8)
	}
	return ""
}

func (t theme) fg(c color, s string) string { return t.sgr(s, t.fgCode(c)) }

// hyperlink wraps text in an OSC 8 sequence pointing at url, for terminals
// that turn it into a genuinely clickable link (iTerm2, WezTerm, Kitty,
// Ghostty…), when t.hyperlinks says the terminal can be trusted with one;
// otherwise text passes through completely unchanged. The visible text this
// produces is identical either way — the escape only adds clickability, it
// never changes what is rendered — so a caller may wrap the result in sgr
// styling exactly as it would the plain text.
func (t theme) hyperlink(url, text string) string {
	if !t.hyperlinks || url == "" || text == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func (t theme) ok(s string) string     { return t.fg(colOK, s) }
func (t theme) warn(s string) string   { return t.fg(colWarn, s) }
func (t theme) bad(s string) string    { return t.fg(colBad, s) }
func (t theme) accent(s string) string { return t.fg(colAccent, s) }
func (t theme) dim(s string) string    { return t.fg(colDim, s) }
func (t theme) bold(s string) string   { return t.sgr(s, "1") }

func sevColor(sev severity) color {
	switch sev {
	case sevBad:
		return colBad
	case sevWarn:
		return colWarn
	default:
		return colOK
	}
}

// sev colours s by severity.
func (t theme) sev(sev severity, s string) string { return t.fg(sevColor(sev), s) }

// fill paints s as gauge fill: reverse video in the severity's colour, so
// the fill is a solid block of meaning with the channel's text still legible
// inside it.
func (t theme) fill(sev severity, s string) string {
	return t.sgr(s, "7", t.fgCode(sevColor(sev)))
}

// identity colours s by a stable hash of key, so an account keeps its colour
// across frames, restarts, and screens.
func (t theme) identity(key, s string) string {
	h := fnv.New32a()
	h.Write([]byte(key))
	return t.fg(identityCycle[h.Sum32()%uint32(len(identityCycle))], s)
}
