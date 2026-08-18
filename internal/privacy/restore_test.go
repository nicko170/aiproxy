package privacy

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func ev(payload string) []byte {
	var typ struct{ Type string }
	json.Unmarshal([]byte(payload), &typ)
	return []byte("event: " + typ.Type + "\ndata: " + payload + "\n\n")
}

func textDelta(index int, text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return ev(string(b))
}

// emitted concatenates the restorer's output for a sequence of events.
func emitted(t *testing.T, r *Restorer, events ...[]byte) string {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		out, err := r.Event(e)
		if err != nil {
			t.Fatalf("Event: %v", err)
		}
		b.Write(out)
	}
	return b.String()
}

// textOf pulls every text_delta's text out of a stream, grouped by content
// block index and concatenated in index order — the string each block's
// consumer actually assembles.
//
// Grouping by index (rather than simply concatenating in arrival order) is
// deliberate: a correctly streaming restorer emits a block's already-safe
// prefix the instant it is known, without waiting for a placeholder split
// across a chunk boundary to resolve. When two blocks interleave, that means
// their deltas interleave on the wire too — a real client demultiplexes by
// index before reading the text, and this helper does the same so the test
// reflects what the client actually sees per block, not the raw byte order.
func textOf(t *testing.T, stream string) string {
	t.Helper()
	byIndex := map[int]*strings.Builder{}
	var order []int
	for _, chunk := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(chunk, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m struct {
				Type  string `json:"type"`
				Index int    `json:"index"`
				Delta struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("undecodable event %q: %v", line, err)
			}
			b, ok := byIndex[m.Index]
			if !ok {
				b = &strings.Builder{}
				byIndex[m.Index] = b
				order = append(order, m.Index)
			}
			b.WriteString(m.Delta.Text)
			b.WriteString(m.Delta.Thinking)
		}
	}
	sort.Ints(order)
	var out strings.Builder
	for _, idx := range order {
		out.WriteString(byIndex[idx].String())
	}
	return out.String()
}

func TestRestorerSubstitutesAcrossTwoEvents(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	r := NewRestorer(tab, Passthrough, nil)

	// The placeholder is split mid-sentinel, which is the ordinary case.
	cut := len(Sentinel) - 3
	stream := emitted(t, r,
		textDelta(0, "your key "+p[:cut]),
		textDelta(0, p[cut:]+" is stale"),
		ev(`{"type":"content_block_stop","index":0}`),
	)
	if got := textOf(t, stream); got != "your key AKIAIOSFODNN7EXAMPLE is stale" {
		t.Errorf("assembled text = %q", got)
	}
}

// Property 3 at the SSE level: an event needing no change must come back as the
// exact same bytes, not a re-encoded equivalent.
func TestRestorerReturnsUnchangedEventsByteForByte(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	for _, raw := range [][]byte{
		textDelta(0, "just ordinary prose"),
		ev(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12}}}`),
		ev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		ev(`{"type":"ping"}`),
	} {
		out, err := r.Event(raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(raw) {
			t.Errorf("event was rewritten when it needed no change:\n got %q\nwant %q", out, raw)
		}
	}
}

// Unknown fields must survive. A provider adding one must not have it dropped by
// a filter that only knows about the fields it cares about.
func TestRestorerPreservesUnknownFields(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "V")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta":     map[string]any{"type": "text_delta", "text": p},
		"brand_new": "keep me",
	})
	out, err := r.Event(ev(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "brand_new") || !strings.Contains(string(out), "keep me") {
		t.Errorf("unknown field was dropped: %s", out)
	}
}

// A tail withheld when the block ends is emitted as a synthetic delta before the
// stop event, so nothing is lost and the stop still arrives last.
func TestRestorerFlushesAPartialTailBeforeStop(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	stream := emitted(t, r,
		textDelta(0, "trailing bracket ["),
		ev(`{"type":"content_block_stop","index":0}`),
	)
	if got := textOf(t, stream); got != "trailing bracket [" {
		t.Errorf("assembled text = %q; the withheld byte must be flushed", got)
	}
	if !strings.Contains(stream, "content_block_stop") {
		t.Error("the stop event was lost")
	}
	if strings.Index(stream, "content_block_stop") < strings.LastIndex(stream, "text_delta") {
		t.Error("the synthetic flush must precede the stop event")
	}
}

// Two blocks interleave; each keeps its own pending buffer.
func TestRestorerKeepsBlocksIndependent(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "SEKRIT")
	r := NewRestorer(tab, Passthrough, nil)
	cut := len(Sentinel) - 2
	stream := emitted(t, r,
		textDelta(0, "a"+p[:cut]),
		textDelta(1, "b"+p[:cut]),
		textDelta(0, p[cut:]+"A"),
		textDelta(1, p[cut:]+"B"),
	)
	got := textOf(t, stream)
	if !strings.Contains(got, "aSEKRITA") || !strings.Contains(got, "bSEKRITB") {
		t.Errorf("blocks bled into each other: %q", got)
	}
}

func TestRestorerHandlesThinkingDeltas(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada Lovelace")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": "about " + p},
	})
	stream := emitted(t, r, ev(string(b)))
	if got := textOf(t, stream); got != "about Ada Lovelace" {
		t.Errorf("thinking text = %q", got)
	}
}

// A complete text on content_block_start is restored whole.
func TestRestorerHandlesContentBlockStart(t *testing.T) {
	tab, p := tableWith(t, LabelEmail, "ada@example.com")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": "mail " + p},
	})
	out, err := r.Event(ev(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ada@example.com") {
		t.Errorf("content_block_start was not restored: %s", out)
	}
}

func TestRestorerBodyHandlesNonStreamingResponses(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	r := NewRestorer(tab, Passthrough, nil)
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"key ` + p + ` here"}],"model":"claude-opus-5"}`)
	out, err := r.Body(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("body was not restored: %s", out)
	}
	if !strings.Contains(string(out), `"model":"claude-opus-5"`) {
		t.Errorf("body lost structure: %s", out)
	}
}

