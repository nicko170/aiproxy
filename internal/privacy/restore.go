package privacy

import (
	"encoding/json"
	"fmt"
)

// Restorer substitutes plaintext back into a response, event by event.
//
// One per response. Not safe for concurrent use, which matches how Relay reads a
// body: one goroutine, one event at a time.
type Restorer struct {
	table        *Table
	mode         UnresolvedMode
	onUnresolved func(string)
	// blocks holds one rewriter per content-block index, because each block is
	// its own independent text stream and a placeholder never spans two.
	blocks map[int]*blockState
}

// blockState is one content block's rewriter plus which delta field carries its
// text, so a synthetic flush event can be built with the right shape.
type blockState struct {
	rw        *rewriter
	deltaType string // text_delta | thinking_delta | input_json_delta
	field     string // text | thinking | partial_json
}

func NewRestorer(table *Table, mode UnresolvedMode, onUnresolved func(string)) *Restorer {
	return &Restorer{
		table: table, mode: mode, onUnresolved: onUnresolved,
		blocks: map[int]*blockState{},
	}
}

// deltaFields maps a delta type to the field holding its text.
var deltaFields = map[string]string{
	"text_delta":       "text",
	"thinking_delta":   "thinking",
	"input_json_delta": "partial_json",
}

// Event rewrites one complete raw SSE event.
//
// The returned bytes may contain MORE than one event: a content_block_stop with
// bytes still withheld is preceded by a synthetic delta carrying them, so nothing
// is dropped and the stop still arrives last.
//
// An event that needs no change is returned as the original bytes. That is not an
// optimisation — it is what makes "a stream with no sentinel is passed through
// byte-for-byte" true rather than approximately true.
func (r *Restorer) Event(raw []byte) ([]byte, error) {
	line, payload, ok := dataPayload(raw)
	if !ok {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		// Unparseable is the provider's business, not ours. Relaying it untouched
		// beats severing a stream that the client might handle perfectly well.
		return raw, nil
	}

	switch m["type"] {
	case "content_block_delta":
		return r.delta(raw, line, m)
	case "content_block_start":
		return r.blockStart(raw, line, m)
	case "content_block_stop":
		return r.blockStop(raw, m)
	default:
		return raw, nil
	}
}

func (r *Restorer) delta(raw []byte, line int, m map[string]any) ([]byte, error) {
	delta, _ := m["delta"].(map[string]any)
	if delta == nil {
		return raw, nil
	}
	deltaType, _ := delta["type"].(string)
	field, known := deltaFields[deltaType]
	if !known {
		return raw, nil
	}
	text, isString := delta[field].(string)
	if !isString {
		return raw, nil
	}

	st := r.block(indexOf(m), deltaType, field)
	out, err := st.rewrite(text)
	if err != nil {
		return nil, err
	}
	// Unchanged and nothing withheld: hand back the original bytes.
	if out == text && st.rw.Pending() == 0 {
		return raw, nil
	}
	delta[field] = out
	return r.reencode(raw, line, m)
}

func (r *Restorer) blockStart(raw []byte, line int, m map[string]any) ([]byte, error) {
	// A new block supersedes any state under the same index; a stale rewriter
	// here would leak one block's pending bytes into the next.
	idx := indexOf(m)
	delete(r.blocks, idx)

	block, _ := m["content_block"].(map[string]any)
	if block == nil {
		return raw, nil
	}
	text, isString := block["text"].(string)
	if !isString || text == "" {
		return raw, nil
	}
	// A complete value: restore it whole with a throwaway rewriter, so no
	// pending state is created for a block that has not started streaming.
	w := newRewriter(r.table, r.mode, r.onUnresolved)
	out, err := w.Write(text)
	if err != nil {
		return nil, err
	}
	tail, err := w.Flush()
	if err != nil {
		return nil, err
	}
	if out+tail == text {
		return raw, nil
	}
	block["text"] = out + tail
	return r.reencode(raw, line, m)
}

