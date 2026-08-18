package anthropic

import (
	"net/http"
	"testing"
)

func TestParseUsageBodyReadsANonStreamingResponse(t *testing.T) {
	p := New(http.DefaultClient)
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant",
	  "content":[{"type":"text","text":"hi"}],
	  "usage":{"input_tokens":42,"output_tokens":7,
	           "cache_read_input_tokens":300,"cache_creation_input_tokens":9}}`)

	got, ok := p.ParseUsageBody(body)
	if !ok {
		t.Fatal("a non-streaming message body carries usage and must be parsed")
	}
	if got.InputTokens != 42 || got.OutputTokens != 7 ||
		got.CacheReadTokens != 300 || got.CacheWriteTokens != 9 {
		t.Errorf("usage = %+v, want 42/7/300/9", got)
	}
}

func TestParseUsageBodyIgnoresBodiesWithoutUsage(t *testing.T) {
	p := New(http.DefaultClient)
	for _, body := range []string{
		`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`,
		`{"input_tokens": 5}`, // count_tokens shape: no usage envelope
		`not json at all`,
		``,
	} {
		if _, ok := p.ParseUsageBody([]byte(body)); ok {
			t.Errorf("body %q should not report usage", body)
		}
	}
}