func TestRestorerBodyWithNothingToDoIsByteIdentical(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"nothing here"}]}`)
	out, err := r.Body(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed with nothing to restore:\n got %s\nwant %s", out, body)
	}
}

func TestRestorerReportsUnresolvedPlaceholders(t *testing.T) {
	var seen []string
	r := NewRestorer(NewTable(testKey), Passthrough, func(p string) { seen = append(seen, p) })
	stream := emitted(t, r, textDelta(0, "orphan [[AIPROXY_SECRET_deadbeef]] here"))
	if got := textOf(t, stream); !strings.Contains(got, "[[AIPROXY_SECRET_deadbeef]]") {
		t.Errorf("the placeholder must pass through verbatim, got %q", got)
	}
	if len(seen) != 1 {
		t.Errorf("onUnresolved reports = %v, want 1", seen)
	}
}

func TestRestorerIgnoresMalformedEvents(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	// Not our business to police the provider's framing: an event we cannot
	// parse is relayed untouched rather than turned into an error that severs a
	// working stream.
	for _, raw := range [][]byte{
		[]byte("event: ping\n\n"),
		[]byte("data: not json\n\n"),
		[]byte(": comment\n\n"),
		[]byte("\n\n"),
	} {
		out, err := r.Event(raw)
		if err != nil {
			t.Errorf("Event(%q) errored: %v", raw, err)
		}
		if string(out) != string(raw) {
			t.Errorf("Event(%q) = %q, want it unchanged", raw, out)
		}
	}
}

// TestRestorerBodyRestoresUnderStructuralKeys is the non-streaming half of a
// symmetry the request side does not have. Inbound, "name"/"id"/"type" are
// skipped so the filter cannot corrupt the protocol. Outbound, a placeholder
// under one of those keys can only be there because we minted it, so skipping
// it emits our own placeholder to the client and does not even count it.
func TestRestorerBodyRestoresUnderStructuralKeys(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "acme-prod.internal")
	var unresolved []string
	r := NewRestorer(tab, Passthrough, func(s string) { unresolved = append(unresolved, s) })

	for _, key := range []string{"name", "id", "type", "note"} {
		body := []byte(`{"` + key + `":"prefix ` + p + ` suffix"}`)
		out, err := r.Body(body)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if !strings.Contains(string(out), "acme-prod.internal") {
			t.Errorf("placeholder under %q was not restored: %s", key, out)
		}
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved callback fired %d times for placeholders that resolve", len(unresolved))
	}
}

// TestRestorerBodyCountsUnresolvedUnderStructuralKeys: the old skip meant an
// unrestorable placeholder under "name" was neither restored NOR counted, so
// Unresolved stayed 0 while the agent received a placeholder.
func TestRestorerBodyCountsUnresolvedUnderStructuralKeys(t *testing.T) {
	var unresolved []string
	r := NewRestorer(NewTable(testKey), Passthrough, func(s string) { unresolved = append(unresolved, s) })
	if _, err := r.Body([]byte(`{"name":"orphan [[AIPROXY_SECRET_deadbeef]]"}`)); err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 {
		t.Errorf("unresolved = %v, want exactly one placeholder reported", unresolved)
	}
}

// TestStreamingAndNonStreamingAgreeOnTheSameToolCall is the regression that
// motivated the change: identical content restored differently depending only on
// stream:true|false is a property-1 violation, and one of the two answers was
// silently wrong.
func TestStreamingAndNonStreamingAgreeOnTheSameToolCall(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "acme-prod.internal")

	nonStreaming := []byte(`{"type":"tool_use","name":"` + p + `","input":{"host":"` + p + `"}}`)
	got, err := NewRestorer(tab, Passthrough, nil).Body(nonStreaming)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"tool_use","name":"acme-prod.internal","input":{"host":"acme-prod.internal"}}`
	if string(got) != want {
		t.Errorf("non-streaming:\n got %s\nwant %s", got, want)
	}

	// The same values arriving as a block start: both fields must resolve too.
	start, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{
			"type": "tool_use", "name": p, "input": map[string]any{"host": p},
		},
	})
	out, err := NewRestorer(tab, Passthrough, nil).Event(ev(string(start)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "AIPROXY_") {
		t.Errorf("content_block_start left a placeholder behind: %s", out)
	}
	if strings.Count(string(out), "acme-prod.internal") != 2 {
		t.Errorf("content_block_start restored %d of 2 fields: %s", strings.Count(string(out), "acme-prod.internal"), out)
	}
}

// The response path takes the same walker over bytes the PROVIDER chose, so a
// compromised or simply buggy upstream must not be able to kill the proxy with
// nesting. An unparseable body is already relayed untouched — the depth refusal
// has to take that same path rather than sever the stream, because the client
// may well handle the response perfectly well.
func TestRestorerBodyRelaysAPathologicallyNestedBodyUntouched(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	// Deep enough to be the real thing rather than a proxy for it: without the
	// walker's depth bound this exact document takes the process down with
	// "fatal error: stack overflow" — not a failing test, a dead proxy.
	body := nested(5_000_000)
	out, err := r.Body(body)
	if err != nil {
		t.Fatalf("Body() = %v, want a deeply nested response relayed, not refused", err)
	}
	if string(out) != string(body) {
		t.Errorf("body was altered:\n got %s\nwant %s", out, body)
	}
}
