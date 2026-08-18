package privacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func inputJSONDelta(index int, partial string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
	return ev(string(b))
}

// assembledInput concatenates every partial_json fragment in a stream and
// unmarshals the result — exactly what the agent does before acting on a tool
// call. If this fails to parse, the agent's file write fails.
func assembledInput(t *testing.T, stream string) map[string]any {
	t.Helper()
	var b strings.Builder
	for _, chunk := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(chunk, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m struct {
				Delta struct {
					Type    string `json:"type"`
					Partial string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("undecodable event: %v", err)
			}
			if m.Delta.Type == "input_json_delta" {
				b.WriteString(m.Delta.Partial)
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(b.String()), &out); err != nil {
		t.Fatalf("assembled tool input is not valid JSON: %v\n%s", err, b.String())
	}
	return out
}

// The headline case: a file write whose content contains a secret carrying every
// character that needs escaping. The agent must receive the original content
// byte-exactly.
func TestInputJSONDeltaRestoresContentExactly(t *testing.T) {
	const secret = "line1\nkey=\"quoted\"\\slash\ttab"
	tab := NewTable(testKey)
	p, err := tab.Add(LabelSecret, secret)
	if err != nil {
		t.Fatal(err)
	}

	// What upstream sends: the tool input document, with the placeholder where
	// the secret was.
	toolInput := map[string]any{"file_path": "/etc/app.conf", "content": "before\n" + p + "\nafter"}
	doc, err := json.Marshal(toolInput)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRestorer(tab, Passthrough, nil)
	// Split the document into 5-byte fragments, the way a real stream arrives.
	var events [][]byte
	for i := 0; i < len(doc); i += 5 {
		j := i + 5
		if j > len(doc) {
			j = len(doc)
		}
		events = append(events, inputJSONDelta(0, string(doc[i:j])))
	}
	events = append(events, ev(`{"type":"content_block_stop","index":0}`))

	got := assembledInput(t, emitted(t, r, events...))
	if got["file_path"] != "/etc/app.conf" {
		t.Errorf("file_path = %v", got["file_path"])
	}
	want := "before\n" + secret + "\nafter"
	if got["content"] != want {
		t.Errorf("content = %q\nwant %q", got["content"], want)
	}
}

// Byte-level invariance: every fragment size must produce the same assembled
// document. A single escaping bug shows up at one particular split.
func TestInputJSONDeltaIsInvariantAcrossFragmentSizes(t *testing.T) {
	const secret = `p@ss"w\ord`
	tab := NewTable(testKey)
	p, _ := tab.Add(LabelSecret, secret)
	doc, _ := json.Marshal(map[string]any{"content": "x" + p + "y"})

	for size := 1; size <= len(doc); size++ {
		r := NewRestorer(tab, Passthrough, nil)
		var events [][]byte
		for i := 0; i < len(doc); i += size {
			j := i + size
			if j > len(doc) {
				j = len(doc)
			}
			events = append(events, inputJSONDelta(0, string(doc[i:j])))
		}
		events = append(events, ev(`{"type":"content_block_stop","index":0}`))

		got := assembledInput(t, emitted(t, r, events...))
		if got["content"] != "x"+secret+"y" {
			t.Fatalf("fragment size %d gave content %q, want %q", size, got["content"], "x"+secret+"y")
		}
	}
}

// With nothing to restore, the fragments must be untouched — a tool call that
// contained no secret must not be re-encoded at all.
func TestInputJSONDeltaWithNothingToRestoreIsByteIdentical(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	raw := inputJSONDelta(0, `{"file_path":"/a/b","content":"plain"}`)
	out, err := r.Event(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Errorf("fragment was rewritten:\n got %q\nwant %q", out, raw)
	}
}

func TestJSONInnerEscaping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`plain`, `plain`},
		{`with "quotes"`, `with \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{"new\nline", `new\nline`},
		{"tab\there", `tab\there`},
	} {
		if got := jsonInner(c.in); got != c.want {
			t.Errorf("jsonInner(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The escaping is only applied to input_json_delta. A text_delta carrying the
// same secret must come back raw, because there is only one level of escaping
// there and double-escaping would show the agent literal backslashes.
func TestTextDeltaIsNotDoubleEscaped(t *testing.T) {
	const secret = `has "quotes" and \slash`
	tab := NewTable(testKey)
	p, _ := tab.Add(LabelSecret, secret)
	r := NewRestorer(tab, Passthrough, nil)
	stream := emitted(t, r, textDelta(0, "value: "+p))
	if got := textOf(t, stream); got != "value: "+secret {
		t.Errorf("text delta = %q, want %q", got, "value: "+secret)
	}
}
