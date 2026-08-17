package testutil

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func TestFakeUpstreamServesScriptsInOrderThenRepeatsLast(t *testing.T) {
	up := NewFakeUpstream(t,
		Script{Status: 429},
		Script{Status: 200, Body: `{"ok":true}`},
	)

	codes := []int{}
	for i := 0; i < 3; i++ {
		res, err := http.Get(up.URL() + "/v1/messages")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		codes = append(codes, res.StatusCode)
	}

	want := []int{429, 200, 200} // last script repeats
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("request %d: got %d, want %d", i, codes[i], want[i])
		}
	}
	if got := len(up.Requests()); got != 3 {
		t.Errorf("recorded %d requests, want 3", got)
	}
}

func TestFakeUpstreamRecordsRequestBodyAndHeaders(t *testing.T) {
	up := NewFakeUpstream(t, Script{Status: 200})

	req, _ := http.NewRequest("POST", up.URL()+"/v1/messages", strings("hello"))
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	rec := up.Requests()[0]
	if rec.Method != "POST" || rec.Path != "/v1/messages" {
		t.Errorf("got %s %s", rec.Method, rec.Path)
	}
	if string(rec.Body) != "hello" {
		t.Errorf("body = %q, want %q", rec.Body, "hello")
	}
	if rec.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("authorization not recorded")
	}
}

func TestFakeUpstreamStreamsSSEWithDelays(t *testing.T) {
	up := NewFakeUpstream(t, Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []SSEChunk{
			{Delay: 0, Data: "event: a\n\n"},
			{Delay: 60 * time.Millisecond, Data: "event: b\n\n"},
		},
	})

	res, err := http.Get(up.URL() + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	start := time.Now()
	buf := make([]byte, 256)
	n, _ := res.Body.Read(buf)
	firstAt := time.Since(start)
	if n == 0 {
		t.Fatal("no first chunk")
	}
	if firstAt > 40*time.Millisecond {
		t.Errorf("first chunk arrived after %v, expected promptly", firstAt)
	}

	n, _ = res.Body.Read(buf)
	secondAt := time.Since(start)
	if n == 0 {
		t.Fatal("no second chunk")
	}
	if secondAt < 50*time.Millisecond {
		t.Errorf("second chunk arrived after %v, expected >= 50ms", secondAt)
	}
}

func strings(s string) *stringReader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
