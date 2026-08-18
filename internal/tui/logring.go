package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogLine is one rendered slog record.
type LogLine struct {
	At    int64 // unix ms
	Level string
	Text  string
}

// LogRing is a bounded ring of slog records the Activity screen renders
// (spec §8: under a TUI, slog feeds a ring buffer instead of stderr, which a
// full-screen program would only corrupt). Writes never block and never
// allocate beyond the ring; the TUI polls Snapshot on its own cadence.
type LogRing struct {
	mu    sync.Mutex
	lines []LogLine
	max   int
}

// NewLogRing builds a ring holding at most n lines.
func NewLogRing(n int) *LogRing {
	if n <= 0 {
		n = 200
	}
	return &LogRing{max: n}
}

// Handler returns a slog.Handler at the given level writing into the ring.
func (r *LogRing) Handler(level slog.Leveler) slog.Handler {
	return &ringHandler{ring: r, level: level}
}

// Snapshot copies the current lines, oldest first.
func (r *LogRing) Snapshot() []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogLine, len(r.lines))
	copy(out, r.lines)
	return out
}

func (r *LogRing) add(l LogLine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) >= r.max {
		copy(r.lines, r.lines[1:])
		r.lines[len(r.lines)-1] = l
		return
	}
	r.lines = append(r.lines, l)
}

type ringHandler struct {
	ring  *LogRing
	level slog.Leveler
	attrs []slog.Attr
	group string
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *ringHandler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Message)
	emit := func(a slog.Attr) {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fmt.Fprintf(&b, " %s=%v", key, a.Value)
	}
	for _, a := range h.attrs {
		emit(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		emit(a)
		return true
	})
	at := rec.Time
	if at.IsZero() {
		at = time.Now()
	}
	h.ring.add(LogLine{At: at.UnixMilli(), Level: rec.Level.String(), Text: b.String()})
	return nil
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &n
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	n := *h
	if n.group != "" {
		n.group += "." + name
	} else {
		n.group = name
	}
	return &n
}