func (r *Restorer) blockStop(raw []byte, m map[string]any) ([]byte, error) {
	idx := indexOf(m)
	st, ok := r.blocks[idx]
	if !ok {
		return raw, nil
	}
	delete(r.blocks, idx)

	tail, err := st.rw.Flush()
	if err != nil {
		return nil, err
	}
	if tail == "" {
		return raw, nil
	}
	synthetic, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": st.deltaType, st.field: tail},
	})
	if err != nil {
		return nil, fmt.Errorf("privacy: encode flush event: %w", err)
	}
	out := append([]byte("event: content_block_delta\ndata: "), synthetic...)
	out = append(out, '\n', '\n')
	return append(out, raw...), nil
}

// reencode rebuilds the event around a modified payload.
//
// The payload is re-marshalled from a map, so key order changes and unknown
// fields survive. Order does not matter here: unlike the request side, nothing
// downstream hashes these bytes — the client parses them.
func (r *Restorer) reencode(raw []byte, line int, m map[string]any) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("privacy: encode event: %w", err)
	}
	return replacePayload(raw, line, payload), nil
}

func (r *Restorer) block(idx int, deltaType, field string) *blockState {
	if st, ok := r.blocks[idx]; ok {
		return st
	}
	st := &blockState{deltaType: deltaType, field: field}
	if deltaType == "input_json_delta" {
		st.rw = newJSONRewriter(r.table, r.mode, r.onUnresolved)
	} else {
		st.rw = newRewriter(r.table, r.mode, r.onUnresolved)
	}
	r.blocks[idx] = st
	return st
}

// rewrite feeds text through this block's rewriter. The escaping difference
// between a text stream and a JSON-fragment stream is carried by the rewriter
// chosen in block(), so there is exactly one place that decision is made.
func (st *blockState) rewrite(text string) (string, error) {
	return st.rw.Write(text)
}

// indexOf reads a content block index. JSON numbers decode as float64 through
// map[string]any; a missing or non-numeric index is treated as block 0, which is
// what a single-block response uses anyway.
func indexOf(m map[string]any) int {
	if f, ok := m["index"].(float64); ok {
		return int(f)
	}
	return 0
}

// Body restores a complete non-streaming response.
//
// It reuses the request side's machinery: the same JSON walker finds string
// values, and each is rewritten whole. Structural keys are skipped for the same
// reason they are on the way in — rewriting "model" or "id" would corrupt the
// response — and the original bytes are returned when nothing changes.
func (r *Restorer) Body(body []byte) ([]byte, error) {
	spans, err := WalkStrings(body)
	if err != nil {
		return body, nil // an unparseable body is the provider's business
	}
	type replacement struct {
		start, end int
		literal    []byte
	}
	var reps []replacement
	for _, span := range spans {
		if SkipKey(span.Key, span.ParentKey) {
			continue
		}
		w := newRewriter(r.table, r.mode, r.onUnresolved)
		out, err := w.Write(span.Value)
		if err != nil {
			return nil, err
		}
		tail, err := w.Flush()
		if err != nil {
			return nil, err
		}
		if out+tail == span.Value {
			continue
		}
		lit, err := json.Marshal(out + tail)
		if err != nil {
			return nil, fmt.Errorf("privacy: encode restored value: %w", err)
		}
		reps = append(reps, replacement{start: span.Start, end: span.End, literal: lit})
	}
	if len(reps) == 0 {
		return body, nil
	}
	out := make([]byte, len(body))
	copy(out, body)
	// Last span first, for the same reason Redact does it: an earlier rewrite
	// would invalidate every offset after it.
	for i := len(reps) - 1; i >= 0; i-- {
		rep := reps[i]
		tail := append([]byte{}, out[rep.end:]...)
		out = append(out[:rep.start], rep.literal...)
		out = append(out, tail...)
	}
	return out, nil
}
