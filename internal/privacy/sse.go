package privacy

import "bytes"

// dataPayload finds the "data:" line in one raw SSE event and returns its index
// among the event's lines plus the payload bytes.
//
// Anthropic sends exactly one data line per event. Multi-line data — legal SSE,
// where the payload is the lines joined by "\n" — is deliberately NOT handled:
// ok is false and the caller relays the event untouched, which is the safe
// outcome for a shape this code has never seen.
func dataPayload(raw []byte) (int, []byte, bool) {
	lines := bytes.Split(raw, []byte("\n"))
	idx, count := -1, 0
	for i, l := range lines {
		if bytes.HasPrefix(l, []byte("data:")) {
			idx, count = i, count+1
		}
	}
	if count != 1 {
		return 0, nil, false
	}
	payload := bytes.TrimPrefix(lines[idx], []byte("data:"))
	return idx, bytes.TrimPrefix(payload, []byte(" ")), true
}

// replacePayload rebuilds raw with the data line's payload swapped out,
// preserving every other line and the event's trailing blank line exactly.
func replacePayload(raw []byte, line int, payload []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	lines[line] = append([]byte("data: "), payload...)
	return bytes.Join(lines, []byte("\n"))
}
