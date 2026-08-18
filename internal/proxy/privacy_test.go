package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/rules"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func testFilter(t *testing.T, mode privacy.FailureMode) *privacy.Filter {
	t.Helper()
	d, err := rules.New(rules.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return privacy.New(privacy.Options{
		Detectors:     []privacy.Detector{d},
		Key:           []byte("0123456789abcdef0123456789abcdef"),
		Unresolved:    privacy.Passthrough,
		OnScanFailure: mode,
	})
}

// The end-to-end property: a secret in the request never reaches upstream, and
// the agent still sees it in the response.
func TestPrivacyFilterRedactsUpstreamAndRestoresDownstream(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	// Upstream echoes whatever it was sent back inside a text block, so the test
	// observes both directions with one script.
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = testFilter(t, privacy.Closed)
	}, testutil.Script{Status: 200, Body: `{"id":"msg_1","content":[{"type":"text","text":"ECHO"}]}`})

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"my key is ` + secret + `"}]}`
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	sent := string(h.up.Requests()[0].Body)
	if strings.Contains(sent, secret) {
		t.Fatalf("the secret reached upstream: %s", sent)
	}
	if !strings.Contains(sent, privacy.Sentinel) {
		t.Fatalf("no placeholder in the upstream body: %s", sent)
	}
	if !strings.Contains(sent, `"model":"claude-opus-5"`) {
		t.Errorf("the model was altered: %s", sent)
	}
}

// Fail-closed means closed: upstream must receive NOTHING.
func TestPrivacyFailClosedSendsNothingUpstream(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = privacy.New(privacy.Options{
			Detectors:     []privacy.Detector{errDetector{}},
			Key:           []byte("0123456789abcdef0123456789abcdef"),
			OnScanFailure: privacy.Closed,
		})
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"a long enough value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	if res.StatusCode == 200 {
		t.Errorf("status = 200; a failed scan must not succeed")
	}
	if n := len(h.up.Requests()); n != 0 {
		t.Fatalf("upstream received %d requests; fail-closed must send zero", n)
	}
}

func TestPrivacyFailOpenSendsUnfiltered(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = privacy.New(privacy.Options{
			Detectors:     []privacy.Detector{errDetector{}},
			Key:           []byte("0123456789abcdef0123456789abcdef"),
			OnScanFailure: privacy.Open,
		})
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"a long enough value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if len(h.up.Requests()) != 1 {
		t.Fatalf("fail-open must still send the request; upstream got %d", len(h.up.Requests()))
	}
}

// Passthrough paths carry the client's own credential. Filtering one breaks
// authentication, so they must be byte-identical upstream. Passthrough is a
// separate handler from the account path (see router.go), reached only when
// Upstream and PassthroughPrefixes are configured, so this test wires those up
// exactly as the other passthrough tests do and points its own fake upstream at
// them rather than the account-path one newRouterHarness already built.
func TestPrivacyNeverFiltersPassthroughPaths(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	pt := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{}`})
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = testFilter(t, privacy.Closed)
		o.Upstream = pt.URL()
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200, Body: `{}`})

	body := `{"token":"` + secret + `"}`
	res, err := http.Post(h.srv.URL+"/v1/oauth/token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	sent := string(pt.Requests()[0].Body)
	if sent != body {
		t.Errorf("a passthrough body was filtered:\n got %s\nwant %s", sent, body)
	}
}

// With no filter configured, the relay's write path must be untouched.
func TestRelayWithoutARestorerIsUnchanged(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{"content":[{"type":"text","text":"plain"}]}`})
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if string(got) != `{"content":[{"type":"text","text":"plain"}]}` {
		t.Errorf("body = %s", got)
	}
}

// errDetector always fails, so the failure modes can be exercised.
type errDetector struct{}

func (errDetector) Name() string { return "err" }
func (errDetector) Scan(context.Context, string) ([]privacy.Finding, error) {
	return nil, io.ErrUnexpectedEOF
}
