package privacy

import "encoding/json"

// jsonInner escapes s as JSON string CONTENT — the bytes that would appear
// between the quotes — without the quotes themselves.
//
// This is the inner of two escaping levels on the input_json_delta path. The
// fragments of that delta concatenate into the tool's input document, and a
// placeholder inside it sits inside a string literal of THAT document; so a
// restored value containing a quote, a backslash, or a newline has to be escaped
// for it. The outer level — escaping partial_json itself for the SSE event's
// JSON — is json.Marshal's job when the event is re-encoded.
//
// Getting this wrong does not leak a secret. It produces a tool input the agent
// cannot parse, or worse, one it parses differently: a file write that lands
// mangled content on the operator's disk. That is why it has its own test file.
func jsonInner(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail; returning s unchanged is the
		// conservative branch if it somehow did.
		return s
	}
	return string(b[1 : len(b)-1])
}
