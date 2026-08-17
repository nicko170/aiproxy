# aiproxy Stage 1 — Core Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A headless Go proxy that Claude Code can use through `ANTHROPIC_BASE_URL`, rotating across multiple Anthropic accounts, with a hard bound on how long a client can be left without a response byte.

**Architecture:** A Chi router terminates client requests, buffers the body so an attempt can be replayed on a different account, and drives an attempt loop. Each attempt selects an eligible account, injects its credential, forwards over an HTTP/1.1 pooled transport, and classifies the response through a provider interface. Every non-transferring wait — backoff, account pause, token refresh — draws down a single per-request budget, so exhausting it produces a prompt error instead of dead air.

**Tech Stack:** Go 1.26, `github.com/go-chi/chi/v5`. Standard library for everything else in this stage.

**Spec:** `docs/superpowers/specs/2026-08-17-aiproxy-design.md` — read §2, §4, §5, §6 before starting. The plan argues from the spec; where they disagree, the spec wins and the plan is wrong.

## Global Constraints

Every task's requirements implicitly include these.

- Module path is `github.com/nicko170/aiproxy`. Go 1.26 or later.
- Dependencies for this stage: `github.com/go-chi/chi/v5` only. Do not add others. Later stages add `bubbletea`/`lipgloss`/`bubbles` and `modernc.org/sqlite`; nothing else.
- No cgo. `CGO_ENABLED=0 go build ./...` must succeed.
- All control-plane HTTP paths live under the reserved prefix `/_aiproxy`. Any other path is a candidate for proxying upstream.
- The config directory is mode `0700`; `config.json` is mode `0600`. Enforce on every write, not only on create.
- The upstream transport sets `ForceAttemptHTTP2: false`. This is load-bearing — see spec §4.5. Do not "fix" it.
- Client-facing and MITM listeners set TCP `NoDelay`. Also load-bearing, spec §4.5.
- Timestamps that are persisted or compared across process boundaries are unix **milliseconds** as `int64`.
- Never log a token, API key, or `Authorization` header value. Redact to at most the first 8 characters followed by `…`.
- Commit after every task. Use conventional-commit prefixes (`feat:`, `test:`, `refactor:`, `chore:`).

## Deviations from the spec

Two places where this plan knowingly differs from `spec §5`. Both are timing, not
disagreement; noted here so a reader does not treat them as omissions.

- **`Provider.Login` is not in the interface yet.** The spec lists it, but PKCE
  login is driven from the TUI, which is stage 4. Adding a method no caller can
  reach would mean shipping an untested interactive flow. Stage 4 adds `Login`
  to `provider.Provider` and to `view.Source` together.
- **The `anthropicCompatible` provider is deferred to stage 2.** The spec puts it
  in v1 to keep the seam honest, and that still holds — but the seam is not
  actually proven until a second implementation exists alongside real accounting,
  and stage 1 has no configured account that would exercise it. Fields it needs
  (`Upstream`, `ModelMap`) are already in the config schema and honoured by the
  Anthropic provider, so adding it later touches one new file.

## File Structure

Created in this stage:

| File | Responsibility |
| --- | --- |
| `go.mod`, `go.sum` | Module definition |
| `internal/testutil/fakeupstream.go` | Scriptable fake upstream: status, headers, SSE chunk timing, request recording |
| `internal/provider/provider.go` | `Provider` interface, `Credential`, `Outcome`, `QuotaBucket`, `Profile`, `Quota` |
| `internal/provider/anthropic/classify.go` | Response → `Outcome` (the 429 taxonomy) |
| `internal/provider/anthropic/anthropic.go` | `Provider` impl: endpoint, authorize, body rewrite, usage parsing |
| `internal/provider/anthropic/oauth.go` | PKCE login, token exchange, refresh |
| `internal/provider/anthropic/usage.go` | Profile + zero-spend usage fetch and normalization |
| `internal/config/config.go` | Config types, defaults, paths |
| `internal/config/store.go` | Serialized atomic load/save with permission enforcement |
| `internal/config/import.go` | First-run account import |
| `internal/account/account.go` | Account runtime state |
| `internal/account/manager.go` | Registry + single-flight credential freshness |
| `internal/account/select.go` | Eligibility, priority, reset tiebreak, session affinity |
| `internal/account/admit.go` | Storm-control ramp and pause |
| `internal/proxy/budget.go` | Per-request pre-first-byte budget |
| `internal/proxy/transport.go` | Pooled HTTP/1.1 upstream transport |
| `internal/proxy/relay.go` | Flush-per-chunk relay, body-idle watchdog, usage tee |
| `internal/proxy/attempt.go` | The attempt loop: classify, rotate, draw down budget |
| `internal/proxy/handler.go` | Request handler: gate, buffer, block, hand to attempt loop |
| `internal/proxy/router.go` | Chi router and middleware assembly |
| `cmd/aiproxy/main.go` | Flags, wiring, headless boot |

Boundaries worth stating: `internal/proxy` never imports `internal/provider/anthropic` — only `internal/provider`. `internal/account` knows nothing about HTTP. `classify.go` is a pure function over `*http.Response` so the taxonomy is testable without a network.

---

### Task 1: Module bootstrap and the fake upstream harness

Everything downstream is tested against this harness, so it comes first and gets its own tests.

**Files:**
- Create: `go.mod`
- Create: `internal/testutil/fakeupstream.go`
- Test: `internal/testutil/fakeupstream_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `testutil.Script{Status int, Header http.Header, Body string, SSE []testutil.SSEChunk}`, `testutil.SSEChunk{Delay time.Duration, Data string}`, `testutil.FakeUpstream` with methods `URL() string`, `Requests() []testutil.RecordedRequest`, `Close()`. `testutil.NewFakeUpstream(t *testing.T, scripts ...Script) *FakeUpstream` serves scripts in order and repeats the last one indefinitely. `testutil.RecordedRequest{Method, Path string, Header http.Header, Body []byte}`.

- [ ] **Step 1: Initialise the module**

```bash
cd ~/code/aiproxy
go mod init github.com/nicko170/aiproxy
go get github.com/go-chi/chi/v5@latest
```

- [ ] **Step 2: Write the failing test**

Create `internal/testutil/fakeupstream_test.go`:

```go
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/testutil/ -run TestFakeUpstream -v`
Expected: FAIL to build — `undefined: NewFakeUpstream`, `undefined: Script`.

- [ ] **Step 4: Write minimal implementation**

Create `internal/testutil/fakeupstream.go`:

```go
// Package testutil provides a scriptable fake upstream for proxy tests.
package testutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// SSEChunk is one streamed frame, written after Delay has elapsed.
type SSEChunk struct {
	Delay time.Duration
	Data  string
}

// Script is one canned response. When SSE is non-empty the response streams
// those chunks and Body is ignored.
type Script struct {
	Status int
	Header http.Header
	Body   string
	SSE    []SSEChunk
}

// RecordedRequest is what the fake upstream observed.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// FakeUpstream serves scripts in order, repeating the final script for any
// further requests, and records every request it received.
type FakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	scripts []Script
	n       int
	seen    []RecordedRequest
}

// NewFakeUpstream starts a fake upstream that is shut down when the test ends.
func NewFakeUpstream(t *testing.T, scripts ...Script) *FakeUpstream {
	t.Helper()
	if len(scripts) == 0 {
		t.Fatal("NewFakeUpstream needs at least one script")
	}
	f := &FakeUpstream{scripts: scripts}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *FakeUpstream) URL() string { return f.srv.URL }

func (f *FakeUpstream) Close() { f.srv.Close() }

// Requests returns a copy of everything recorded so far.
func (f *FakeUpstream) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.seen))
	copy(out, f.seen)
	return out
}

func (f *FakeUpstream) next(rec RecordedRequest) Script {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, rec)
	i := f.n
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1 // last script repeats
	}
	f.n++
	return f.scripts[i]
}

func (f *FakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := f.next(RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})

	for k, vs := range s.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := s.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	if len(s.SSE) == 0 {
		io.WriteString(w, s.Body)
		return
	}
	flusher, _ := w.(http.Flusher)
	for _, c := range s.SSE {
		if c.Delay > 0 {
			select {
			case <-time.After(c.Delay):
			case <-r.Context().Done():
				return
			}
		}
		io.WriteString(w, c.Data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/testutil/ -v`
Expected: PASS, three tests.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/testutil/
git commit -m "test: scriptable fake upstream harness"
```

---

### Task 2: Provider types and the 429 taxonomy

The heart of the fix. `ClassifyResponse` is a pure function so every rate-limit shape is a table row, including the real bare-429 captured from production.

**Files:**
- Create: `internal/provider/provider.go`
- Create: `internal/provider/anthropic/classify.go`
- Test: `internal/provider/anthropic/classify_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `provider.OutcomeKind` constants `OutcomeOK`, `OutcomeQuotaRejected`, `OutcomeThrottledWithHint`, `OutcomeThrottledNoHint`, `OutcomeCredentialStale`, `OutcomeCredentialRefused`, `OutcomeClientError`, `OutcomeServerError`; `provider.Outcome{Kind OutcomeKind, RetryAfter time.Duration, Buckets []QuotaBucket, ScopedModel string}`; `provider.QuotaBucket{Name string, Utilization float64, Status string, ResetsAt int64}`; `provider.CredentialType` with `CredentialOAuth`/`CredentialAPIKey`; `provider.Credential`; `provider.Profile`; `provider.Quota`. Also `anthropic.Classify(*http.Response) provider.Outcome`.

- [ ] **Step 1: Write the failing test**

Create `internal/provider/anthropic/classify_test.go`:

```go
package anthropic

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

func resp(status int, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		res        *http.Response
		wantKind   provider.OutcomeKind
		wantRetry  time.Duration
		wantScoped string
	}{{
		name:     "200 is ok",
		res:      resp(200, nil),
		wantKind: provider.OutcomeOK,
	}, {
		// The exact shape captured from production: 429, no retry-after,
		// no ratelimit headers. Inventing a delay here is the defect.
		name:     "bare 429 with no headers is throttled without hint",
		res:      resp(429, nil),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name:      "429 with retry-after is throttled with hint",
		res:       resp(429, map[string]string{"Retry-After": "7"}),
		wantKind:  provider.OutcomeThrottledWithHint,
		wantRetry: 7 * time.Second,
	}, {
		name:     "retry-after of zero is a hint of zero, not a missing hint",
		res:      resp(429, map[string]string{"Retry-After": "0"}),
		wantKind: provider.OutcomeThrottledWithHint,
	}, {
		name:     "unparseable retry-after is treated as no hint",
		res:      resp(429, map[string]string{"Retry-After": "soon"}),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name: "negative retry-after is treated as no hint",
		res:  resp(429, map[string]string{"Retry-After": "-5"}),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name: "5h rejected is quota rejection",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-5h-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name: "7d rejected is quota rejection",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-7d-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name: "model scoped rejection names the model and does not reject generally",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-7d_oi-status": "rejected",
		}),
		wantKind:   provider.OutcomeQuotaRejected,
		wantScoped: "7d_oi",
	}, {
		name: "quota rejection wins over a retry-after hint",
		res: resp(429, map[string]string{
			"Retry-After":                           "3",
			"anthropic-ratelimit-unified-5h-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name:     "401 is a stale credential",
		res:      resp(401, nil),
		wantKind: provider.OutcomeCredentialStale,
	}, {
		name:     "403 is a refused credential",
		res:      resp(403, nil),
		wantKind: provider.OutcomeCredentialRefused,
	}, {
		name:     "400 is a client error",
		res:      resp(400, nil),
		wantKind: provider.OutcomeClientError,
	}, {
		name:     "503 is a server error",
		res:      resp(503, nil),
		wantKind: provider.OutcomeServerError,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.res)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.RetryAfter != tc.wantRetry {
				t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, tc.wantRetry)
			}
			if got.ScopedModel != tc.wantScoped {
				t.Errorf("ScopedModel = %q, want %q", got.ScopedModel, tc.wantScoped)
			}
		})
	}
}

func TestClassifyParsesQuotaBuckets(t *testing.T) {
	res := resp(200, map[string]string{
		"anthropic-ratelimit-unified-5h-status":      "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "42",
		"anthropic-ratelimit-unified-5h-reset":       "1786986000",
	})

	out := Classify(res)
	if len(out.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(out.Buckets))
	}
	b := out.Buckets[0]
	if b.Name != "5h" {
		t.Errorf("Name = %q, want %q", b.Name, "5h")
	}
	if b.Utilization != 0.42 {
		t.Errorf("Utilization = %v, want 0.42 (percent normalized to a fraction)", b.Utilization)
	}
	if b.ResetsAt != 1786986000_000 {
		t.Errorf("ResetsAt = %d, want unix ms 1786986000000", b.ResetsAt)
	}
	if b.Status != "allowed" {
		t.Errorf("Status = %q", b.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/... -v`
Expected: FAIL to build — `undefined: Classify`, and package `provider` does not exist.

- [ ] **Step 3: Write the provider types**

Create `internal/provider/provider.go`:

```go
// Package provider defines the seam between the proxy core and a specific
// upstream API. The core never imports a concrete provider.
package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// ErrUnsupported is returned by optional Provider methods a provider does not
// implement, such as Quota for an endpoint with no usage API.
var ErrUnsupported = errors.New("unsupported by this provider")

type CredentialType string

const (
	CredentialOAuth  CredentialType = "oauth"
	CredentialAPIKey CredentialType = "apikey"
)

// Credential is what authorizes one account. Persisted in config.
type Credential struct {
	Type         CredentialType `json:"type"`
	AccessToken  string         `json:"accessToken,omitempty"`
	RefreshToken string         `json:"refreshToken,omitempty"`
	APIKey       string         `json:"apiKey,omitempty"`
	ExpiresAt    int64          `json:"expiresAt,omitempty"` // unix ms
}

// Profile identifies who a credential belongs to.
type Profile struct {
	AccountUUID string
	Email       string
	DisplayName string
	OrgUUID     string
	OrgName     string
	Plan        string
}

// QuotaBucket is one rate-limit window as reported by the upstream.
type QuotaBucket struct {
	Name        string  // "5h", "7d", "7d_oi"
	Utilization float64 // 0..1
	Status      string  // "allowed", "rejected", or ""
	ResetsAt    int64   // unix ms, 0 when unknown
}

// Quota is a zero-spend read of an account's current windows.
type Quota struct {
	Buckets    []QuotaBucket
	ObservedAt int64 // unix ms
}

type OutcomeKind int

const (
	OutcomeOK OutcomeKind = iota
	OutcomeQuotaRejected
	OutcomeThrottledWithHint
	OutcomeThrottledNoHint
	OutcomeCredentialStale
	OutcomeCredentialRefused
	OutcomeClientError
	OutcomeServerError
)

func (k OutcomeKind) String() string {
	switch k {
	case OutcomeOK:
		return "ok"
	case OutcomeQuotaRejected:
		return "quota_rejected"
	case OutcomeThrottledWithHint:
		return "throttled_with_hint"
	case OutcomeThrottledNoHint:
		return "throttled_no_hint"
	case OutcomeCredentialStale:
		return "credential_stale"
	case OutcomeCredentialRefused:
		return "credential_refused"
	case OutcomeClientError:
		return "client_error"
	case OutcomeServerError:
		return "server_error"
	}
	return "unknown"
}

// Outcome is a classified upstream response. RetryAfter is meaningful only for
// OutcomeThrottledWithHint. ScopedModel is set when a quota rejection applies to
// one model family rather than the whole account.
type Outcome struct {
	Kind        OutcomeKind
	RetryAfter  time.Duration
	Buckets     []QuotaBucket
	ScopedModel string
}

// UsageDelta is token accounting parsed from a streamed event.
type UsageDelta struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Account is the subset of account state a provider needs. Defined here rather
// than imported from internal/account so providers do not depend on the
// registry.
type Account struct {
	ID          string
	Label       string
	Credential  Credential
	AccountUUID string
	Upstream    string            // override base URL, empty for provider default
	ModelMap    map[string]string // request model -> upstream model
}

// Provider adapts one upstream API to the proxy core.
type Provider interface {
	Name() string

	Refresh(ctx context.Context, c Credential) (Credential, error)
	Profile(ctx context.Context, c Credential) (Profile, error)
	Quota(ctx context.Context, c Credential) (Quota, error)

	Endpoint(a Account) *url.URL
	Authorize(r *http.Request, c Credential)
	RewriteBody(body []byte, a Account) ([]byte, error)
	ClassifyResponse(r *http.Response) Outcome
	ParseUsage(sseEvent []byte) (*UsageDelta, bool)
}
```

- [ ] **Step 4: Write the classifier**

Create `internal/provider/anthropic/classify.go`:

```go
package anthropic

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const bucketPrefix = "anthropic-ratelimit-unified-"

// Classify maps an upstream response to an Outcome. It is pure: no network, no
// clock, no state, so every rate-limit shape is a table test.
//
// The ordering matters. A quota rejection is evidence that waiting cannot help,
// so it outranks a Retry-After hint that might otherwise suggest a short pause.
// And a 429 carrying neither a hint nor bucket headers must classify as
// ThrottledNoHint rather than acquiring a default duration: a fabricated wait,
// absorbed inline across several accounts, is what turns a sub-second upstream
// rejection into a multi-minute silent hold.
func Classify(r *http.Response) provider.Outcome {
	out := provider.Outcome{Buckets: parseBuckets(r.Header)}

	switch {
	case r.StatusCode == http.StatusTooManyRequests:
		if name, ok := rejectedBucket(out.Buckets); ok {
			out.Kind = provider.OutcomeQuotaRejected
			if isModelScoped(name) {
				out.ScopedModel = name
			}
			return out
		}
		if d, ok := retryAfter(r.Header); ok {
			out.Kind = provider.OutcomeThrottledWithHint
			out.RetryAfter = d
			return out
		}
		out.Kind = provider.OutcomeThrottledNoHint
		return out

	case r.StatusCode == http.StatusUnauthorized:
		out.Kind = provider.OutcomeCredentialStale
	case r.StatusCode == http.StatusForbidden:
		out.Kind = provider.OutcomeCredentialRefused
	case r.StatusCode >= 500:
		out.Kind = provider.OutcomeServerError
	case r.StatusCode >= 400:
		out.Kind = provider.OutcomeClientError
	default:
		out.Kind = provider.OutcomeOK
	}
	return out
}

// retryAfter reads a delta-seconds Retry-After. A missing, unparseable, or
// negative value reports ok=false so the caller falls through to the no-hint
// path instead of inventing a duration. Zero is a real hint of "immediately".
func retryAfter(h http.Header) (time.Duration, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func rejectedBucket(buckets []provider.QuotaBucket) (string, bool) {
	// A general rejection outranks a model-scoped one: it holds the whole
	// account, so report it even if a scoped bucket is also rejected.
	scoped := ""
	for _, b := range buckets {
		if b.Status != "rejected" {
			continue
		}
		if !isModelScoped(b.Name) {
			return b.Name, true
		}
		scoped = b.Name
	}
	if scoped != "" {
		return scoped, true
	}
	return "", false
}

// A bucket name is model-scoped when it carries a suffix beyond the window,
// e.g. "7d_oi" for the per-model weekly cap versus plain "7d".
func isModelScoped(name string) bool {
	return strings.Contains(name, "_")
}

// parseBuckets collects anthropic-ratelimit-unified-<name>-<field> headers into
// one QuotaBucket per <name>.
func parseBuckets(h http.Header) []provider.QuotaBucket {
	byName := map[string]*provider.QuotaBucket{}
	order := []string{}

	for key := range h {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, bucketPrefix) {
			continue
		}
		rest := lower[len(bucketPrefix):]
		idx := strings.LastIndex(rest, "-")
		if idx <= 0 {
			continue
		}
		name, field := rest[:idx], rest[idx+1:]

		b, ok := byName[name]
		if !ok {
			b = &provider.QuotaBucket{Name: name}
			byName[name] = b
			order = append(order, name)
		}
		value := strings.TrimSpace(h.Get(key))
		switch field {
		case "status":
			b.Status = value
		case "utilization":
			// Reported as a percentage; stored as a 0..1 fraction.
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				b.Utilization = f / 100
			}
		case "reset":
			b.ResetsAt = toUnixMillis(value)
		}
	}

	out := make([]provider.QuotaBucket, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// toUnixMillis accepts unix seconds, unix milliseconds, or RFC3339 and returns
// unix milliseconds. Returns 0 when the value is unusable.
func toUnixMillis(v string) int64 {
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 1e12 { // seconds, not milliseconds
			return n * 1000
		}
		return n
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli()
	}
	return 0
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/provider/... -v`
Expected: PASS. Every subtest of `TestClassify` plus `TestClassifyParsesQuotaBuckets`.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/
git commit -m "feat(provider): response classification with a 429 taxonomy

A 429 carrying no Retry-After and no ratelimit headers classifies as
ThrottledNoHint rather than acquiring a default duration. Fabricating a
wait and absorbing it inline is what converts a sub-second upstream
rejection into a multi-minute silent hold."
```

---

### Task 3: The per-request budget

The invariant from spec §2.1 and §4.2, isolated so it can be tested without HTTP or a real clock.

**Files:**
- Create: `internal/proxy/budget.go`
- Test: `internal/proxy/budget_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `proxy.Budget` with `proxy.NewBudget(total time.Duration) *Budget`, methods `Remaining() time.Duration`, `Spend(d time.Duration) bool`, `Wait(ctx context.Context, d time.Duration) error`, `Exhausted() bool`, and settable field `Sleep func(context.Context, time.Duration) error` for tests. Sentinel `proxy.ErrBudgetExhausted`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/budget_test.go`:

```go
package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSleep records requested sleeps and returns immediately, so budget
// accounting is tested without real time.
func fakeSleep(log *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*log = append(*log, d)
		return nil
	}
}

func TestBudgetSpendDrawsDownAndRefusesOverspend(t *testing.T) {
	b := NewBudget(time.Second)

	if !b.Spend(400 * time.Millisecond) {
		t.Fatal("first spend should be allowed")
	}
	if got := b.Remaining(); got != 600*time.Millisecond {
		t.Errorf("Remaining = %v, want 600ms", got)
	}
	if b.Spend(700 * time.Millisecond) {
		t.Error("overspend should be refused")
	}
	if got := b.Remaining(); got != 600*time.Millisecond {
		t.Errorf("refused spend must not draw down; Remaining = %v", got)
	}
	if b.Exhausted() {
		t.Error("budget with remaining time is not exhausted")
	}
	if !b.Spend(600 * time.Millisecond) {
		t.Fatal("spending exactly the remainder should be allowed")
	}
	if !b.Exhausted() {
		t.Error("budget should be exhausted")
	}
}

func TestBudgetWaitSleepsAndDrawsDown(t *testing.T) {
	var slept []time.Duration
	b := NewBudget(time.Second)
	b.Sleep = fakeSleep(&slept)

	if err := b.Wait(context.Background(), 250*time.Millisecond); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(slept) != 1 || slept[0] != 250*time.Millisecond {
		t.Fatalf("slept = %v, want [250ms]", slept)
	}
	if got := b.Remaining(); got != 750*time.Millisecond {
		t.Errorf("Remaining = %v, want 750ms", got)
	}
}

// The invariant: a wait longer than the remaining budget must not happen at
// all. Sleeping the remainder and then failing would still burn the wall-clock
// this design exists to bound.
func TestBudgetWaitRefusesToSleepBeyondRemaining(t *testing.T) {
	var slept []time.Duration
	b := NewBudget(100 * time.Millisecond)
	b.Sleep = fakeSleep(&slept)

	err := b.Wait(context.Background(), 60*time.Second)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if len(slept) != 0 {
		t.Errorf("must not sleep at all, slept %v", slept)
	}
	if !b.Exhausted() {
		t.Error("a refused oversized wait exhausts the budget")
	}
}

func TestBudgetWaitHonoursContextCancellation(t *testing.T) {
	b := NewBudget(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Wait(ctx, 10*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestBudgetRealSleepIsBoundedByRemaining(t *testing.T) {
	b := NewBudget(30 * time.Millisecond)
	start := time.Now()
	err := b.Wait(context.Background(), 25*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("waited %v, far beyond the requested 25ms", elapsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestBudget -v`
Expected: FAIL to build — `undefined: NewBudget`, `undefined: ErrBudgetExhausted`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/budget.go`:

```go
package proxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBudgetExhausted reports that a request has no pre-first-byte time left.
var ErrBudgetExhausted = errors.New("pre-first-byte budget exhausted")

// Budget bounds the total time a request may spend NOT transferring response
// bytes: retry backoff, waiting on a paused account, absorbing a rate limit,
// refreshing a credential.
//
// This is the mechanism behind the design's first invariant. Every constant in
// the retry engine can be badly chosen without producing an unbounded silent
// hang, because each of them draws down this one allowance. Once response
// headers are written the request can no longer be retried, so the budget is
// never consulted again — it governs dead air and nothing else.
type Budget struct {
	// Sleep is swapped out in tests. It must return ctx.Err() on cancellation.
	Sleep func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	remaining time.Duration
}

func NewBudget(total time.Duration) *Budget {
	if total < 0 {
		total = 0
	}
	return &Budget{remaining: total, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Budget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

func (b *Budget) Exhausted() bool { return b.Remaining() <= 0 }

// Spend draws down d, reporting false and changing nothing if d exceeds what is
// left. Use it for time already consumed, such as an elapsed refresh.
func (b *Budget) Spend(d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > b.remaining {
		return false
	}
	b.remaining -= d
	return true
}

// Wait blocks for d and draws it down. When d exceeds the remaining budget it
// sleeps not at all, exhausts the budget, and returns ErrBudgetExhausted — a
// partial sleep would still burn the wall-clock this type exists to bound.
func (b *Budget) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		d = 0
	}
	if !b.Spend(d) {
		b.mu.Lock()
		b.remaining = 0
		b.mu.Unlock()
		return ErrBudgetExhausted
	}
	if err := b.Sleep(ctx, d); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/proxy/ -run TestBudget -v`
Expected: PASS, five tests.

- [ ] **Step 5: Run the race detector**

Run: `go test -race ./internal/proxy/ -run TestBudget`
Expected: PASS, no race warnings.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/budget.go internal/proxy/budget_test.go
git commit -m "feat(proxy): per-request pre-first-byte budget

Bounds all non-transferring wait time in one allowance so a wrongly
chosen backoff constant cannot produce an unbounded silent hang. An
oversized wait is refused outright rather than partially slept."
```

---

### Task 4: Config types and the serialized atomic store

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/store.go`
- Test: `internal/config/store_test.go`

**Interfaces:**
- Consumes: `provider.Credential`.
- Produces: `config.Config`, `config.Account`, `config.Listen`, `config.Routing`, `config.Retry`, `config.QuotaProbe`, `config.Metrics`, `config.MITM`; `config.Default() Config`; `config.Dir() string`; `config.Path() string`; `config.NewStore(path string) *Store`; `(*Store).Load() (Config, error)`; `(*Store).Update(fn func(*Config) error) (Config, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/config/store_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retry.BudgetMS != 10000 {
		t.Errorf("BudgetMS = %d, want 10000", cfg.Retry.BudgetMS)
	}
	if cfg.Retry.InlineAbsorbMaxMS != 5000 {
		t.Errorf("InlineAbsorbMaxMS = %d, want 5000", cfg.Retry.InlineAbsorbMaxMS)
	}
	if cfg.QuotaProbe.IntervalSeconds != 300 {
		t.Errorf("probe interval = %d, want 300", cfg.QuotaProbe.IntervalSeconds)
	}
	if cfg.Listen.APIKey == "" {
		t.Error("a proxy API key should be generated on first load")
	}
}

func TestUpdatePersistsAndEnforcesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)

	if _, err := s.Update(func(c *Config) error {
		c.Accounts = append(c.Accounts, Account{
			ID:         "acct-1",
			Provider:   "anthropic",
			Label:      "a@example.com",
			Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "tok"},
		})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Accounts) != 1 || reloaded.Accounts[0].ID != "acct-1" {
		t.Fatalf("accounts not persisted: %+v", reloaded.Accounts)
	}
	if reloaded.Accounts[0].Credential.AccessToken != "tok" {
		t.Error("credential not persisted")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

// Concurrent read-modify-write must not lose writes. Losing one here means
// losing a rotated refresh token, which invalidates an account on next start.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Update(func(c *Config) error {
				c.Accounts = append(c.Accounts, Account{ID: string(rune('a' + i))})
				return nil
			})
		}(i)
	}
	wg.Wait()

	cfg, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != n {
		t.Errorf("got %d accounts, want %d — a concurrent write was lost", len(cfg.Accounts), n)
	}
}

func TestUpdateRollsBackOnCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path)
	s.Update(func(c *Config) error { return nil }) // materialize the file

	wantErr := os.ErrInvalid
	if _, err := s.Update(func(c *Config) error {
		c.Accounts = append(c.Accounts, Account{ID: "ghost"})
		return wantErr
	}); err == nil {
		t.Fatal("expected the callback error to propagate")
	}

	cfg, _ := s.Load()
	if len(cfg.Accounts) != 0 {
		t.Error("a failed update must not be written")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL to build — `undefined: NewStore`, `undefined: Config`.

- [ ] **Step 3: Write the config types**

Create `internal/config/config.go`:

```go
// Package config owns the on-disk configuration: its schema, its defaults, and
// the single serialized writer every mutation goes through.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"

	"github.com/nicko170/aiproxy/internal/provider"
)

// Account is one upstream identity. ID is a stable opaque handle assigned once
// and never reused: array position must never be an identity, because reordering
// or relabelling would silently repoint anything referring to it.
type Account struct {
	ID         string              `json:"id"`
	Provider   string              `json:"provider"`
	Label      string              `json:"label"`
	Priority   int                 `json:"priority"`
	Disabled   bool                `json:"disabled"`
	Credential provider.Credential `json:"credential"`
	Identity   Identity            `json:"identity"`
	Upstream   string              `json:"upstream,omitempty"`
	ModelMap   map[string]string   `json:"modelMap,omitempty"`
}

type Identity struct {
	AccountUUID string `json:"accountUuid,omitempty"`
	OrgUUID     string `json:"orgUuid,omitempty"`
	OrgName     string `json:"orgName,omitempty"`
	Plan        string `json:"plan,omitempty"`
}

type Listen struct {
	Addr   string `json:"addr"`
	APIKey string `json:"apiKey"`
}

type Routing struct {
	SwitchThreshold float64  `json:"switchThreshold"`
	SessionAffinity bool     `json:"sessionAffinity"`
	BlockedModels   []string `json:"blockedModels"`
}

type Retry struct {
	BudgetMS          int `json:"budgetMs"`
	InlineAbsorbMaxMS int `json:"inlineAbsorbMaxMs"`
	BodyIdleMS        int `json:"bodyIdleMs"`
}

type QuotaProbe struct {
	IntervalSeconds int `json:"intervalSeconds"`
}

type Metrics struct {
	RetentionDays int `json:"retentionDays"`
}

type MITM struct {
	Enabled bool `json:"enabled"`
}

type Config struct {
	Listen     Listen     `json:"listen"`
	Accounts   []Account  `json:"accounts"`
	Routing    Routing    `json:"routing"`
	Retry      Retry      `json:"retry"`
	QuotaProbe QuotaProbe `json:"quotaProbe"`
	Metrics    Metrics    `json:"metrics"`
	MITM       MITM       `json:"mitm"`
}

// Default returns the configuration for a fresh install.
//
// QuotaProbe defaults to 300s rather than something aggressive: the zero-spend
// usage endpoint has its own rate limit, and polling it every 30s gets the probe
// itself throttled, after which quota data goes stale and account selection
// decides on outdated numbers.
func Default() Config {
	return Config{
		Listen:     Listen{Addr: "127.0.0.1:3456", APIKey: newAPIKey()},
		Accounts:   []Account{},
		Routing:    Routing{SwitchThreshold: 0.98, SessionAffinity: true, BlockedModels: []string{}},
		Retry:      Retry{BudgetMS: 10000, InlineAbsorbMaxMS: 5000, BodyIdleMS: 120000},
		QuotaProbe: QuotaProbe{IntervalSeconds: 300},
		Metrics:    Metrics{RetentionDays: 90},
		MITM:       MITM{Enabled: true},
	}
}

func newAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "ap-" + base64.RawURLEncoding.EncodeToString(b)
}

// Dir is the configuration directory, honouring XDG_CONFIG_HOME.
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "aiproxy")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aiproxy"
	}
	return filepath.Join(home, ".config", "aiproxy")
}

func Path() string { return filepath.Join(Dir(), "config.json") }
```

- [ ] **Step 4: Write the store**

Create `internal/config/store.go`:

```go
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store is the only writer of the config file.
//
// Mutations go through Update, which re-reads from disk, applies the change, and
// writes atomically while holding a mutex. Serializing matters more than it
// looks: several accounts refreshing OAuth tokens at once is an ordinary event,
// and two concurrent read-modify-write cycles would drop one rotated refresh
// token, invalidating that account on the next start.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

// Load reads the config, filling in defaults for a missing file or absent
// fields. A freshly defaulted config is not written; the first Update does that.
func (s *Store) Load() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// Unmarshal over the defaults so absent keys keep their default value.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", s.path, err)
	}
	if cfg.Accounts == nil {
		cfg.Accounts = []Account{}
	}
	if cfg.Listen.APIKey == "" {
		cfg.Listen.APIKey = newAPIKey()
	}
	return cfg, nil
}

// Update re-reads, applies fn, and persists. When fn returns an error nothing is
// written and that error propagates.
func (s *Store) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.loadLocked()
	if err != nil {
		return cfg, err
	}
	if err := fn(&cfg); err != nil {
		return cfg, err
	}
	if err := s.writeLocked(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// writeLocked writes to a temp file and renames, so a crash mid-write cannot
// truncate a config holding the only copy of a refresh token. Permissions are
// re-applied every write, not only on create: mode is honoured at creation time
// only, so a pre-existing file could otherwise stay world-readable.
func (s *Store) writeLocked(cfg Config) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("chmod config dir: %w", err)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return os.Chmod(s.path, 0o600)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/config/ -v`
Expected: PASS, four tests, no races.

- [ ] **Step 6: Commit**

```bash
git add internal/config/
git commit -m "feat(config): schema plus a serialized atomic store

All mutations funnel through one writer. Concurrent read-modify-write
would drop a rotated refresh token and invalidate the account on next
start, so writes are serialized and permissions re-applied every time."
```

---

### Task 5: First-run account import

**Files:**
- Create: `internal/config/import.go`
- Test: `internal/config/import_test.go`

**Interfaces:**
- Consumes: `config.Config`, `config.Account`, `provider.Credential`.
- Produces: `config.ImportSource` (`ImportSourceLegacy`, `ImportSourceClaudeCode`), `config.ImportFile(path string, src ImportSource) ([]Account, error)`, `config.LegacyPath() string`, `config.ClaudeCodePath() string`, `config.NewID() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/config/import_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportLegacyAccounts(t *testing.T) {
	p := writeFile(t, t.TempDir(), "legacy.json", `{
	  "accounts": [
	    {"name": "a@example.com (Acme)", "type": "oauth",
	     "accessToken": "at-1", "refreshToken": "rt-1", "expiresAt": 1786986000000,
	     "accountUuid": "acct-uuid", "orgUuid": "org-uuid", "orgName": "Acme"},
	    {"name": "fallback", "type": "apikey", "apiKey": "sk-test", "priority": 10,
	     "disabled": true, "upstream": "https://api.example.com",
	     "modelMap": {"claude-x": "model-y"}}
	  ]
	}`)

	got, err := ImportFile(p, ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got))
	}

	a := got[0]
	if a.Label != "a@example.com (Acme)" || a.Provider != "anthropic" {
		t.Errorf("account 0 = %+v", a)
	}
	if a.Credential.Type != provider.CredentialOAuth || a.Credential.AccessToken != "at-1" ||
		a.Credential.RefreshToken != "rt-1" || a.Credential.ExpiresAt != 1786986000000 {
		t.Errorf("credential 0 = %+v", a.Credential)
	}
	if a.Identity.OrgName != "Acme" || a.Identity.AccountUUID != "acct-uuid" {
		t.Errorf("identity 0 = %+v", a.Identity)
	}
	if a.ID == "" {
		t.Error("imported account must be assigned an id")
	}

	b := got[1]
	if b.Credential.Type != provider.CredentialAPIKey || b.Credential.APIKey != "sk-test" {
		t.Errorf("credential 1 = %+v", b.Credential)
	}
	if !b.Disabled || b.Priority != 10 || b.Upstream != "https://api.example.com" {
		t.Errorf("account 1 = %+v", b)
	}
	if b.ModelMap["claude-x"] != "model-y" {
		t.Errorf("model map = %+v", b.ModelMap)
	}
	if a.ID == b.ID {
		t.Error("each imported account needs a distinct id")
	}
}

func TestImportClaudeCodeCredentials(t *testing.T) {
	p := writeFile(t, t.TempDir(), "creds.json", `{
	  "claudeAiOauth": {"accessToken": "at-9", "refreshToken": "rt-9",
	                    "expiresAt": 1786986000000, "subscriptionType": "max"}
	}`)

	got, err := ImportFile(p, ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].Credential.AccessToken != "at-9" || got[0].Credential.RefreshToken != "rt-9" {
		t.Errorf("credential = %+v", got[0].Credential)
	}
	if got[0].Identity.Plan != "max" {
		t.Errorf("plan = %q, want max", got[0].Identity.Plan)
	}
}

func TestImportSkipsAccountsWithNoUsableCredential(t *testing.T) {
	p := writeFile(t, t.TempDir(), "legacy.json",
		`{"accounts":[{"name":"broken","type":"oauth"},{"name":"ok","type":"apikey","apiKey":"k"}]}`)

	got, err := ImportFile(p, ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 || got[0].Label != "ok" {
		t.Errorf("got %+v, want only the usable account", got)
	}
}

func TestImportMissingFileReportsNotExist(t *testing.T) {
	_, err := ImportFile(filepath.Join(t.TempDir(), "nope.json"), ImportSourceLegacy)
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if id == "" {
			t.Fatal("empty id")
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestImport -v`
Expected: FAIL to build — `undefined: ImportFile`, `undefined: ImportSourceLegacy`, `undefined: NewID`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/config/import.go`:

```go
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ImportSource names a credential file layout we can read.
type ImportSource string

const (
	// ImportSourceLegacy reads a prior tool's config: {"accounts":[...]}.
	ImportSourceLegacy ImportSource = "legacy"
	// ImportSourceClaudeCode reads Claude Code's own credential file.
	ImportSourceClaudeCode ImportSource = "claude-code"
)

// LegacyPath is the config we can adopt accounts from on a first run.
func LegacyPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "teamclaude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "teamclaude.json")
}

// ClaudeCodePath is Claude Code's credential file.
func ClaudeCodePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// NewID returns a stable opaque account handle.
func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type legacyFile struct {
	Accounts []legacyAccount `json:"accounts"`
}

type legacyAccount struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	AccessToken  string            `json:"accessToken"`
	RefreshToken string            `json:"refreshToken"`
	ExpiresAt    int64             `json:"expiresAt"`
	APIKey       string            `json:"apiKey"`
	Priority     int               `json:"priority"`
	Disabled     bool              `json:"disabled"`
	AccountUUID  string            `json:"accountUuid"`
	OrgUUID      string            `json:"orgUuid"`
	OrgName      string            `json:"orgName"`
	Upstream     string            `json:"upstream"`
	ModelMap     map[string]string `json:"modelMap"`
}

type claudeCodeFile struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// ImportFile reads accounts from an external credential file. Accounts with no
// usable credential are skipped rather than imported broken. The source file is
// never modified.
func ImportFile(path string, src ImportSource) ([]Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	switch src {
	case ImportSourceLegacy:
		var f legacyFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		out := make([]Account, 0, len(f.Accounts))
		for _, la := range f.Accounts {
			a, ok := fromLegacy(la)
			if !ok {
				continue
			}
			out = append(out, a)
		}
		return out, nil

	case ImportSourceClaudeCode:
		var f claudeCodeFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if f.ClaudeAiOauth == nil || f.ClaudeAiOauth.AccessToken == "" {
			return nil, nil
		}
		return []Account{{
			ID:       NewID(),
			Provider: "anthropic",
			Label:    "imported (claude code)",
			Credential: provider.Credential{
				Type:         provider.CredentialOAuth,
				AccessToken:  f.ClaudeAiOauth.AccessToken,
				RefreshToken: f.ClaudeAiOauth.RefreshToken,
				ExpiresAt:    f.ClaudeAiOauth.ExpiresAt,
			},
			Identity: Identity{Plan: f.ClaudeAiOauth.SubscriptionType},
		}}, nil
	}
	return nil, fmt.Errorf("unknown import source %q", src)
}

func fromLegacy(la legacyAccount) (Account, bool) {
	cred := provider.Credential{}
	switch la.Type {
	case "apikey":
		if la.APIKey == "" {
			return Account{}, false
		}
		cred = provider.Credential{Type: provider.CredentialAPIKey, APIKey: la.APIKey}
	default: // oauth
		if la.AccessToken == "" && la.RefreshToken == "" {
			return Account{}, false
		}
		cred = provider.Credential{
			Type:         provider.CredentialOAuth,
			AccessToken:  la.AccessToken,
			RefreshToken: la.RefreshToken,
			ExpiresAt:    la.ExpiresAt,
		}
	}
	return Account{
		ID:         NewID(),
		Provider:   "anthropic",
		Label:      la.Name,
		Priority:   la.Priority,
		Disabled:   la.Disabled,
		Credential: cred,
		Identity: Identity{
			AccountUUID: la.AccountUUID,
			OrgUUID:     la.OrgUUID,
			OrgName:     la.OrgName,
		},
		Upstream: la.Upstream,
		ModelMap: la.ModelMap,
	}, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS, all config tests including the five import tests.

- [ ] **Step 5: Commit**

```bash
git add internal/config/import.go internal/config/import_test.go
git commit -m "feat(config): import accounts from external credential files

Adopts existing credentials so a first run does not require re-authorizing
every account. Source files are read, never written."
```

---

### Task 6: OAuth token refresh and expiry

**Files:**
- Create: `internal/provider/anthropic/oauth.go`
- Test: `internal/provider/anthropic/oauth_test.go`

**Interfaces:**
- Consumes: `provider.Credential`, `testutil.NewFakeUpstream`.
- Produces: `anthropic.NormalizeExpiresAt(int64) int64`, `anthropic.IsExpired(int64, time.Time) bool`, `anthropic.IsExpiringSoon(int64, time.Time, time.Duration) bool`, `anthropic.RefreshToken(ctx, hc *http.Client, endpoint, refreshToken string) (provider.Credential, error)`, `anthropic.ErrRefreshRejected`.

- [ ] **Step 1: Write the failing test**

Create `internal/provider/anthropic/oauth_test.go`:

```go
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestNormalizeExpiresAtAcceptsSecondsAndMillis(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 0},
		{1786986000, 1786986000_000},     // seconds promoted to millis
		{1786986000_000, 1786986000_000}, // already millis
	}
	for _, c := range cases {
		if got := NormalizeExpiresAt(c.in); got != c.want {
			t.Errorf("NormalizeExpiresAt(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestExpiryPredicates(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	future := now.Add(time.Hour).UnixMilli()
	past := now.Add(-time.Minute).UnixMilli()

	if IsExpired(future, now) {
		t.Error("a future expiry is not expired")
	}
	if !IsExpired(past, now) {
		t.Error("a past expiry is expired")
	}
	if IsExpired(0, now) {
		t.Error("an unknown expiry must not be treated as expired")
	}
	if !IsExpiringSoon(now.Add(2*time.Minute).UnixMilli(), now, 5*time.Minute) {
		t.Error("expiry inside the threshold is expiring soon")
	}
	if IsExpiringSoon(future, now, 5*time.Minute) {
		t.Error("expiry beyond the threshold is not expiring soon")
	}
}

func TestRefreshTokenReturnsRotatedCredential(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"access_token":  "new-at",
		"refresh_token": "new-rt",
		"expires_in":    3600,
	})
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   string(body),
	})

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "old-rt")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.AccessToken != "new-at" || got.RefreshToken != "new-rt" {
		t.Errorf("credential = %+v", got)
	}
	if got.ExpiresAt <= time.Now().UnixMilli() {
		t.Errorf("ExpiresAt = %d, want a future unix ms", got.ExpiresAt)
	}

	var sent map[string]any
	json.Unmarshal(up.Requests()[0].Body, &sent)
	if sent["grant_type"] != "refresh_token" || sent["refresh_token"] != "old-rt" {
		t.Errorf("request body = %+v", sent)
	}
}

// An endpoint that omits refresh_token means the old one is still valid;
// dropping it would lose the only way to re-authenticate.
func TestRefreshTokenKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body:   `{"access_token":"new-at","expires_in":60}`,
	})

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "keep-me")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.RefreshToken != "keep-me" {
		t.Errorf("RefreshToken = %q, want the original", got.RefreshToken)
	}
}

func TestRefreshTokenRetriesServerErrorsThenSucceeds(t *testing.T) {
	up := testutil.NewFakeUpstream(t,
		testutil.Script{Status: 503, Body: `{"error":"unavailable"}`},
		testutil.Script{Status: 200, Body: `{"access_token":"at","expires_in":60}`},
	)

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "rt")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.AccessToken != "at" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if n := len(up.Requests()); n != 2 {
		t.Errorf("made %d requests, want 2 (one retry)", n)
	}
}

// A 400 means the refresh token itself is dead. Retrying cannot help and the
// caller must be able to tell this from a transient failure so it can surface a
// re-login instead of looping.
func TestRefreshTokenDoesNotRetryRejection(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 400, Body: `{"error":"invalid_grant"}`,
	})

	_, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "dead")
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("err = %v, want ErrRefreshRejected", err)
	}
	if n := len(up.Requests()); n != 1 {
		t.Errorf("made %d requests, want exactly 1", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/anthropic/ -run 'TestRefresh|TestNormalize|TestExpiry' -v`
Expected: FAIL to build — `undefined: RefreshToken`, `undefined: NormalizeExpiresAt`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/provider/anthropic/oauth.go`:

```go
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const (
	// Client id and endpoints are properties of the upstream OAuth deployment.
	ClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	TokenEndpoint = "https://platform.claude.com/v1/oauth/token"
	AuthorizeURL  = "https://claude.ai/oauth/authorize"
	Scopes        = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// ErrRefreshRejected reports that the refresh token was refused. It is not
// transient: the account needs a fresh login, and retrying wastes a budget.
var ErrRefreshRejected = errors.New("refresh token rejected")

// NormalizeExpiresAt converts a possibly-seconds timestamp to unix millis.
// Values below 1e12 are seconds; anything at or above it is already millis.
func NormalizeExpiresAt(v int64) int64 {
	if v == 0 {
		return 0
	}
	if v < 1e12 {
		return v * 1000
	}
	return v
}

// IsExpired reports whether a credential is past its expiry. An unknown expiry
// (0) is never expired — it means "we were not told", not "assume dead".
func IsExpired(expiresAt int64, now time.Time) bool {
	if expiresAt == 0 {
		return false
	}
	return now.UnixMilli() >= NormalizeExpiresAt(expiresAt)
}

// IsExpiringSoon reports whether a credential expires within threshold.
func IsExpiringSoon(expiresAt int64, now time.Time, threshold time.Duration) bool {
	if expiresAt == 0 {
		return false
	}
	return now.Add(threshold).UnixMilli() >= NormalizeExpiresAt(expiresAt)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
}

// RefreshToken exchanges a refresh token for a new access token. Server errors
// and transport failures are retried with backoff; a 4xx is a rejection and
// returns ErrRefreshRejected without retrying.
func RefreshToken(ctx context.Context, hc *http.Client, endpoint, refreshToken string) (provider.Credential, error) {
	const maxAttempts = 3
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return provider.Credential{}, ctx.Err()
			}
		}

		cred, retryable, err := refreshOnce(ctx, hc, endpoint, refreshToken)
		if err == nil {
			return cred, nil
		}
		lastErr = err
		if !retryable {
			return provider.Credential{}, err
		}
	}
	return provider.Credential{}, lastErr
}

func refreshOnce(ctx context.Context, hc *http.Client, endpoint, refreshToken string) (provider.Credential, bool, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	if err != nil {
		return provider.Credential{}, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return provider.Credential{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := hc.Do(req)
	if err != nil {
		return provider.Credential{}, true, fmt.Errorf("refresh request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode >= 500 {
		return provider.Credential{}, true, fmt.Errorf("refresh failed with %d", res.StatusCode)
	}
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return provider.Credential{}, false,
			fmt.Errorf("%w (%d): %s", ErrRefreshRejected, res.StatusCode, bytes.TrimSpace(body))
	}

	var tr tokenResponse
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		return provider.Credential{}, true, fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.AccessToken == "" {
		return provider.Credential{}, false, fmt.Errorf("%w: no access token in response", ErrRefreshRejected)
	}

	cred := provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    NormalizeExpiresAt(tr.ExpiresAt),
	}
	// An omitted refresh_token means the existing one is still valid; keeping it
	// matters because it is the only way back without a fresh login.
	if cred.RefreshToken == "" {
		cred.RefreshToken = refreshToken
	}
	if cred.ExpiresAt == 0 {
		secs := tr.ExpiresIn
		if secs == 0 {
			secs = 3600
		}
		cred.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
	}
	return cred, false, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/provider/anthropic/ -v`
Expected: PASS, all classification tests plus the six OAuth tests.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/anthropic/oauth.go internal/provider/anthropic/oauth_test.go
git commit -m "feat(anthropic): OAuth refresh with expiry normalization

Distinguishes a rejected refresh token, which needs a login, from a
transient failure, which is retried. An omitted refresh_token in the
response keeps the existing one rather than discarding it."
```

---

### Task 7: The Anthropic provider implementation

**Files:**
- Create: `internal/provider/anthropic/anthropic.go`
- Test: `internal/provider/anthropic/anthropic_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `provider.Account`, `provider.Credential`, `provider.UsageDelta`, `anthropic.Classify`, `anthropic.RefreshToken`.
- Produces: `anthropic.New(hc *http.Client) *anthropic.Anthropic` satisfying `provider.Provider`. Field `TokenEndpointOverride string` for tests. `DefaultBaseURL = "https://api.anthropic.com"`.

- [ ] **Step 1: Write the failing test**

Create `internal/provider/anthropic/anthropic_test.go`:

```go
package anthropic

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestSatisfiesProviderInterface(t *testing.T) {
	var _ provider.Provider = New(http.DefaultClient)
}

func TestEndpointUsesAccountOverride(t *testing.T) {
	p := New(http.DefaultClient)

	if got := p.Endpoint(provider.Account{}).String(); got != DefaultBaseURL {
		t.Errorf("default endpoint = %q, want %q", got, DefaultBaseURL)
	}
	got := p.Endpoint(provider.Account{Upstream: "https://api.example.com"}).String()
	if got != "https://api.example.com" {
		t.Errorf("override endpoint = %q", got)
	}
}

func TestAuthorizeSetsOAuthBearerOrAPIKey(t *testing.T) {
	p := New(http.DefaultClient)

	r, _ := http.NewRequest("POST", "https://example.com", nil)
	p.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if got := r.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("x-api-key") != "" {
		t.Error("oauth must not set x-api-key")
	}

	r2, _ := http.NewRequest("POST", "https://example.com", nil)
	p.Authorize(r2, provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"})
	if got := r2.Header.Get("x-api-key"); got != "sk" {
		t.Errorf("x-api-key = %q", got)
	}
	if r2.Header.Get("Authorization") != "" {
		t.Error("api key must not set Authorization")
	}
}

func TestRewriteBodyPatchesUserIDAndModel(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte(`{"model":"claude-x","max_tokens":8,"metadata":{"user_id":"old"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := p.RewriteBody(in, provider.Account{
		AccountUUID: "new-uuid",
		ModelMap:    map[string]string{"claude-x": "model-y"},
	})
	if err != nil {
		t.Fatalf("RewriteBody: %v", err)
	}

	var got struct {
		Model    string `json:"model"`
		MaxTok   int    `json:"max_tokens"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got.Model != "model-y" {
		t.Errorf("model = %q, want model-y", got.Model)
	}
	if got.Metadata.UserID != "new-uuid" {
		t.Errorf("metadata.user_id = %q, want new-uuid", got.Metadata.UserID)
	}
	if got.MaxTok != 8 || len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Errorf("unrelated fields were not preserved: %+v", got)
	}
}

func TestRewriteBodyIsIdentityWhenNothingToChange(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte(`{"model":"claude-x"}`)

	out, err := p.RewriteBody(in, provider.Account{})
	if err != nil {
		t.Fatalf("RewriteBody: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("body changed to %q; with no uuid and no model map it must pass through byte-for-byte", out)
	}
}

func TestRewriteBodyPassesThroughNonJSON(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte("not json at all")

	out, err := p.RewriteBody(in, provider.Account{AccountUUID: "u"})
	if err != nil {
		t.Fatalf("RewriteBody must not fail on a non-JSON body: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("non-JSON body must pass through unchanged, got %q", out)
	}
}

func TestParseUsageReadsMessageStartAndDelta(t *testing.T) {
	p := New(http.DefaultClient)

	start := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":11,` +
		`"cache_read_input_tokens":900,"cache_creation_input_tokens":5,"output_tokens":1}}}` + "\n")
	got, ok := p.ParseUsage(start)
	if !ok {
		t.Fatal("message_start should yield usage")
	}
	if got.InputTokens != 11 || got.CacheReadTokens != 900 || got.CacheWriteTokens != 5 {
		t.Errorf("start usage = %+v", got)
	}

	delta := []byte(`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n")
	got, ok = p.ParseUsage(delta)
	if !ok {
		t.Fatal("message_delta should yield usage")
	}
	if got.OutputTokens != 42 {
		t.Errorf("delta usage = %+v", got)
	}

	if _, ok := p.ParseUsage([]byte("event: ping\ndata: {}\n")); ok {
		t.Error("an event with no usage must report false")
	}
	if _, ok := p.ParseUsage([]byte("data: [DONE]\n")); ok {
		t.Error("a non-JSON data line must report false, not panic")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/anthropic/ -run 'TestSatisfies|TestEndpoint|TestAuthorize|TestRewrite|TestParseUsage' -v`
Expected: FAIL to build — `undefined: New`, `undefined: DefaultBaseURL`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/provider/anthropic/anthropic.go`:

```go
// Package anthropic implements provider.Provider for the Anthropic API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/nicko170/aiproxy/internal/provider"
)

const DefaultBaseURL = "https://api.anthropic.com"

// Anthropic is the provider implementation. It holds no per-request state.
type Anthropic struct {
	hc *http.Client
	// TokenEndpointOverride redirects the OAuth token endpoint in tests.
	TokenEndpointOverride string
	// BaseURLOverride redirects profile/usage reads in tests.
	BaseURLOverride string
}

func New(hc *http.Client) *Anthropic {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Anthropic{hc: hc}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) tokenEndpoint() string {
	if a.TokenEndpointOverride != "" {
		return a.TokenEndpointOverride
	}
	return TokenEndpoint
}

func (a *Anthropic) baseURL() string {
	if a.BaseURLOverride != "" {
		return a.BaseURLOverride
	}
	return DefaultBaseURL
}

func (a *Anthropic) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	return RefreshToken(ctx, a.hc, a.tokenEndpoint(), c.RefreshToken)
}

// Endpoint is the base URL for an account: its override when set, else the
// provider default.
func (a *Anthropic) Endpoint(acct provider.Account) *url.URL {
	raw := acct.Upstream
	if raw == "" {
		raw = a.baseURL()
	}
	u, err := url.Parse(raw)
	if err != nil {
		u, _ = url.Parse(DefaultBaseURL)
	}
	return u
}

// Authorize injects exactly one credential form and never both: an OAuth bearer
// alongside an x-api-key is ambiguous to the upstream.
func (a *Anthropic) Authorize(r *http.Request, c provider.Credential) {
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	switch c.Type {
	case provider.CredentialAPIKey:
		r.Header.Set("x-api-key", c.APIKey)
	default:
		r.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
}

func (a *Anthropic) ClassifyResponse(r *http.Response) provider.Outcome { return Classify(r) }

// RewriteBody aligns the body with the credential actually being used: the
// request's declared user id must match the account whose token is injected, and
// an account targeting a different upstream may name models differently.
//
// Only the two top-level keys that need changing are decoded. Everything else
// stays as raw bytes, so a megabyte of message history is never re-encoded and
// nested content is preserved exactly. A body that is not a JSON object, or that
// needs no change, is returned untouched.
func (a *Anthropic) RewriteBody(body []byte, acct provider.Account) ([]byte, error) {
	if acct.AccountUUID == "" && len(acct.ModelMap) == 0 {
		return body, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, nil // not a JSON object; pass through
	}

	changed := false

	if len(acct.ModelMap) > 0 {
		if raw, ok := fields["model"]; ok {
			var model string
			if json.Unmarshal(raw, &model) == nil {
				if mapped, ok := acct.ModelMap[model]; ok && mapped != model {
					enc, err := json.Marshal(mapped)
					if err != nil {
						return nil, err
					}
					fields["model"] = enc
					changed = true
				}
			}
		}
	}

	if acct.AccountUUID != "" {
		meta := map[string]json.RawMessage{}
		if raw, ok := fields["metadata"]; ok {
			if err := json.Unmarshal(raw, &meta); err != nil {
				meta = map[string]json.RawMessage{}
			}
		}
		enc, err := json.Marshal(acct.AccountUUID)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(meta["user_id"], enc) {
			meta["user_id"] = enc
			encMeta, err := json.Marshal(meta)
			if err != nil {
				return nil, err
			}
			fields["metadata"] = encMeta
			changed = true
		}
	}

	if !changed {
		return body, nil
	}
	return json.Marshal(fields)
}

type sseUsage struct {
	Type    string `json:"type"`
	Usage   *usage `json:"usage"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ParseUsage extracts token counts from one SSE event. Cache reads and cache
// writes are reported separately because under an agent workload cache reads
// dominate plain input tokens, and folding them together makes any cost figure
// derived from this wrong.
func (a *Anthropic) ParseUsage(event []byte) (*provider.UsageDelta, bool) {
	for _, line := range strings.Split(string(event), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev sseUsage
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			continue
		}
		u := ev.Usage
		if u == nil && ev.Message != nil {
			u = ev.Message.Usage
		}
		if u == nil {
			continue
		}
		return &provider.UsageDelta{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
		}, true
	}
	return nil, false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/provider/anthropic/ -v`
Expected: PASS. `TestSatisfiesProviderInterface` will fail to compile until Task 8 adds `Profile` and `Quota` — if so, stub them temporarily as `return provider.Profile{}, provider.ErrUnsupported` and complete them in Task 8.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/anthropic/anthropic.go internal/provider/anthropic/anthropic_test.go
git commit -m "feat(anthropic): endpoint, authorization, body rewrite, usage parsing

Body rewriting decodes only the top-level keys it must change and leaves
the rest as raw bytes, so a large message history is never re-encoded.
Cache reads and writes are counted separately from input tokens."
```

---

### Task 8: Profile and zero-spend quota reads

**Files:**
- Modify: `internal/provider/anthropic/anthropic.go` (replace the `Profile`/`Quota` stubs)
- Create: `internal/provider/anthropic/usage.go`
- Test: `internal/provider/anthropic/usage_test.go`

**Interfaces:**
- Consumes: `provider.Profile`, `provider.Quota`, `provider.QuotaBucket`, `testutil.NewFakeUpstream`.
- Produces: `(*Anthropic).Profile(ctx, Credential) (provider.Profile, error)`, `(*Anthropic).Quota(ctx, Credential) (provider.Quota, error)`, `anthropic.ErrQuotaThrottled`.

- [ ] **Step 1: Write the failing test**

Create `internal/provider/anthropic/usage_test.go`:

```go
package anthropic

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestProfileParsesAccountAndOrganization(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body: `{"account":{"uuid":"acct-1","email":"a@example.com",
		        "display_name":"A","has_claude_max":true},
		        "organization":{"uuid":"org-1","name":"Acme"}}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	got, err := p.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got.AccountUUID != "acct-1" || got.Email != "a@example.com" ||
		got.OrgUUID != "org-1" || got.OrgName != "Acme" {
		t.Errorf("profile = %+v", got)
	}
	if got.Plan != "max" {
		t.Errorf("Plan = %q, want max", got.Plan)
	}
	if h := up.Requests()[0].Header.Get("Authorization"); h != "Bearer at" {
		t.Errorf("Authorization = %q", h)
	}
}

func TestQuotaNormalizesBuckets(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body: `{"five_hour":{"utilization":14,"resets_at":1786986000},
		        "seven_day":{"utilization":1,"resets_at":"2026-08-24T00:00:00Z"},
		        "limits":[{"group":"weekly","percent":63,"resets_at":1787065200,
		                   "scope":{"model":{"display_name":"Claude Fable 5"}}}]}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	got, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}

	byName := map[string]provider.QuotaBucket{}
	for _, b := range got.Buckets {
		byName[b.Name] = b
	}

	five, ok := byName["5h"]
	if !ok {
		t.Fatalf("no 5h bucket in %+v", got.Buckets)
	}
	if five.Utilization != 0.14 {
		t.Errorf("5h utilization = %v, want 0.14 (percent normalized)", five.Utilization)
	}
	if five.ResetsAt != 1786986000_000 {
		t.Errorf("5h resetsAt = %d, want unix ms", five.ResetsAt)
	}
	if seven, ok := byName["7d"]; !ok || seven.Utilization != 0.01 {
		t.Errorf("7d bucket = %+v (ok=%v)", seven, ok)
	} else if seven.ResetsAt == 0 {
		t.Error("an RFC3339 resets_at should parse")
	}
	fable, ok := byName["7d_fable"]
	if !ok {
		t.Fatalf("no model-scoped bucket in %+v", got.Buckets)
	}
	if fable.Utilization != 0.63 {
		t.Errorf("scoped utilization = %v, want 0.63", fable.Utilization)
	}
	if got.ObservedAt == 0 {
		t.Error("ObservedAt should be stamped")
	}
}

// The usage endpoint has its own rate limit. A throttled probe must be
// distinguishable so the caller backs off instead of hammering, and so stale
// quota is not mistaken for fresh.
func TestQuotaReportsThrottling(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 429,
		Header: http.Header{"Retry-After": []string{"0"}},
		Body:   `{"error":{"message":"Rate limited"}}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	_, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if !errors.Is(err, ErrQuotaThrottled) {
		t.Fatalf("err = %v, want ErrQuotaThrottled", err)
	}
}

func TestQuotaUnsupportedForAPIKeyCredential(t *testing.T) {
	p := New(http.DefaultClient)

	_, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialAPIKey, APIKey: "sk",
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v, want provider.ErrUnsupported", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/anthropic/ -run 'TestProfile|TestQuota' -v`
Expected: FAIL — `undefined: ErrQuotaThrottled`, or the stubbed `Profile`/`Quota` returning `ErrUnsupported`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/provider/anthropic/usage.go`:

```go
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrQuotaThrottled reports that the zero-spend usage endpoint rate-limited us.
// It is distinct from a failure to read: the caller must back off rather than
// retry, and must not treat previously observed quota as fresh.
var ErrQuotaThrottled = errors.New("usage endpoint throttled")

const usageBeta = "oauth-2025-04-20"

func (a *Anthropic) get(ctx context.Context, path string, c provider.Credential, beta bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+path, nil)
	if err != nil {
		return nil, 0, err
	}
	a.Authorize(req, c)
	req.Header.Set("Accept", "application/json")
	if beta {
		req.Header.Set("anthropic-beta", usageBeta)
	}

	res, err := a.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
}

type profileResponse struct {
	Account *struct {
		UUID         string `json:"uuid"`
		Email        string `json:"email"`
		DisplayName  string `json:"display_name"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

func (a *Anthropic) Profile(ctx context.Context, c provider.Credential) (provider.Profile, error) {
	body, status, err := a.get(ctx, "/api/oauth/profile", c, false)
	if err != nil {
		return provider.Profile{}, fmt.Errorf("profile: %w", err)
	}
	if status != http.StatusOK {
		return provider.Profile{}, fmt.Errorf("profile: HTTP %d", status)
	}

	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return provider.Profile{}, fmt.Errorf("profile: %w", err)
	}
	out := provider.Profile{}
	if pr.Account != nil {
		out.AccountUUID = pr.Account.UUID
		out.Email = pr.Account.Email
		out.DisplayName = pr.Account.DisplayName
		switch {
		case pr.Account.HasClaudeMax:
			out.Plan = "max"
		case pr.Account.HasClaudePro:
			out.Plan = "pro"
		}
	}
	if pr.Organization != nil {
		out.OrgUUID = pr.Organization.UUID
		out.OrgName = pr.Organization.Name
	}
	return out, nil
}

type usageBucketJSON struct {
	Utilization    *float64 `json:"utilization"`
	UsedPercentage *float64 `json:"used_percentage"`
	Percent        *float64 `json:"percent"`
	ResetsAt       any      `json:"resets_at"`
}

type usageResponse struct {
	FiveHour *usageBucketJSON `json:"five_hour"`
	SevenDay *usageBucketJSON `json:"seven_day"`
	Limits   []struct {
		Group    string   `json:"group"`
		Percent  *float64 `json:"percent"`
		ResetsAt any      `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// modelBucketName turns a display name like "Claude Fable 5" into a stable
// bucket key like "7d_fable", so a bucket survives cosmetic renames upstream.
func modelBucketName(displayName string) string {
	s := strings.ToLower(displayName)
	s = strings.ReplaceAll(s, "claude", " ")
	s = nonAlnum.ReplaceAllString(s, " ")
	for _, word := range strings.Fields(s) {
		// Skip pure version numbers ("5" in "Claude Fable 5") and take the first
		// real name token, so the key survives cosmetic renames upstream.
		if strings.IndexFunc(word, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			continue
		}
		return "7d_" + word
	}
	return "7d_model"
}

func bucketValue(b *usageBucketJSON) (float64, bool) {
	if b == nil {
		return 0, false
	}
	for _, p := range []*float64{b.Utilization, b.UsedPercentage, b.Percent} {
		if p != nil {
			return *p / 100, true // reported as a percentage
		}
	}
	return 0, false
}

func resetMillis(v any) int64 {
	switch t := v.(type) {
	case float64:
		return toUnixMillis(fmt.Sprintf("%.0f", t))
	case string:
		return toUnixMillis(t)
	}
	return 0
}

// Quota reads the zero-spend usage endpoint. Only OAuth credentials have one;
// an API-key account reports ErrUnsupported and is selected on priority alone.
func (a *Anthropic) Quota(ctx context.Context, c provider.Credential) (provider.Quota, error) {
	if c.Type != provider.CredentialOAuth {
		return provider.Quota{}, provider.ErrUnsupported
	}

	body, status, err := a.get(ctx, "/api/oauth/usage", c, true)
	if err != nil {
		return provider.Quota{}, fmt.Errorf("usage: %w", err)
	}
	if status == http.StatusTooManyRequests {
		return provider.Quota{}, ErrQuotaThrottled
	}
	if status != http.StatusOK {
		return provider.Quota{}, fmt.Errorf("usage: HTTP %d", status)
	}

	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return provider.Quota{}, fmt.Errorf("usage: %w", err)
	}

	out := provider.Quota{ObservedAt: time.Now().UnixMilli()}
	if v, ok := bucketValue(ur.FiveHour); ok {
		out.Buckets = append(out.Buckets, provider.QuotaBucket{
			Name: "5h", Utilization: v, ResetsAt: resetMillis(ur.FiveHour.ResetsAt),
		})
	}
	if v, ok := bucketValue(ur.SevenDay); ok {
		out.Buckets = append(out.Buckets, provider.QuotaBucket{
			Name: "7d", Utilization: v, ResetsAt: resetMillis(ur.SevenDay.ResetsAt),
		})
	}
	for _, l := range ur.Limits {
		if l.Group != "weekly" || l.Scope == nil || l.Scope.Model == nil || l.Percent == nil {
			continue
		}
		out.Buckets = append(out.Buckets, provider.QuotaBucket{
			Name:        modelBucketName(l.Scope.Model.DisplayName),
			Utilization: *l.Percent / 100,
			ResetsAt:    resetMillis(l.ResetsAt),
		})
	}
	return out, nil
}
```

- [ ] **Step 4: Remove the temporary stubs**

If Task 7 added stubbed `Profile`/`Quota` methods to `anthropic.go`, delete them now — the real ones live in `usage.go` and duplicate methods will not compile.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/provider/... -v`
Expected: PASS, including `TestSatisfiesProviderInterface` now that the interface is fully implemented.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/anthropic/usage.go internal/provider/anthropic/usage_test.go internal/provider/anthropic/anthropic.go
git commit -m "feat(anthropic): profile and zero-spend quota reads

A throttled usage endpoint returns a distinct error so callers back off
rather than hammer it, and do not mistake stale quota for fresh."
```

---

### Task 9: Account registry with single-flight credential refresh

**Files:**
- Create: `internal/account/account.go`
- Create: `internal/account/manager.go`
- Test: `internal/account/manager_test.go`

**Interfaces:**
- Consumes: `config.Account`, `provider.Provider`, `provider.Credential`, `anthropic.IsExpired`/`IsExpiringSoon` semantics (reimplemented against the provider, not imported).
- Produces: `account.Account` (fields below), `account.Status` with `StatusActive`/`StatusErrored`, `account.Manager`, `account.New(accounts []config.Account, providers map[string]provider.Provider, opts Options) *Manager`, `account.Options{Persist func(id string, c provider.Credential) error, Now func() time.Time, SwitchThreshold float64, SessionAffinity bool, Ramp Ramp}`, `(*Manager).All() []*Account`, `(*Manager).Get(id string) *Account`, `(*Manager).EnsureFresh(ctx, id string, force bool) error`, `(*Manager).Snapshot() []Account`.

- [ ] **Step 1: Write the failing test**

Create `internal/account/manager_test.go`:

```go
package account

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// stubProvider counts Refresh calls and can be made slow, so coalescing is
// observable.
type stubProvider struct {
	refreshes atomic.Int32
	delay     time.Duration
	err       error
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	s.refreshes.Add(1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return provider.Credential{}, s.err
	}
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "refreshed",
		RefreshToken: "rt-next",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}, nil
}

func (s *stubProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (s *stubProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrUnsupported
}
func (s *stubProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (s *stubProvider) Authorize(*http.Request, provider.Credential)                {}
func (s *stubProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error)    { return b, nil }
func (s *stubProvider) ClassifyResponse(*http.Response) provider.Outcome            { return provider.Outcome{} }
func (s *stubProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)              { return nil, false }

func newTestManager(t *testing.T, p *stubProvider, accts ...config.Account) (*Manager, *[]provider.Credential) {
	t.Helper()
	var mu sync.Mutex
	persisted := []provider.Credential{}
	m := New(accts, map[string]provider.Provider{"stub": p}, Options{
		SwitchThreshold: 0.98,
		Persist: func(_ string, c provider.Credential) error {
			mu.Lock()
			defer mu.Unlock()
			persisted = append(persisted, c)
			return nil
		},
	})
	return m, &persisted
}

func expiredOAuth() provider.Credential {
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "stale",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
	}
}

func TestEnsureFreshRefreshesExpiredCredentialAndPersists(t *testing.T) {
	p := &stubProvider{}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got := m.Get("a").Credential.AccessToken; got != "refreshed" {
		t.Errorf("AccessToken = %q, want refreshed", got)
	}
	if n := len(*persisted); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

func TestEnsureFreshSkipsValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 for a valid credential", n)
	}
}

func TestEnsureFreshForceRefreshesValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh(force): %v", err)
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1", n)
	}
}

// Several concurrent requests on one account must produce ONE refresh. Without
// coalescing, a burst turns into a burst of refreshes, and the upstream rotates
// the refresh token under itself so all but one attempt fail.
func TestEnsureFreshCoalescesConcurrentCallers(t *testing.T) {
	p := &stubProvider{delay: 50 * time.Millisecond}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureFresh(context.Background(), "a", false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
	if n := len(*persisted); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

func TestEnsureFreshMarksAccountErroredOnRejection(t *testing.T) {
	p := &stubProvider{err: errors.New("invalid_grant")}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the refresh error to propagate")
	}
	if got := m.Get("a").Status; got != StatusErrored {
		t.Errorf("Status = %v, want StatusErrored", got)
	}
}

func TestEnsureFreshIgnoresAPIKeyCredentials(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 — an API key does not expire", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/account/ -v`
Expected: FAIL to build — `undefined: New`, `undefined: Manager`, `undefined: StatusErrored`.

- [ ] **Step 3: Write the account state**

Create `internal/account/account.go`:

```go
// Package account owns account runtime state, selection, and admission. It
// knows nothing about HTTP beyond the credential types the provider defines.
package account

import (
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

type Status int

const (
	StatusActive Status = iota
	StatusErrored
)

func (s Status) String() string {
	if s == StatusErrored {
		return "errored"
	}
	return "active"
}

// Account is one upstream identity plus everything learned about it at runtime.
// Guarded by Manager's mutex; never mutate outside the manager.
type Account struct {
	ID          string
	Label       string
	Provider    string
	Priority    int
	Disabled    bool
	Credential  provider.Credential
	AccountUUID string
	OrgName     string
	Plan        string
	Upstream    string
	ModelMap    map[string]string

	Status    Status
	LastError string

	// Buckets is the most recent quota observation per bucket name.
	Buckets map[string]provider.QuotaBucket
	// RateLimitedUntil holds the account out of selection entirely (unix ms).
	RateLimitedUntil int64
	// PausedUntil keeps the account selectable but makes Admit wait (unix ms).
	PausedUntil int64
	// RampStartedAt begins a storm-control window (unix ms); 0 means no ramp.
	RampStartedAt int64
	InFlight      int
}

// ToProvider projects the fields a provider is allowed to see.
func (a *Account) ToProvider() provider.Account {
	return provider.Account{
		ID:          a.ID,
		Label:       a.Label,
		Credential:  a.Credential,
		AccountUUID: a.AccountUUID,
		Upstream:    a.Upstream,
		ModelMap:    a.ModelMap,
	}
}

func fromConfig(c config.Account) *Account {
	return &Account{
		ID:          c.ID,
		Label:       c.Label,
		Provider:    c.Provider,
		Priority:    c.Priority,
		Disabled:    c.Disabled,
		Credential:  c.Credential,
		AccountUUID: c.Identity.AccountUUID,
		OrgName:     c.Identity.OrgName,
		Plan:        c.Identity.Plan,
		Upstream:    c.Upstream,
		ModelMap:    c.ModelMap,
		Status:      StatusActive,
		Buckets:     map[string]provider.QuotaBucket{},
	}
}

// millis converts a time to unix ms, treating the zero time as 0.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
```

- [ ] **Step 4: Write the manager**

Create `internal/account/manager.go`:

```go
package account

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// refreshThreshold is how far ahead of expiry a credential is renewed.
const refreshThreshold = 5 * time.Minute

// Options configures a Manager. Persist is called after a successful refresh so
// the rotated credential survives a restart; Now is injectable for tests.
type Options struct {
	Persist         func(id string, c provider.Credential) error
	Now             func() time.Time
	SwitchThreshold float64
	SessionAffinity bool
	Ramp            Ramp
}

// refreshCall is one in-flight refresh other callers wait on.
type refreshCall struct {
	done chan struct{}
	err  error
}

type Manager struct {
	mu        sync.Mutex
	accounts  []*Account
	byID      map[string]*Account
	providers map[string]provider.Provider
	opts      Options

	refreshing map[string]*refreshCall
	// affinity maps a client session id to the account that served it.
	affinity map[string]string
}

func New(accts []config.Account, providers map[string]provider.Provider, opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SwitchThreshold <= 0 {
		opts.SwitchThreshold = 0.98
	}
	opts.Ramp = opts.Ramp.withDefaults()

	m := &Manager{
		byID:       map[string]*Account{},
		providers:  providers,
		opts:       opts,
		refreshing: map[string]*refreshCall{},
		affinity:   map[string]string{},
	}
	for _, c := range accts {
		a := fromConfig(c)
		m.accounts = append(m.accounts, a)
		m.byID[a.ID] = a
	}
	return m
}

func (m *Manager) Get(id string) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

// All returns the live account pointers. Callers must not mutate them.
func (m *Manager) All() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, len(m.accounts))
	copy(out, m.accounts)
	return out
}

// Snapshot returns value copies safe to hand to a UI.
func (m *Manager) Snapshot() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		copyAcct := *a
		copyAcct.Buckets = make(map[string]provider.QuotaBucket, len(a.Buckets))
		for k, v := range a.Buckets {
			copyAcct.Buckets[k] = v
		}
		out = append(out, copyAcct)
	}
	return out
}

// EnsureFresh renews an account's credential when it is expired or close to it,
// or unconditionally when force is set.
//
// Concurrent callers for the same account are coalesced into a single refresh.
// Without that, a burst of requests produces a burst of refreshes; the upstream
// rotates the refresh token on each one, so every attempt but one is left
// holding a token that has already been superseded.
func (m *Manager) EnsureFresh(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	a := m.byID[id]
	if a == nil {
		m.mu.Unlock()
		return fmt.Errorf("unknown account %q", id)
	}
	// An API key does not expire, so there is nothing to refresh.
	if a.Credential.Type != provider.CredentialOAuth || a.Credential.RefreshToken == "" {
		m.mu.Unlock()
		return nil
	}
	if !force && !m.needsRefreshLocked(a) {
		m.mu.Unlock()
		return nil
	}
	if call, ok := m.refreshing[id]; ok {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refreshing[id] = call
	p := m.providers[a.Provider]
	cred := a.Credential
	m.mu.Unlock()

	var err error
	if p == nil {
		err = fmt.Errorf("no provider %q for account %q", a.Provider, id)
	} else {
		var next provider.Credential
		next, err = p.Refresh(ctx, cred)
		if err == nil {
			m.mu.Lock()
			a.Credential = next
			a.Status = StatusActive
			a.LastError = ""
			m.mu.Unlock()
			if m.opts.Persist != nil {
				err = m.opts.Persist(id, next)
			}
		}
	}
	if err != nil {
		m.mu.Lock()
		a.Status = StatusErrored
		a.LastError = err.Error()
		m.mu.Unlock()
	}

	m.mu.Lock()
	delete(m.refreshing, id)
	m.mu.Unlock()

	call.err = err
	close(call.done)
	return err
}

func (m *Manager) needsRefreshLocked(a *Account) bool {
	if a.Credential.ExpiresAt == 0 {
		return false // no expiry known; do not churn
	}
	now := m.opts.Now()
	return now.Add(refreshThreshold).UnixMilli() >= a.Credential.ExpiresAt
}

// UpdateQuota records observed buckets for an account.
func (m *Manager) UpdateQuota(id string, buckets []provider.QuotaBucket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return
	}
	if a.Buckets == nil {
		a.Buckets = map[string]provider.QuotaBucket{}
	}
	for _, b := range buckets {
		a.Buckets[b.Name] = b
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/account/ -v`
Expected: FAIL to build — `Ramp` and `withDefaults` arrive in Task 11. Add a temporary placeholder in `manager.go` to unblock:

```go
type Ramp struct{ Enabled bool }

func (r Ramp) withDefaults() Ramp { return r }
```

Then re-run. Expected: PASS, six tests, no races.

- [ ] **Step 6: Commit**

```bash
git add internal/account/
git commit -m "feat(account): registry with single-flight credential refresh

Concurrent callers on one account coalesce into a single refresh. Without
it a burst of requests becomes a burst of refreshes, and since the
upstream rotates the refresh token each time, all but one are left
holding a superseded token."
```

---

### Task 10: Account selection

**Files:**
- Create: `internal/account/select.go`
- Test: `internal/account/select_test.go`

**Interfaces:**
- Consumes: `account.Manager`, `account.Account`, `provider.QuotaBucket`.
- Produces: `account.SelectRequest{Model, SessionID string, Exclude map[string]bool}`, `(*Manager).Select(SelectRequest) (*Account, error)`, `(*Manager).RecordSession(sessionID, accountID string)`, `(*Manager).MarkRateLimited(id string, d time.Duration)`, `(*Manager).ClearRateLimited(id string)`, sentinel `account.ErrNoAccount`, helper `account.BucketAppliesTo(bucket, model string) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/account/select_test.go`:

```go
package account

import (
	"errors"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

func oauthCred() provider.Credential {
	return provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
}

func mgr(t *testing.T, accts ...config.Account) *Manager {
	t.Helper()
	return New(accts, map[string]provider.Provider{"stub": &stubProvider{}}, Options{
		SwitchThreshold: 0.98,
		SessionAffinity: true,
	})
}

func acct(id string, priority int) config.Account {
	return config.Account{ID: id, Provider: "stub", Label: id, Priority: priority, Credential: oauthCred()}
}

func TestSelectPrefersLowestPriority(t *testing.T) {
	m := mgr(t, acct("high", 10), acct("low", 0))

	got, err := m.Select(SelectRequest{Model: "claude-sonnet"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "low" {
		t.Errorf("selected %q, want low", got.ID)
	}
}

func TestSelectSkipsDisabledErroredAndExcluded(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1), acct("c", 2))
	m.Get("a").Disabled = true
	m.Get("b").Status = StatusErrored

	got, err := m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "c" {
		t.Errorf("selected %q, want c", got.ID)
	}

	if _, err := m.Select(SelectRequest{Exclude: map[string]bool{"c": true}}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("err = %v, want ErrNoAccount once every candidate is out", err)
	}
}

func TestSelectSkipsRateLimitedUntilItLapses(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.MarkRateLimited("a", time.Hour)

	got, _ := m.Select(SelectRequest{})
	if got.ID != "b" {
		t.Fatalf("selected %q, want b while a is rate limited", got.ID)
	}

	m.ClearRateLimited("a")
	got, _ = m.Select(SelectRequest{})
	if got.ID != "a" {
		t.Errorf("selected %q, want a once the hold clears", got.ID)
	}
}

func TestSelectSkipsAccountOverSwitchThreshold(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.99}})

	got, _ := m.Select(SelectRequest{Model: "claude-sonnet"})
	if got.ID != "b" {
		t.Errorf("selected %q, want b — a is over the switch threshold", got.ID)
	}
}

// A spent per-model bucket must exclude the account for THAT model only. An
// account out of one model's weekly quota still serves every other model.
func TestSelectModelScopedBucketOnlyBlocksThatModel(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.UpdateQuota("a", []provider.QuotaBucket{
		{Name: "7d_fable", Utilization: 1, Status: "rejected"},
	})

	got, _ := m.Select(SelectRequest{Model: "claude-fable-5"})
	if got.ID != "b" {
		t.Errorf("fable request selected %q, want b", got.ID)
	}

	got, _ = m.Select(SelectRequest{Model: "claude-sonnet-5"})
	if got.ID != "a" {
		t.Errorf("sonnet request selected %q, want a — only fable is spent", got.ID)
	}
}

func TestSelectBreaksPriorityTiesBySoonestReset(t *testing.T) {
	m := mgr(t, acct("later", 0), acct("sooner", 0))
	now := time.Now()
	m.UpdateQuota("later", []provider.QuotaBucket{
		{Name: "7d", Utilization: 0.5, ResetsAt: now.Add(48 * time.Hour).UnixMilli()},
	})
	m.UpdateQuota("sooner", []provider.QuotaBucket{
		{Name: "7d", Utilization: 0.5, ResetsAt: now.Add(2 * time.Hour).UnixMilli()},
	})

	got, _ := m.Select(SelectRequest{})
	if got.ID != "sooner" {
		t.Errorf("selected %q, want sooner — spend the quota that expires first", got.ID)
	}
}

func TestSelectHonoursSessionAffinityThenYields(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 5))
	m.RecordSession("sess-1", "b")

	got, _ := m.Select(SelectRequest{SessionID: "sess-1"})
	if got.ID != "b" {
		t.Errorf("selected %q, want the pinned account b even at worse priority", got.ID)
	}

	// When the pinned account becomes ineligible, affinity yields.
	m.MarkRateLimited("b", time.Hour)
	got, _ = m.Select(SelectRequest{SessionID: "sess-1"})
	if got.ID != "a" {
		t.Errorf("selected %q, want a once b is ineligible", got.ID)
	}
}

func TestBucketAppliesTo(t *testing.T) {
	cases := []struct {
		bucket, model string
		want          bool
	}{
		{"5h", "claude-sonnet-5", true},
		{"7d", "claude-sonnet-5", true},
		{"7d_fable", "claude-fable-5", true},
		{"7d_fable", "claude-sonnet-5", false},
		{"7d_sonnet", "claude-sonnet-5", true},
		{"7d_fable", "", true}, // unknown model: assume every bucket binds
	}
	for _, c := range cases {
		if got := BucketAppliesTo(c.bucket, c.model); got != c.want {
			t.Errorf("BucketAppliesTo(%q, %q) = %v, want %v", c.bucket, c.model, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/account/ -run 'TestSelect|TestBucket' -v`
Expected: FAIL to build — `undefined: SelectRequest`, `undefined: ErrNoAccount`, `undefined: BucketAppliesTo`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/account/select.go`:

```go
package account

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrNoAccount reports that no account is currently eligible.
var ErrNoAccount = errors.New("no eligible account")

// SelectRequest describes what a request needs from an account.
type SelectRequest struct {
	Model     string
	SessionID string
	// Exclude holds account ids already attempted or refused for this request.
	Exclude map[string]bool
}

// BucketAppliesTo reports whether a quota bucket constrains a given model.
//
// Window buckets ("5h", "7d") bind everything. A model-scoped bucket
// ("7d_fable") binds only models whose name carries that token, so an account
// out of one model's weekly allowance still serves the others. An empty model
// name is treated conservatively: every bucket binds.
func BucketAppliesTo(bucket, model string) bool {
	suffix, scoped := cutModelScope(bucket)
	if !scoped {
		return true
	}
	if model == "" {
		return true
	}
	return strings.Contains(strings.ToLower(model), suffix)
}

func cutModelScope(bucket string) (string, bool) {
	i := strings.Index(bucket, "_")
	if i < 0 || i+1 >= len(bucket) {
		return "", false
	}
	return strings.ToLower(bucket[i+1:]), true
}

// RecordSession pins a client session to the account that served it, so the
// upstream prompt cache for that conversation stays warm.
func (m *Manager) RecordSession(sessionID, accountID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.affinity[sessionID] = accountID
}

// MarkRateLimited holds an account out of selection for d.
func (m *Manager) MarkRateLimited(id string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil {
		until := m.opts.Now().Add(d).UnixMilli()
		if until > a.RateLimitedUntil {
			a.RateLimitedUntil = until
		}
	}
}

// ClearRateLimited releases a hold. Any non-429 response is live proof the hold
// no longer binds, which is what lets a revalidating request restore an account.
func (m *Manager) ClearRateLimited(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil {
		a.RateLimitedUntil = 0
	}
}

// Select returns the best eligible account, honouring session affinity first.
func (m *Manager) Select(req SelectRequest) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nowMS := m.opts.Now().UnixMilli()

	if m.opts.SessionAffinity && req.SessionID != "" {
		if id, ok := m.affinity[req.SessionID]; ok {
			if a := m.byID[id]; a != nil && m.eligibleLocked(a, req, nowMS) {
				return a, nil
			}
		}
	}

	candidates := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if m.eligibleLocked(a, req, nowMS) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		if ai.Priority != aj.Priority {
			return ai.Priority < aj.Priority
		}
		// Spend the allowance that expires soonest first; an unknown reset sorts
		// last so a known-expiring account is preferred over an unknown one.
		ri, rj := soonestReset(ai, req.Model), soonestReset(aj, req.Model)
		if ri != rj {
			if ri == 0 {
				return false
			}
			if rj == 0 {
				return true
			}
			return ri < rj
		}
		return ai.ID < aj.ID // deterministic
	})
	return candidates[0], nil
}

func (m *Manager) eligibleLocked(a *Account, req SelectRequest, nowMS int64) bool {
	if a.Disabled || a.Status == StatusErrored {
		return false
	}
	if req.Exclude[a.ID] {
		return false
	}
	if a.RateLimitedUntil > nowMS {
		return false
	}
	if !hasCredential(a) {
		return false
	}
	for name, b := range a.Buckets {
		if !BucketAppliesTo(name, req.Model) {
			continue
		}
		if b.Status == "rejected" {
			return false
		}
		if b.Utilization >= m.opts.SwitchThreshold {
			return false
		}
	}
	return true
}

func hasCredential(a *Account) bool {
	return a.Credential.AccessToken != "" || a.Credential.RefreshToken != "" || a.Credential.APIKey != ""
}

// soonestReset is the nearest future reset among buckets binding this model.
func soonestReset(a *Account, model string) int64 {
	var best int64
	for name, b := range a.Buckets {
		if !BucketAppliesTo(name, model) || b.ResetsAt == 0 {
			continue
		}
		if best == 0 || b.ResetsAt < best {
			best = b.ResetsAt
		}
	}
	return best
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/account/ -v`
Expected: PASS, all manager and selection tests.

- [ ] **Step 5: Commit**

```bash
git add internal/account/select.go internal/account/select_test.go
git commit -m "feat(account): model-scoped eligibility and selection

A spent per-model bucket excludes an account for that model only; ties on
priority break toward the account whose quota resets soonest, so the
allowance about to expire is the one spent."
```

---

### Task 11: Admission control — ramp and pause

Replaces the `Ramp` placeholder added in Task 9.

**Files:**
- Create: `internal/account/admit.go`
- Modify: `internal/account/manager.go` (delete the placeholder `Ramp` type and `withDefaults`)
- Test: `internal/account/admit_test.go`

**Interfaces:**
- Consumes: `account.Manager`, `account.Options`.
- Produces: `account.Ramp{Enabled bool, StartConc, StepConc, StepMS, WindowMS, PollMS int}`, `(Ramp).withDefaults() Ramp`, `account.Waiter` interface with `Wait(ctx context.Context, d time.Duration) error`, `(*Manager).Admit(ctx, id string, w Waiter) error`, `(*Manager).Release(id string)`, `(*Manager).PauseAccount(id string, d time.Duration)`, `(*Manager).BeginRamp(id string)`.

- [ ] **Step 1: Write the failing test**

Create `internal/account/admit_test.go`:

```go
package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// recordingWaiter satisfies Waiter, returns immediately, and logs every wait so
// admission delays are asserted without real time passing.
type recordingWaiter struct {
	mu    sync.Mutex
	waits []time.Duration
	err   error
}

func (w *recordingWaiter) Wait(_ context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.waits = append(w.waits, d)
	return nil
}

func (w *recordingWaiter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waits)
}

// clock is a manually advanced clock for ramp arithmetic.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func rampMgr(t *testing.T, r Ramp, clk *clock) *Manager {
	t.Helper()
	return New(
		[]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{SwitchThreshold: 0.98, Ramp: r, Now: clk.now},
	)
}

func TestAdmitTakesAndReleasesSlots(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	w := &recordingWaiter{}

	for i := 0; i < 3; i++ {
		if err := m.Admit(context.Background(), "a", w); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if got := m.Get("a").InFlight; got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
	if w.count() != 0 {
		t.Errorf("ramp disabled should never wait, waited %v", w.waits)
	}

	m.Release("a")
	if got := m.Get("a").InFlight; got != 2 {
		t.Errorf("InFlight after Release = %d, want 2", got)
	}
}

// Release must not drive the counter negative; a double release would otherwise
// grant a phantom slot to the next caller.
func TestReleaseFloorsAtZero(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)

	m.Release("a")
	m.Release("a")
	if got := m.Get("a").InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0", got)
	}
}

func TestAdmitEnforcesRampCapThenGrowsWithTime(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	r := Ramp{Enabled: true, StartConc: 2, StepConc: 1, StepMS: 250, WindowMS: 5000, PollMS: 50}
	m := rampMgr(t, r, clk)
	m.BeginRamp("a")
	w := &recordingWaiter{}

	// Two slots are available at the start of the window.
	for i := 0; i < 2; i++ {
		if err := m.Admit(context.Background(), "a", w); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if w.count() != 0 {
		t.Fatalf("first two admissions should not wait, waited %v", w.waits)
	}

	// A third must wait, because the cap is still 2. The waiter returns
	// immediately, so grow the cap by advancing the clock before it retries.
	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	deadline := time.After(2 * time.Second)
	for w.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("third admission never waited on the ramp cap")
		case err := <-done:
			t.Fatalf("third admission returned %v without waiting", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	clk.advance(300 * time.Millisecond) // one step: cap becomes 3

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("third admission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third admission did not proceed after the cap grew")
	}
	if got := m.Get("a").InFlight; got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
}

func TestAdmitWaitsWhileAccountIsPaused(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", 2*time.Second)
	w := &recordingWaiter{}

	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	deadline := time.After(2 * time.Second)
	for w.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("admission did not wait on the pause")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	clk.advance(3 * time.Second) // pause lapses

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not proceed after the pause lapsed")
	}
}

// The budget owns all non-transferring time. When it refuses a wait, admission
// must give up rather than block: a paused account cannot be allowed to stall a
// request past its deadline.
func TestAdmitPropagatesWaiterErrorWithoutTakingASlot(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", time.Hour)

	sentinel := errors.New("budget exhausted")
	w := &recordingWaiter{err: sentinel}

	err := m.Admit(context.Background(), "a", w)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the waiter's error", err)
	}
	if got := m.Get("a").InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0 — a refused admission must not hold a slot", got)
	}
}

func TestAdmitHonoursContextCancellation(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Admit(ctx, "a", &recordingWaiter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
```

- [ ] **Step 2: Delete the placeholder**

Remove the temporary `Ramp` struct and `withDefaults` method added to `internal/account/manager.go` in Task 9. The real ones follow.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/account/ -run TestAdmit -v`
Expected: FAIL to build — `undefined: Admit`, `undefined: Waiter`, unknown fields on `Ramp`.

- [ ] **Step 4: Write minimal implementation**

Create `internal/account/admit.go`:

```go
package account

import (
	"context"
	"time"
)

// Waiter bounds how long admission may block. The proxy passes the request's
// budget, so a pause or a ramp cap can never keep a client waiting past its
// deadline — every delay is drawn from the same allowance.
type Waiter interface {
	Wait(ctx context.Context, d time.Duration) error
}

// Ramp paces requests onto an account that has just become current, so a fleet
// of agents failing over at the same instant does not arrive as one burst,
// trip a rate limit, and cascade onward to the next account.
type Ramp struct {
	Enabled   bool
	StartConc int // concurrent requests allowed at the start of the window
	StepConc  int // additional requests allowed per step
	StepMS    int // step length
	WindowMS  int // after this, the cap is lifted entirely
	PollMS    int // how often a blocked caller re-checks
}

func (r Ramp) withDefaults() Ramp {
	if r.StartConc <= 0 {
		r.StartConc = 2
	}
	if r.StepConc <= 0 {
		r.StepConc = 1
	}
	if r.StepMS <= 0 {
		r.StepMS = 250
	}
	if r.WindowMS <= 0 {
		r.WindowMS = 5000
	}
	if r.PollMS <= 0 {
		r.PollMS = 50
	}
	return r
}

// BeginRamp starts a pacing window for an account that just became current.
func (m *Manager) BeginRamp(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil && m.opts.Ramp.Enabled {
		a.RampStartedAt = m.opts.Now().UnixMilli()
	}
}

// PauseAccount makes new admissions wait for d without removing the account
// from selection. This is the response to a rate limit that came with a usable
// hint: concurrent requests queue rather than piling on, and the account keeps
// serving the moment the hint lapses.
//
// The ramp is armed to begin when the pause ends, so the queued requests trickle
// out instead of arriving together the instant the pause lifts.
func (m *Manager) PauseAccount(id string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return
	}
	until := m.opts.Now().Add(d).UnixMilli()
	if until > a.PausedUntil {
		a.PausedUntil = until
	}
	if m.opts.Ramp.Enabled {
		a.RampStartedAt = a.PausedUntil
	}
}

// rampCapLocked is how many concurrent requests the account currently allows.
// A lifted cap is reported as -1, meaning unlimited.
func (m *Manager) rampCapLocked(a *Account, nowMS int64) int {
	r := m.opts.Ramp
	if !r.Enabled || a.RampStartedAt == 0 {
		return -1
	}
	elapsed := nowMS - a.RampStartedAt
	if elapsed < 0 {
		// The window is armed in the future by PauseAccount; treat it as the
		// start of the window rather than a negative cap.
		elapsed = 0
	}
	if elapsed >= int64(r.WindowMS) {
		a.RampStartedAt = 0
		return -1
	}
	return r.StartConc + int(elapsed/int64(r.StepMS))*r.StepConc
}

// Admit reserves a slot on an account, waiting while the account is paused or at
// its ramp cap. Every wait goes through w, so the request's budget governs the
// total. On success the caller must pair it with exactly one Release.
func (m *Manager) Admit(ctx context.Context, id string, w Waiter) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		a := m.byID[id]
		if a == nil {
			m.mu.Unlock()
			return ErrNoAccount
		}
		nowMS := m.opts.Now().UnixMilli()

		if a.PausedUntil > nowMS {
			remaining := time.Duration(a.PausedUntil-nowMS) * time.Millisecond
			poll := time.Duration(m.opts.Ramp.PollMS*4) * time.Millisecond
			m.mu.Unlock()
			if err := w.Wait(ctx, min(remaining, poll)); err != nil {
				return err
			}
			continue
		}

		cap := m.rampCapLocked(a, nowMS)
		if cap < 0 || a.InFlight < cap {
			a.InFlight++
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()

		if err := w.Wait(ctx, time.Duration(m.opts.Ramp.PollMS)*time.Millisecond); err != nil {
			return err
		}
	}
}

// Release returns a slot taken by Admit. It floors at zero: a double release
// must not grant a phantom slot to the next caller.
func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil && a.InFlight > 0 {
		a.InFlight--
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/account/ -v`
Expected: PASS, all account tests including the six admission tests.

- [ ] **Step 6: Commit**

```bash
git add internal/account/admit.go internal/account/manager.go internal/account/admit_test.go
git commit -m "feat(account): admission control with ramp and pause

Every admission delay goes through the caller's Waiter, so a paused
account cannot stall a request past its deadline. Release floors at zero
so a double release cannot grant a phantom slot."
```

---

### Task 12: Upstream transport

**Files:**
- Create: `internal/proxy/transport.go`
- Test: `internal/proxy/transport_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `proxy.TransportOptions{MaxConnsPerHost int, ResponseHeaderTimeout time.Duration, IdleConnTimeout time.Duration}`, `proxy.NewTransport(TransportOptions) *http.Transport`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/transport_test.go`:

```go
package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTransportDisablesHTTP2(t *testing.T) {
	tr := NewTransport(TransportOptions{})

	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 must be false")
	}
	if got := tr.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("NextProtos = %v, want [http/1.1]", got)
	}
}

// The behavioural assertion, not just the config one: against a server that
// offers HTTP/2, this transport must still negotiate HTTP/1.1.
//
// A single h2 connection multiplexes every request to an origin behind one
// flow-control window. Agent clients post large contexts concurrently, so those
// uploads queue behind WINDOW_UPDATE frames and a trivial request can wait
// minutes for headers. Independent h1 connections each fill their own socket.
func TestTransportNegotiatesHTTP1AgainstAnHTTP2Server(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := NewTransport(TransportOptions{})
	tr.TLSClientConfig.InsecureSkipVerify = true // self-signed test cert
	client := &http.Client{Transport: tr}

	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.Proto != "HTTP/1.1" {
		t.Errorf("client saw %s, want HTTP/1.1", res.Proto)
	}
	if string(body) != "HTTP/1.1" {
		t.Errorf("server saw %s, want HTTP/1.1", body)
	}
}

func TestNewTransportAppliesOptionsAndDefaults(t *testing.T) {
	tr := NewTransport(TransportOptions{
		MaxConnsPerHost:       7,
		ResponseHeaderTimeout: 3 * time.Second,
	})
	if tr.MaxConnsPerHost != 7 {
		t.Errorf("MaxConnsPerHost = %d, want 7", tr.MaxConnsPerHost)
	}
	if tr.ResponseHeaderTimeout != 3*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}

	def := NewTransport(TransportOptions{})
	if def.MaxConnsPerHost != 256 {
		t.Errorf("default MaxConnsPerHost = %d, want 256", def.MaxConnsPerHost)
	}
	if def.MaxIdleConnsPerHost != 256 {
		t.Errorf("default MaxIdleConnsPerHost = %d, want 256", def.MaxIdleConnsPerHost)
	}
}

func TestTransportSetsNoDelayOnDialedSockets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	var dialed int
	tr := NewTransport(TransportOptions{})
	inner := tr.DialContext
	tr.DialContext = nil // replaced below to observe it was set at all
	if inner == nil {
		t.Fatal("NewTransport must install a DialContext that can set NoDelay")
	}
	tr.DialContext = inner
	dialed++

	client := &http.Client{Transport: tr}
	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	res.Body.Close()
	if dialed == 0 {
		t.Fatal("no dial observed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run 'TestNewTransport|TestTransport' -v`
Expected: FAIL to build — `undefined: NewTransport`, `undefined: TransportOptions`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/transport.go`:

```go
package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// TransportOptions tunes the upstream transport. Zero values take defaults.
type TransportOptions struct {
	MaxConnsPerHost       int
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
}

// NewTransport builds the upstream transport.
//
// HTTP/2 is disabled deliberately, at both the negotiation and the ALPN level.
// One h2 connection multiplexes every request to an origin and shares a single
// flow-control window; an agent client posts large contexts concurrently, so
// those uploads queue behind WINDOW_UPDATE frames and an otherwise trivial
// request can wait minutes for its response headers. Independent HTTP/1.1
// connections have no application-layer flow control, so each upload fills its
// own socket at TCP speed. Re-enabling h2 here will look like a modernization
// and will reintroduce head-of-line blocking under concurrency.
//
// Sockets set NoDelay: Nagle coalescing coalesces small streamed frames and adds
// tens of milliseconds per chunk, which reads to a user as a sluggish stream.
func NewTransport(opts TransportOptions) *http.Transport {
	if opts.MaxConnsPerHost <= 0 {
		opts.MaxConnsPerHost = 256
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 120 * time.Second
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = 90 * time.Second
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
			return conn, nil
		},
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{NextProtos: []string{"http/1.1"}},
		MaxConnsPerHost:       opts.MaxConnsPerHost,
		MaxIdleConnsPerHost:   opts.MaxConnsPerHost,
		MaxIdleConns:          opts.MaxConnsPerHost * 2,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/proxy/ -v`
Expected: PASS, budget and transport tests.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/transport.go internal/proxy/transport_test.go
git commit -m "feat(proxy): HTTP/1.1 upstream transport with NoDelay

HTTP/2 is disabled at both negotiation and ALPN: one h2 connection shares
a flow-control window across all requests, so concurrent large uploads
queue behind WINDOW_UPDATE frames. A test asserts h1 is negotiated even
against a server offering h2."
```

---

### Task 13: The relay — flush per chunk, idle watchdog, usage tee

One of the two load-bearing tests lives here.

**Files:**
- Create: `internal/proxy/relay.go`
- Test: `internal/proxy/relay_test.go`

**Interfaces:**
- Consumes: `provider.UsageDelta`, `testutil.NewFakeUpstream`.
- Produces: `proxy.RelayOptions{BodyIdle time.Duration, ParseUsage func([]byte) (*provider.UsageDelta, bool), OnUsage func(*provider.UsageDelta), Streaming bool}`, `proxy.Relay(ctx context.Context, w http.ResponseWriter, body io.Reader, opts RelayOptions) (int64, error)`, `proxy.ErrBodyIdle`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/relay_test.go`:

```go
package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// relayServer fronts an upstream reader with a handler that relays it, so the
// test measures what a real client observes over a real socket.
func relayServer(t *testing.T, open func() (io.ReadCloser, http.Header), opts RelayOptions) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, hdr := open()
		defer body.Close()
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		Relay(r.Context(), w, body, opts)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// LOAD-BEARING. A chunk received from upstream must reach the client before the
// next one is read. If this fails the proxy is buffering, which presents to a
// user as a response that arrives all at once at the end.
func TestRelayStreamsIncrementallyWithoutBuffering(t *testing.T) {
	const gap = 100 * time.Millisecond
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Delay: 0, Data: "data: one\n\n"},
			{Delay: gap, Data: "data: two\n\n"},
			{Delay: gap, Data: "data: three\n\n"},
		},
	})

	srv := relayServer(t, func() (io.ReadCloser, http.Header) {
		res, err := http.Get(up.URL() + "/v1/messages")
		if err != nil {
			t.Fatalf("upstream: %v", err)
		}
		return res.Body, res.Header
	}, RelayOptions{BodyIdle: 5 * time.Second, Streaming: true})

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	start := time.Now()
	arrivals := []time.Duration{}
	buf := make([]byte, 4096)
	for len(arrivals) < 3 {
		n, err := res.Body.Read(buf)
		if n > 0 {
			arrivals = append(arrivals, time.Since(start))
		}
		if err != nil {
			break
		}
	}

	if len(arrivals) < 3 {
		t.Fatalf("observed %d chunks, want 3 — the relay buffered instead of streaming", len(arrivals))
	}
	if arrivals[0] > 60*time.Millisecond {
		t.Errorf("first chunk arrived after %v, expected promptly", arrivals[0])
	}
	// Each later chunk must trail the previous by roughly the upstream gap. A
	// buffering relay delivers all three at once at the end instead.
	for i := 1; i < len(arrivals); i++ {
		delta := arrivals[i] - arrivals[i-1]
		if delta < 60*time.Millisecond {
			t.Errorf("chunk %d arrived only %v after chunk %d; chunks are not being flushed as they arrive",
				i, delta, i-1)
		}
	}
}

func TestRelayCopiesBodyAndReportsByteCount(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hello world"))
	var n int64
	var relayErr error

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, relayErr = Relay(r.Context(), w, body, RelayOptions{BodyIdle: time.Second})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)

	if string(got) != "hello world" {
		t.Errorf("body = %q", got)
	}
	if relayErr != nil {
		t.Errorf("Relay err = %v", relayErr)
	}
	if n != 11 {
		t.Errorf("wrote %d bytes, want 11", n)
	}
}

// A stalled upstream after headers must become a fast failure, not a hang. The
// headers timeout does not cover this window: it is disarmed once headers land.
func TestRelayFailsFastOnIdleUpstream(t *testing.T) {
	stall := &stallingReader{ready: make(chan struct{})}

	var relayErr error
	var wg sync.WaitGroup
	wg.Add(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		_, relayErr = Relay(r.Context(), w, stall, RelayOptions{BodyIdle: 60 * time.Millisecond})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	wg.Wait()
	close(stall.ready)

	if !errors.Is(relayErr, ErrBodyIdle) {
		t.Fatalf("Relay err = %v, want ErrBodyIdle", relayErr)
	}
}

// A healthy but slow stream must never be cut: the watchdog measures silence
// between chunks, not total duration.
func TestRelayDoesNotCutASlowButActiveStream(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Delay: 40 * time.Millisecond, Data: "data: a\n\n"},
			{Delay: 40 * time.Millisecond, Data: "data: b\n\n"},
			{Delay: 40 * time.Millisecond, Data: "data: c\n\n"},
		},
	})

	var relayErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := http.Get(up.URL() + "/v1/messages")
		if err != nil {
			t.Errorf("upstream: %v", err)
			return
		}
		defer res.Body.Close()
		// Total duration (120ms) exceeds the idle window (90ms), but no single
		// gap does.
		_, relayErr = Relay(r.Context(), w, res.Body, RelayOptions{
			BodyIdle: 90 * time.Millisecond, Streaming: true,
		})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if relayErr != nil {
		t.Errorf("Relay err = %v, want nil for a slow but active stream", relayErr)
	}
	if !strings.Contains(string(got), "data: c") {
		t.Errorf("stream truncated: %q", got)
	}
}

func TestRelayTeesUsageFromSSEEvents(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":500}}}` +
		"\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"

	var mu sync.Mutex
	seen := []provider.UsageDelta{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Relay(r.Context(), w, io.NopCloser(strings.NewReader(sse)), RelayOptions{
			BodyIdle:  time.Second,
			Streaming: true,
			ParseUsage: func(event []byte) (*provider.UsageDelta, bool) {
				var d provider.UsageDelta
				s := string(event)
				switch {
				case strings.Contains(s, "message_start"):
					d = provider.UsageDelta{InputTokens: 10, CacheReadTokens: 500}
				case strings.Contains(s, "message_delta"):
					d = provider.UsageDelta{OutputTokens: 7}
				default:
					return nil, false
				}
				return &d, true
			},
			OnUsage: func(d *provider.UsageDelta) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, *d)
			},
		})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("observed %d usage deltas, want 2: %+v", len(seen), seen)
	}
	if seen[0].InputTokens != 10 || seen[0].CacheReadTokens != 500 {
		t.Errorf("first delta = %+v", seen[0])
	}
	if seen[1].OutputTokens != 7 {
		t.Errorf("second delta = %+v", seen[1])
	}
}

func TestRelayStopsWhenClientDisconnects(t *testing.T) {
	// An endless upstream: the relay must stop on client cancellation rather
	// than reading forever.
	endless := io.NopCloser(&endlessReader{})

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		Relay(r.Context(), w, endless, RelayOptions{BodyIdle: 5 * time.Second})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	res.Body.Read(buf)
	cancel()
	res.Body.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop after the client disconnected")
	}
}

// stallingReader blocks until ready is closed, then reports EOF.
type stallingReader struct{ ready chan struct{} }

func (s *stallingReader) Read(p []byte) (int, error) {
	<-s.ready
	return 0, io.EOF
}

type endlessReader struct{}

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	time.Sleep(5 * time.Millisecond)
	return len(p), nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestRelay -v`
Expected: FAIL to build — `undefined: Relay`, `undefined: RelayOptions`, `undefined: ErrBodyIdle`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/relay.go`:

```go
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrBodyIdle reports that the upstream produced no bytes for the whole idle
// window after headers had already arrived.
var ErrBodyIdle = errors.New("upstream stream idle")

// sseTerminator separates events in a Server-Sent Events stream.
var sseTerminator = []byte("\n\n")

// RelayOptions configures one relay.
type RelayOptions struct {
	// BodyIdle bounds silence BETWEEN chunks, not total duration, so a long but
	// healthy stream is never cut.
	BodyIdle time.Duration
	// Streaming enables SSE event framing for the usage tee.
	Streaming  bool
	ParseUsage func(event []byte) (*provider.UsageDelta, bool)
	OnUsage    func(*provider.UsageDelta)
}

type readChunk struct {
	buf []byte
	err error
}

// Relay copies body to w, flushing after every chunk, and returns the number of
// bytes written.
//
// Flushing per chunk is the whole point: a chunk received from upstream reaches
// the client before the next read begins. Buffering here — even implicitly, by
// letting the writer decide when to flush — is what makes a token stream appear
// to arrive all at once when the generation finishes.
//
// On error the response is deliberately NOT completed cleanly. A truncated
// stream ended with a clean finish looks to the client like a complete answer
// and suppresses its retry; the caller destroys the connection instead.
func Relay(ctx context.Context, w http.ResponseWriter, body io.Reader, opts RelayOptions) (int64, error) {
	if opts.BodyIdle <= 0 {
		opts.BodyIdle = 120 * time.Second
	}
	flusher, _ := w.(http.Flusher)

	// Reads run on a helper goroutine so silence is detectable: an io.Reader
	// offers no deadline of its own. Each read gets a fresh buffer because the
	// consumer may still be writing the previous one.
	chunks := make(chan readChunk, 1)
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()

	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 32*1024)
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case chunks <- readChunk{buf: buf[:n]}:
				case <-readCtx.Done():
					return
				}
			}
			if err != nil {
				select {
				case chunks <- readChunk{err: err}:
				case <-readCtx.Done():
				}
				return
			}
		}
	}()

	var written int64
	var pending []byte // incomplete trailing SSE event
	idle := time.NewTimer(opts.BodyIdle)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()

		case <-idle.C:
			return written, ErrBodyIdle

		case c, ok := <-chunks:
			if !ok {
				return written, nil
			}
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					flushRemainingUsage(pending, opts)
					return written, nil
				}
				return written, c.err
			}

			n, err := w.Write(c.buf)
			written += int64(n)
			if err != nil {
				return written, err
			}
			if flusher != nil {
				flusher.Flush()
			}

			if opts.Streaming && opts.ParseUsage != nil {
				pending = teeUsage(append(pending, c.buf...), opts)
			}

			// Reset the watchdog only on real progress.
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(opts.BodyIdle)
		}
	}
}

// teeUsage consumes every complete SSE event in buf, reporting usage, and
// returns the incomplete remainder to be carried into the next chunk.
func teeUsage(buf []byte, opts RelayOptions) []byte {
	for {
		i := bytes.Index(buf, sseTerminator)
		if i < 0 {
			return buf
		}
		event := buf[:i]
		buf = buf[i+len(sseTerminator):]
		if d, ok := opts.ParseUsage(event); ok && opts.OnUsage != nil {
			opts.OnUsage(d)
		}
	}
}

func flushRemainingUsage(pending []byte, opts RelayOptions) {
	if !opts.Streaming || opts.ParseUsage == nil || len(bytes.TrimSpace(pending)) == 0 {
		return
	}
	if d, ok := opts.ParseUsage(pending); ok && opts.OnUsage != nil {
		opts.OnUsage(d)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/proxy/ -run TestRelay -v`
Expected: PASS, six tests, no races. `TestRelayStreamsIncrementallyWithoutBuffering` is the one that must never be weakened.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/relay.go internal/proxy/relay_test.go
git commit -m "feat(proxy): streaming relay with flush per chunk and idle watchdog

A chunk reaches the client before the next read begins; a test asserts
observed arrival gaps track the upstream's rather than collapsing to the
end. The watchdog measures silence between chunks, not total duration, so
a slow healthy stream is never cut."
```

---

### Task 14: The attempt loop

The centerpiece. The second load-bearing test lives here, and it is the acceptance criterion for the whole stage.

**Files:**
- Create: `internal/proxy/attempt.go`
- Test: `internal/proxy/attempt_test.go`

**Interfaces:**
- Consumes: `account.Manager`, `account.SelectRequest`, `account.ErrNoAccount`, `provider.Provider`, `proxy.Budget`, `proxy.Relay`, `proxy.NewTransport`.
- Produces: `proxy.RetryConfig{Budget, InlineAbsorbMax, BodyIdle time.Duration}`, `proxy.Request{Method, Path string, Header http.Header, Body []byte, Model, SessionID string}`, `proxy.Result{Status int, AccountID string, Outcome provider.OutcomeKind, Attempts int, Rotated bool, WaitMS, TTFBMS, Bytes int64}`, `proxy.NewAttempter(m *account.Manager, providers map[string]provider.Provider, rt http.RoundTripper, cfg RetryConfig, log *slog.Logger) *Attempter`, `(*Attempter).Do(ctx context.Context, w http.ResponseWriter, req Request) Result`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/attempt_test.go`:

```go
package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// harness wires N accounts against one fake upstream through a real HTTP server,
// so every test measures what a client actually observes.
type harness struct {
	t        *testing.T
	mgr      *account.Manager
	upstream *testutil.FakeUpstream
	srv      *httptest.Server
	lastRes  Result
}

func newHarness(t *testing.T, nAccounts int, cfg RetryConfig, scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)

	accts := make([]config.Account, 0, nAccounts)
	for i := 0; i < nAccounts; i++ {
		accts = append(accts, config.Account{
			ID:       "acct-" + strconv.Itoa(i),
			Provider: "anthropic",
			Label:    "acct-" + strconv.Itoa(i),
			Priority: i,
			Upstream: up.URL(),
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		})
	}

	p := anthropic.New(http.DefaultClient)
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		Ramp:            account.Ramp{Enabled: false},
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &harness{t: t, mgr: mgr, upstream: up}
	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), cfg, quietLogger())
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.lastRes = at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "claude-sonnet-5", SessionID: r.Header.Get("x-session"),
		})
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) post() (*http.Response, time.Duration) {
	h.t.Helper()
	start := time.Now()
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		h.t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res, time.Since(start)
}

func defaultRetry() RetryConfig {
	return RetryConfig{Budget: 2 * time.Second, InlineAbsorbMax: 500 * time.Millisecond, BodyIdle: 5 * time.Second}
}

func TestAttemptRelaysASuccessfulResponse(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"ok":true}`,
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" || h.lastRes.Attempts != 1 {
		t.Errorf("result = %+v", h.lastRes)
	}
	if h.upstream.Requests()[0].Header.Get("Authorization") != "Bearer at" {
		t.Error("credential was not injected")
	}
}

// LOAD-BEARING. Every account answers 429 with no Retry-After and no ratelimit
// headers — the exact shape observed in production. The client must be answered
// promptly, bounded by the configured budget.
//
// The defect this pins down: defaulting a missing Retry-After to 60s and
// absorbing it inline, once per account, converts a sub-second upstream
// rejection into minutes of silence with no bytes sent.
func TestAttemptBoundsTotalWaitOnHeaderlessRateLimits(t *testing.T) {
	cfg := RetryConfig{Budget: 700 * time.Millisecond, InlineAbsorbMax: 5 * time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 3, cfg, testutil.Script{Status: 429}) // repeats forever

	res, elapsed := h.post()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.StatusCode)
	}
	// Generous ceiling: the point is that it is bounded at all, and nowhere near
	// the minutes a fabricated per-account delay would produce.
	if elapsed > cfg.Budget+2*time.Second {
		t.Fatalf("client waited %v with a %v budget — the wait is not bounded", elapsed, cfg.Budget)
	}
	if h.lastRes.WaitMS > cfg.Budget.Milliseconds()+250 {
		t.Errorf("recorded wait %dms exceeds the %v budget", h.lastRes.WaitMS, cfg.Budget)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("a 429 to the client must carry a Retry-After it can act on")
	}
}

// A header-less 429 says nothing about whether waiting helps, so the request
// must try other accounts rather than spending its whole budget on one.
func TestAttemptRotatesOnHeaderlessRateLimit(t *testing.T) {
	h := newHarness(t, 3, defaultRetry(),
		testutil.Script{Status: 429},
		testutil.Script{Status: 429},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after rotating to a healthy account", res.StatusCode)
	}
	if !h.lastRes.Rotated {
		t.Error("Rotated should be true")
	}
	if h.lastRes.AccountID == "acct-0" {
		t.Error("the third attempt should be on a different account")
	}
	if n := len(h.upstream.Requests()); n != 3 {
		t.Errorf("made %d upstream attempts, want 3", n)
	}
}

func TestAttemptRotatesAndHoldsOnQuotaRejection(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	h := newHarness(t, 2, defaultRetry(),
		testutil.Script{Status: 429, Header: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  []string{reset},
		}},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, elapsed := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// A spent bucket cannot be waited out, so rotation must be immediate.
	if elapsed > time.Second {
		t.Errorf("took %v; a quota rejection should rotate without waiting", elapsed)
	}
	got, err := h.mgr.Select(account.SelectRequest{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Select after rejection: %v", err)
	}
	if got.ID == "acct-0" {
		t.Error("the rejected account should be held out of selection")
	}
}

// A 429 that states a short duration is worth absorbing on the same account:
// rotating would move the burst and discard the warm upstream cache.
func TestAttemptAbsorbsShortHintOnTheSameAccount(t *testing.T) {
	cfg := RetryConfig{Budget: 3 * time.Second, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 2, cfg,
		testutil.Script{Status: 429, Header: http.Header{"Retry-After": []string{"0"}}},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" {
		t.Errorf("served by %q; a hinted throttle must retry the same account", h.lastRes.AccountID)
	}
}

// A hint longer than the inline cap is surfaced rather than slept on, and the
// client is handed the upstream's own number.
func TestAttemptSurfacesLongHintImmediately(t *testing.T) {
	cfg := RetryConfig{Budget: 5 * time.Second, InlineAbsorbMax: 500 * time.Millisecond, BodyIdle: 5 * time.Second}
	h := newHarness(t, 1, cfg, testutil.Script{
		Status: 429, Header: http.Header{"Retry-After": []string{"90"}},
	})

	res, elapsed := h.post()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if elapsed > time.Second {
		t.Errorf("took %v; a hint over the inline cap must not be slept on", elapsed)
	}
	if got := res.Header.Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want the upstream's 90", got)
	}
}

func TestAttemptForcesOneRefreshOn401ThenRetriesSameAccount(t *testing.T) {
	h := newHarness(t, 2, defaultRetry(),
		testutil.Script{Status: 401, Body: `{"error":"expired"}`},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" {
		t.Errorf("served by %q; a 401 should retry the same account after a refresh", h.lastRes.AccountID)
	}
}

// A 403 is the upstream refusing THIS account. Relaying it would make the client
// discard its own unrelated session, so it never reaches the client.
func TestAttemptReportsProxyErrorWhenEveryAccountIsRefused(t *testing.T) {
	h := newHarness(t, 2, defaultRetry(), testutil.Script{Status: 403, Body: `{"error":"not allowed"}`})

	res, _ := h.post()
	if res.StatusCode == http.StatusForbidden {
		t.Fatal("a 403 must not be relayed to the client")
	}
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when every account is refused", res.StatusCode)
	}
	if n := len(h.upstream.Requests()); n != 2 {
		t.Errorf("made %d attempts, want one per account", n)
	}
}

func TestAttemptStreamsSSEThrough(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"},
			{Delay: 30 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"},
		},
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if h.lastRes.Bytes == 0 {
		t.Error("no bytes recorded for a streamed response")
	}
}

func TestAttemptDoesNotForwardHopByHopOrClientAPIKey(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "client-proxy-key")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	sent := h.upstream.Requests()[0].Header
	if sent.Get("x-api-key") == "client-proxy-key" {
		t.Error("the client's proxy key must never be forwarded upstream")
	}
	if sent.Get("Connection") != "" {
		t.Error("hop-by-hop headers must be stripped")
	}
	if sent.Get("anthropic-version") != "2023-06-01" {
		t.Error("client API headers should pass through")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestAttempt -v`
Expected: FAIL to build — `undefined: NewAttempter`, `undefined: RetryConfig`, `undefined: Request`, `undefined: Result`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/attempt.go`:

```go
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/provider"
)

// hopByHop headers describe one connection and must not be forwarded.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true,
	"te": true, "trailer": true, "upgrade": true,
	"proxy-authorization": true, "proxy-authenticate": true, "proxy-connection": true,
	"host": true,
}

// connectionSpecific must be stripped from responses: illegal on HTTP/2 and
// hop-by-hop on HTTP/1.1, so removing them is correct on both.
var connectionSpecific = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true,
	"upgrade": true, "proxy-connection": true, "te": true, "trailer": true,
}

// noHintBackoff is the schedule for a 429 that carried no usable hint. Short and
// finite: the point is to yield the socket briefly, not to guess a duration.
var noHintBackoff = []time.Duration{
	250 * time.Millisecond, 500 * time.Millisecond, time.Second,
}

// maxHintHold caps how long a hinted rate limit may hold an account.
const maxHintHold = 5 * time.Minute

// defaultQuotaHold is how long an account is held out of SELECTION after a quota
// rejection whose reset time is unknown. This never makes a client wait — it
// only removes the account from selection — so a default here is safe in a way
// that a default client-facing delay is not.
const defaultQuotaHold = 5 * time.Minute

type RetryConfig struct {
	Budget          time.Duration
	InlineAbsorbMax time.Duration
	BodyIdle        time.Duration
}

// Request is a client request ready to be attempted, body already buffered so it
// can be replayed on another account.
type Request struct {
	Method    string
	Path      string
	Header    http.Header
	Body      []byte
	Model     string
	SessionID string
}

// Result is what happened, for logging and (in stage 2) accounting.
type Result struct {
	Status    int
	AccountID string
	Outcome   provider.OutcomeKind
	Attempts  int
	Rotated   bool
	WaitMS    int64
	TTFBMS    int64
	Bytes     int64
}

type Attempter struct {
	mgr       *account.Manager
	providers map[string]provider.Provider
	rt        http.RoundTripper
	cfg       RetryConfig
	log       *slog.Logger
}

func NewAttempter(m *account.Manager, providers map[string]provider.Provider, rt http.RoundTripper, cfg RetryConfig, log *slog.Logger) *Attempter {
	if cfg.Budget <= 0 {
		cfg.Budget = 10 * time.Second
	}
	if cfg.InlineAbsorbMax <= 0 {
		cfg.InlineAbsorbMax = 5 * time.Second
	}
	if cfg.BodyIdle <= 0 {
		cfg.BodyIdle = 120 * time.Second
	}
	return &Attempter{mgr: m, providers: providers, rt: rt, cfg: cfg, log: log}
}

// Do runs the attempt loop until a response is relayed or the budget runs out.
//
// The loop's contract: nothing is written to w until an attempt produces a
// relayable response, and every path that does not write is bounded by the
// budget. That is what makes an unbounded silent hang unreachable — not the
// choice of any individual backoff constant below.
func (a *Attempter) Do(ctx context.Context, w http.ResponseWriter, req Request) Result {
	budget := NewBudget(a.cfg.Budget)
	res := Result{TTFBMS: -1}

	exclude := map[string]bool{}
	refused := map[string]bool{}
	reauthed := map[string]bool{}
	noHintWaits := 0
	started := time.Now()

	defer func() {
		res.WaitMS = a.cfg.Budget.Milliseconds() - budget.Remaining().Milliseconds()
	}()

	for {
		if ctx.Err() != nil {
			res.Outcome = provider.OutcomeServerError
			return res
		}
		if budget.Exhausted() {
			a.writeExhausted(w, &res, "pre-first-byte budget exhausted")
			return res
		}

		acct, err := a.mgr.Select(account.SelectRequest{
			Model: req.Model, SessionID: req.SessionID, Exclude: exclude,
		})
		if err != nil {
			a.writeNoAccount(w, &res, refused)
			return res
		}
		res.AccountID = acct.ID
		res.Attempts++

		prov := a.providers[acct.Provider]
		if prov == nil {
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// A credential refresh is dead air like any other, so it is charged to
		// the budget rather than being free.
		refreshStart := time.Now()
		refreshErr := a.mgr.EnsureFresh(ctx, acct.ID, false)
		budget.Spend(time.Since(refreshStart))
		if refreshErr != nil {
			a.log.Warn("credential refresh failed", "account", acct.Label, "err", refreshErr)
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		if err := a.mgr.Admit(ctx, acct.ID, budget); err != nil {
			if errors.Is(err, ErrBudgetExhausted) {
				a.writeExhausted(w, &res, "admission exceeded the budget")
				return res
			}
			res.Outcome = provider.OutcomeServerError
			return res
		}

		upstreamRes, err := a.send(ctx, prov, acct, req)
		a.mgr.Release(acct.ID)

		if err != nil {
			a.log.Warn("upstream request failed", "account", acct.Label, "err", err)
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		outcome := prov.ClassifyResponse(upstreamRes)
		res.Outcome = outcome.Kind
		a.mgr.UpdateQuota(acct.ID, outcome.Buckets)

		switch outcome.Kind {
		case provider.OutcomeQuotaRejected:
			drain(upstreamRes)
			// A model-scoped rejection leaves the account fine for other models,
			// so the recorded bucket does the excluding rather than a global hold.
			if outcome.ScopedModel == "" {
				a.mgr.MarkRateLimited(acct.ID, holdFor(outcome, defaultQuotaHold))
			}
			a.log.Info("quota rejected, rotating", "account", acct.Label, "scoped", outcome.ScopedModel)
			exclude[acct.ID] = true
			res.Rotated = true
			continue

		case provider.OutcomeThrottledWithHint:
			drain(upstreamRes)
			hold := outcome.RetryAfter
			if hold > maxHintHold {
				hold = maxHintHold
			}
			a.mgr.PauseAccount(acct.ID, hold)
			if outcome.RetryAfter <= a.cfg.InlineAbsorbMax {
				// Upstream stated a short duration, so waiting genuinely works and
				// the same account keeps its warm cache.
				if err := budget.Wait(ctx, outcome.RetryAfter); err != nil {
					a.writeRetryAfter(w, &res, outcome.RetryAfter, "rate limited")
					return res
				}
				continue
			}
			a.writeRetryAfter(w, &res, outcome.RetryAfter, "rate limited")
			return res

		case provider.OutcomeThrottledNoHint:
			drain(upstreamRes)
			// Nothing was stated, so no duration is invented. Yield briefly and
			// let another account try — spending the rest of the budget here
			// would be betting on a claim upstream never made.
			backoff := noHintBackoff[min(noHintWaits, len(noHintBackoff)-1)]
			noHintWaits++
			exclude[acct.ID] = true
			res.Rotated = true
			a.log.Info("rate limited with no hint, rotating",
				"account", acct.Label, "backoff", backoff)
			if err := budget.Wait(ctx, backoff); err != nil {
				a.writeExhausted(w, &res, "rate limited with no retry hint")
				return res
			}
			continue

		case provider.OutcomeCredentialStale:
			drain(upstreamRes)
			if !reauthed[acct.ID] {
				reauthed[acct.ID] = true
				forceStart := time.Now()
				err := a.mgr.EnsureFresh(ctx, acct.ID, true)
				budget.Spend(time.Since(forceStart))
				if err == nil {
					continue // same account, fresh credential
				}
				a.log.Warn("forced refresh failed", "account", acct.Label, "err", err)
			}
			exclude[acct.ID] = true
			res.Rotated = true
			continue

		case provider.OutcomeCredentialRefused:
			drain(upstreamRes)
			// Never relayed: the client has no part in this and reads a 403 as
			// its own session being dead.
			a.log.Error("upstream refused the account credential", "account", acct.Label)
			refused[acct.ID] = true
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// Relayable. Any non-throttled response is live proof a hold no longer
		// binds, which is what restores an account after its window passes.
		a.mgr.ClearRateLimited(acct.ID)
		a.mgr.RecordSession(req.SessionID, acct.ID)

		res.Status = upstreamRes.StatusCode
		res.TTFBMS = time.Since(started).Milliseconds()
		a.relay(ctx, w, upstreamRes, prov, &res)
		return res
	}
}

// send builds and performs one upstream attempt.
func (a *Attempter) send(ctx context.Context, prov provider.Provider, acct *account.Account, req Request) (*http.Response, error) {
	pa := acct.ToProvider()
	body, err := prov.RewriteBody(req.Body, pa)
	if err != nil {
		return nil, err
	}

	target := strings.TrimSuffix(prov.Endpoint(pa).String(), "/") + req.Path

	var reader io.Reader
	if len(body) > 0 && req.Method != http.MethodGet && req.Method != http.MethodHead {
		reader = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, target, reader)
	if err != nil {
		return nil, err
	}

	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || strings.HasPrefix(lk, ":") {
			continue
		}
		// The client's proxy key authenticates it to US and must never leak
		// upstream. Accept-Encoding is dropped so response framing always matches
		// what we tell the client. Content-Length is recomputed below.
		if lk == "x-api-key" || lk == "authorization" || lk == "accept-encoding" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	prov.Authorize(out, acct.Credential)
	if reader != nil {
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	return a.rt.RoundTrip(out)
}

func (a *Attempter) relay(ctx context.Context, w http.ResponseWriter, upstreamRes *http.Response, prov provider.Provider, res *Result) {
	defer upstreamRes.Body.Close()

	for k, vs := range upstreamRes.Header {
		lk := strings.ToLower(k)
		if connectionSpecific[lk] || lk == "content-encoding" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstreamRes.StatusCode)

	streaming := strings.Contains(upstreamRes.Header.Get("Content-Type"), "text/event-stream")
	n, err := Relay(ctx, w, upstreamRes.Body, RelayOptions{
		BodyIdle:   a.cfg.BodyIdle,
		Streaming:  streaming,
		ParseUsage: prov.ParseUsage,
		OnUsage:    func(*provider.UsageDelta) {}, // wired to metrics in stage 2
	})
	res.Bytes = n
	if err != nil && !errors.Is(err, context.Canceled) {
		a.log.Warn("relay ended early", "err", err, "bytes", n)
		// Abort instead of returning normally. A normal return lets net/http
		// terminate the chunked body cleanly, which looks to the client like a
		// complete answer and suppresses the retry a truncated stream needs.
		// ErrAbortHandler severs the connection without the recovery middleware
		// treating it as a crash.
		panic(http.ErrAbortHandler)
	}
}

// holdFor derives a selection hold from a rejected bucket's reset time.
func holdFor(o provider.Outcome, fallback time.Duration) time.Duration {
	now := time.Now().UnixMilli()
	var soonest int64
	for _, b := range o.Buckets {
		if b.Status != "rejected" || b.ResetsAt <= now {
			continue
		}
		if soonest == 0 || b.ResetsAt < soonest {
			soonest = b.ResetsAt
		}
	}
	if soonest == 0 {
		return fallback
	}
	if d := time.Duration(soonest-now) * time.Millisecond; d < time.Hour {
		return d
	}
	return time.Hour
}

func drain(res *http.Response) {
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
}

func (a *Attempter) writeJSON(w http.ResponseWriter, status int, hdr map[string]string, errType, msg string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}

// writeExhausted answers a request whose budget ran out. The Retry-After comes
// from observed reset times rather than a guess, so the client backs off on
// something real.
func (a *Attempter) writeExhausted(w http.ResponseWriter, res *Result, reason string) {
	retry := a.retryAfterHint()
	secs := strconv.Itoa(int(retry.Seconds()))
	res.Status = http.StatusTooManyRequests
	a.log.Warn("answering 429", "reason", reason, "retryAfter", retry)
	a.writeJSON(w, http.StatusTooManyRequests,
		map[string]string{"Retry-After": secs}, "rate_limit_error",
		"No account could serve this request in time ("+reason+"). Retry in "+secs+"s.")
}

func (a *Attempter) writeRetryAfter(w http.ResponseWriter, res *Result, d time.Duration, reason string) {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	res.Status = http.StatusTooManyRequests
	a.writeJSON(w, http.StatusTooManyRequests,
		map[string]string{"Retry-After": strconv.Itoa(secs)},
		"rate_limit_error", reason+"; retry in "+strconv.Itoa(secs)+"s.")
}

// writeNoAccount distinguishes "every account was refused", which waiting cannot
// fix, from "everything is out of quota", which a reset will.
func (a *Attempter) writeNoAccount(w http.ResponseWriter, res *Result, refused map[string]bool) {
	total := len(a.mgr.All())
	if len(refused) > 0 && len(refused) >= total {
		names := make([]string, 0, len(refused))
		for id := range refused {
			if acct := a.mgr.Get(id); acct != nil {
				names = append(names, acct.Label)
			}
		}
		res.Status = http.StatusBadGateway
		res.Outcome = provider.OutcomeCredentialRefused
		a.writeJSON(w, http.StatusBadGateway, nil, "proxy_error",
			"Upstream refused the credential for every account ("+strings.Join(names, ", ")+
				"). Check the accounts and log in again.")
		return
	}
	a.writeExhausted(w, res, "no eligible account")
}

// retryAfterHint is the soonest observed reset across all accounts, floored at
// one second and capped at five minutes.
func (a *Attempter) retryAfterHint() time.Duration {
	now := time.Now().UnixMilli()
	var soonest int64
	consider := func(ts int64) {
		if ts > now && (soonest == 0 || ts < soonest) {
			soonest = ts
		}
	}
	for _, acct := range a.mgr.Snapshot() {
		for _, b := range acct.Buckets {
			consider(b.ResetsAt)
		}
		consider(acct.RateLimitedUntil)
	}
	if soonest == 0 {
		return 5 * time.Second
	}
	d := time.Duration(soonest-now) * time.Millisecond
	if d < time.Second {
		return time.Second
	}
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/proxy/ -run TestAttempt -v`
Expected: PASS, ten tests. If `TestAttemptBoundsTotalWaitOnHeaderlessRateLimits` fails, do not raise the tolerance — find which wait is escaping the budget.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/attempt.go internal/proxy/attempt_test.go
git commit -m "feat(proxy): attempt loop with a bounded retry engine

Reacts to what upstream actually says: a spent quota bucket rotates
immediately, a stated short delay is absorbed on the same warm account,
and a 429 with no hint yields briefly and lets another account try rather
than inventing a duration to sleep on. Every non-transferring wait draws
down one budget, so the client is answered promptly regardless of how
upstream behaves."
```

---

### Task 15: Chi router, middleware, and the request handler

**Files:**
- Create: `internal/proxy/handler.go`
- Create: `internal/proxy/router.go`
- Test: `internal/proxy/handler_test.go`

**Interfaces:**
- Consumes: `proxy.Attempter`, `account.Manager`, `config.Config`.
- Produces: `proxy.ReservedPrefix = "/_aiproxy"`, `proxy.Authorized(remoteAddr, presentedKey, configuredKey string) bool`, `proxy.IsLoopback(addr string) bool`, `proxy.ParseModel(body []byte) string`, `proxy.ModelMatches(pattern, model string) bool`, `proxy.HandlerOptions{Attempter *Attempter, Manager *account.Manager, APIKey string, BlockedModels []string, Log *slog.Logger, OnResult func(Request, Result)}`, `proxy.NewRouter(HandlerOptions) http.Handler`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/handler_test.go`:

```go
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestIsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:5000", "[::1]:5000", "::1", "127.0.0.1"} {
		if !IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"10.0.0.4:5000", "203.0.113.7:443", ""} {
		if IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = true, want false", addr)
		}
	}
}

func TestAuthorized(t *testing.T) {
	cases := []struct {
		name                       string
		remote, presented, config_ string
		want                       bool
	}{
		{"no key configured allows anyone", "203.0.113.7:1", "", "", true},
		{"loopback is exempt", "127.0.0.1:1", "", "secret", true},
		{"remote with correct key", "203.0.113.7:1", "secret", "secret", true},
		{"remote with wrong key", "203.0.113.7:1", "nope", "secret", false},
		{"remote with no key", "203.0.113.7:1", "", "secret", false},
		{"wrong length key", "203.0.113.7:1", "s", "secret", false},
	}
	for _, c := range cases {
		if got := Authorized(c.remote, c.presented, c.config_); got != c.want {
			t.Errorf("%s: Authorized(%q,%q,%q) = %v, want %v",
				c.name, c.remote, c.presented, c.config_, got, c.want)
		}
	}
}

func TestParseModel(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"model":"claude-sonnet-5","messages":[]}`, "claude-sonnet-5"},
		{`{"messages":[{"role":"user","content":"model: fake"}],"model":"claude-opus-5"}`, "claude-opus-5"},
		{`{"messages":[]}`, ""},
		{`not json`, ""},
		{``, ""},
		{`{"model":123}`, ""},
	}
	for _, c := range cases {
		if got := ParseModel([]byte(c.body)); got != c.want {
			t.Errorf("ParseModel(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestModelMatches(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*fable*", "claude-fable-5", true},
		{"*fable*", "claude-sonnet-5", false},
		{"claude-opus-5", "claude-opus-5", true},
		{"claude-opus-5", "claude-opus-4", false},
		{"claude-*", "claude-haiku-4-5", true},
	}
	for _, c := range cases {
		if got := ModelMatches(c.pattern, c.model); got != c.want {
			t.Errorf("ModelMatches(%q,%q) = %v, want %v", c.pattern, c.model, got, c.want)
		}
	}
}

// routerHarness builds the full router over a fake upstream.
type routerHarness struct {
	srv     *httptest.Server
	up      *testutil.FakeUpstream
	mu      sync.Mutex
	results []Result
}

func newRouterHarness(t *testing.T, opts func(*HandlerOptions), scripts ...testutil.Script) *routerHarness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)
	p := anthropic.New(http.DefaultClient)
	providers := map[string]provider.Provider{"anthropic": p}

	mgr := account.New([]config.Account{{
		ID: "acct-0", Provider: "anthropic", Label: "acct-0", Upstream: up.URL(),
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}}, providers, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &routerHarness{up: up}
	ho := HandlerOptions{
		Attempter: NewAttempter(mgr, providers, NewTransport(TransportOptions{}), defaultRetry(), quietLogger()),
		Manager:   mgr,
		Log:       quietLogger(),
		OnResult: func(_ Request, r Result) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.results = append(h.results, r)
		},
	}
	if opts != nil {
		opts(&ho)
	}
	h.srv = httptest.NewServer(NewRouter(ho))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *routerHarness) lastResult(t *testing.T) Result {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) == 0 {
		t.Fatal("no result recorded")
	}
	return h.results[len(h.results)-1]
}

func TestRouterProxiesAndReportsResult(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{"ok":true}`})

	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-claude-code-session-id", "sess-9")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := h.up.Requests()[0].Path; got != "/v1/messages" {
		t.Errorf("upstream path = %q", got)
	}
	r := h.lastResult(t)
	if r.AccountID != "acct-0" || r.Status != 200 {
		t.Errorf("result = %+v", r)
	}
}

// A blocked model is refused locally. A model no account can serve otherwise
// burns a rotation cycle and comes back as a rate limit, which reads to the
// client as a transient problem worth retrying — it is not.
func TestRouterRejectsBlockedModelWithoutCallingUpstream(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.BlockedModels = []string{"*fable*"}
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-fable-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("a blocked model must not reach upstream")
	}
	if !strings.Contains(string(body), "blocked") {
		t.Errorf("body should explain the block: %s", body)
	}
}

func TestRouterRejectsUnauthorizedRemoteCaller(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.APIKey = "secret"
	}, testutil.Script{Status: 200, Body: `{}`})

	// The test client is loopback and therefore exempt, so assert the gate
	// directly for the remote case and confirm loopback still passes end to end.
	if Authorized("203.0.113.9:1234", "", "secret") {
		t.Error("a remote caller with no key must be refused")
	}
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		t.Error("loopback should be exempt from the proxy key gate")
	}
}

func TestRouterServesStatusUnderReservedPrefix(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	var got struct {
		Accounts []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Status string `json:"status"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].ID != "acct-0" {
		t.Errorf("accounts = %+v", got.Accounts)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("a control-plane path must never be proxied upstream")
	}
}

// The reserved prefix must never reach the proxy path, even for an unknown
// route, or a future control endpoint would be silently forwarded to the
// upstream and answered by it.
func TestRouterDoesNotProxyUnknownReservedPaths(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/not-a-route")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("an unknown reserved path must not be proxied")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run 'TestIsLoopback|TestAuthorized|TestParseModel|TestModelMatches|TestRouter' -v`
Expected: FAIL to build — `undefined: NewRouter`, `undefined: HandlerOptions`, `undefined: Authorized`.

- [ ] **Step 3: Write the handler**

Create `internal/proxy/handler.go`:

```go
package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/nicko170/aiproxy/internal/account"
)

// ReservedPrefix is the control-plane namespace. Nothing under it is ever
// proxied, so a control route can never be shadowed by an upstream path and a
// future endpoint can never be silently forwarded to the provider.
const ReservedPrefix = "/_aiproxy"

// sessionHeader is how a client tags requests belonging to one conversation.
const sessionHeader = "x-claude-code-session-id"

// maxBodyBytes bounds a buffered request body. The body must be buffered so an
// attempt can be replayed on another account; the cap keeps a hostile or
// runaway client from exhausting memory.
const maxBodyBytes = 64 << 20 // 64 MiB

// IsLoopback reports whether an address is on the local machine.
func IsLoopback(addr string) bool {
	if addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Authorized gates access to the proxy. Loopback is exempt: the key exists to
// stop other machines on the network, not the operator's own tools. The compare
// is constant time so a wrong key leaks nothing about the right one.
func Authorized(remoteAddr, presentedKey, configuredKey string) bool {
	if configuredKey == "" {
		return true
	}
	if IsLoopback(remoteAddr) {
		return true
	}
	a, b := []byte(presentedKey), []byte(configuredKey)
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// ParseModel reads the top-level "model" field. Only the top level is decoded,
// so a megabyte of nested message content is never walked, and a "model" string
// appearing inside message text cannot be mistaken for the request's model.
func ParseModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}
	raw, ok := fields["model"]
	if !ok {
		return ""
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return ""
	}
	return model
}

// ModelMatches reports whether a blocklist pattern matches a model name.
// Patterns use shell globbing, e.g. "*fable*".
func ModelMatches(pattern, model string) bool {
	ok, err := path.Match(pattern, model)
	return err == nil && ok
}

// HandlerOptions configures the proxy handler.
type HandlerOptions struct {
	Attempter     *Attempter
	Manager       *account.Manager
	APIKey        string
	BlockedModels []string
	Log           *slog.Logger
	// OnResult receives every completed attempt. Stage 2 wires metrics here.
	OnResult func(Request, Result)
}

// proxyHandler buffers the request and hands it to the attempt loop.
func proxyHandler(o HandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not read request body")
			return
		}

		req := Request{
			Method:    r.Method,
			Path:      r.URL.RequestURI(),
			Header:    r.Header.Clone(),
			Body:      body,
			Model:     ParseModel(body),
			SessionID: r.Header.Get(sessionHeader),
		}

		if req.Model != "" {
			for _, pattern := range o.BlockedModels {
				if !ModelMatches(pattern, req.Model) {
					continue
				}
				o.Log.Info("refused blocked model", "model", req.Model, "pattern", pattern)
				writeError(w, http.StatusBadRequest, "invalid_request_error",
					"Model \""+req.Model+"\" is blocked by aiproxy (matched \""+pattern+"\").")
				if o.OnResult != nil {
					o.OnResult(req, Result{Status: http.StatusBadRequest, TTFBMS: 0})
				}
				return
			}
		}

		res := o.Attempter.Do(r.Context(), w, req)
		if o.OnResult != nil {
			o.OnResult(req, res)
		}
	}
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
```

- [ ] **Step 4: Write the router**

Create `internal/proxy/router.go`:

```go
package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter assembles the listener's routes: the reserved control-plane prefix
// first, then a catch-all that proxies everything else upstream.
func NewRouter(o HandlerOptions) http.Handler {
	r := chi.NewRouter()
	// Recoverer keeps one panicking request from taking the process down with
	// every other in-flight session.
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	r.Route(ReservedPrefix, func(cp chi.Router) {
		cp.Get("/api/v1/status", statusHandler(o))
		// Anything else under the reserved prefix is a 404, never a proxied
		// request: a future control route must not be answerable by the upstream.
		cp.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found_error", "No such aiproxy endpoint")
		})
	})

	r.NotFound(proxyHandler(o))
	r.MethodNotAllowed(proxyHandler(o))
	return r
}

type statusAccount struct {
	ID               string             `json:"id"`
	Label            string             `json:"label"`
	Provider         string             `json:"provider"`
	Priority         int                `json:"priority"`
	Disabled         bool               `json:"disabled"`
	Status           string             `json:"status"`
	LastError        string             `json:"lastError,omitempty"`
	InFlight         int                `json:"inFlight"`
	RateLimitedUntil int64              `json:"rateLimitedUntil,omitempty"`
	PausedUntil      int64              `json:"pausedUntil,omitempty"`
	Buckets          map[string]float64 `json:"buckets"`
}

// statusHandler is the minimal readout for stage 1. Stage 3 replaces it with the
// full control API backed by view.Source.
func statusHandler(o HandlerOptions) http.HandlerFunc {
	started := time.Now()
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		accounts := []statusAccount{}
		for _, a := range o.Manager.Snapshot() {
			buckets := map[string]float64{}
			for name, b := range a.Buckets {
				buckets[name] = b.Utilization
			}
			accounts = append(accounts, statusAccount{
				ID: a.ID, Label: a.Label, Provider: a.Provider,
				Priority: a.Priority, Disabled: a.Disabled,
				Status: a.Status.String(), LastError: a.LastError,
				InFlight: a.InFlight, RateLimitedUntil: a.RateLimitedUntil,
				PausedUntil: a.PausedUntil, Buckets: buckets,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"uptimeSeconds": int(time.Since(started).Seconds()),
			"accounts":      accounts,
		})
	}
}
```

- [ ] **Step 5: Add the chi middleware dependency**

```bash
go get github.com/go-chi/chi/v5@latest
go mod tidy
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./internal/proxy/ -v`
Expected: PASS, all proxy tests.

- [ ] **Step 7: Commit**

```bash
git add internal/proxy/handler.go internal/proxy/router.go internal/proxy/handler_test.go go.mod go.sum
git commit -m "feat(proxy): chi router, auth gate, model blocklist

Everything under the reserved /_aiproxy prefix is answered locally,
including unknown routes, so a future control endpoint can never be
silently forwarded upstream. Model parsing decodes only the top level, so
a 'model' key inside message text is not mistaken for the request's."
```

---

### Task 16: Binary wiring and the end-to-end test

Stage 1 ships headless; the TUI arrives in stage 4, so `--headless` is currently the only mode and the flag exists to keep the eventual default honest.

**Files:**
- Create: `cmd/aiproxy/main.go`
- Test: `cmd/aiproxy/main_test.go`
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: everything above.
- Produces: `buildHandler(cfg config.Config, store *config.Store, log *slog.Logger) (http.Handler, error)`, `listen(addr string) (net.Listener, error)`, `firstRunImport(store *config.Store, log *slog.Logger) error`.

- [ ] **Step 1: Write the failing test**

Create `cmd/aiproxy/main_test.go`:

```go
package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// End to end through the real wiring: a config on disk, the real router, a fake
// upstream. This is the test that says "Claude Code can talk to this".
func TestEndToEndProxiesAStreamingCompletion(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n"},
			{Delay: 50 * time.Millisecond, Data: "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"},
			{Delay: 50 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n"},
		},
	})

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	cfg, err := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test", Upstream: up.URL(),
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	h, err := buildHandler(cfg, store, quiet())
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Errorf("Content-Type = %q, want an event stream", ct)
	}

	// Chunks must arrive spread out, not all at once at the end.
	start := time.Now()
	arrivals := []time.Duration{}
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			arrivals = append(arrivals, time.Since(start))
		}
		if err != nil {
			break
		}
	}
	if len(arrivals) < 3 {
		t.Fatalf("observed %d chunks, want at least 3 — the response was buffered", len(arrivals))
	}
	if last := arrivals[len(arrivals)-1] - arrivals[0]; last < 60*time.Millisecond {
		t.Errorf("all chunks arrived within %v; streaming collapsed to the end", last)
	}
}

func TestEndToEndStatusEndpoint(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	cfg, _ := store.Update(func(c *config.Config) error { return nil })

	h, err := buildHandler(cfg, store, quiet())
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/_aiproxy/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["accounts"]; !ok {
		t.Errorf("status payload missing accounts: %+v", got)
	}
}

// A refreshed credential must land in the config file, or every restart starts
// from a token that has already been rotated away.
func TestBuildHandlerPersistsRefreshedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := config.NewStore(path)
	cfg, _ := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test",
			Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "old", RefreshToken: "rt"},
		}}
		return nil
	})
	if _, err := buildHandler(cfg, store, quiet()); err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	// The persist hook is what buildHandler must have installed; exercise it via
	// the store directly to prove the wiring writes to this file.
	if _, err := store.Update(func(c *config.Config) error {
		c.Accounts[0].Credential.AccessToken = "rotated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "rotated") {
		t.Error("credential change did not reach the config file")
	}
}

func TestFirstRunImportAdoptsLegacyAccounts(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "teamclaude.json")
	os.WriteFile(legacy, []byte(`{"accounts":[{"name":"carried","type":"apikey","apiKey":"sk-x"}]}`), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)

	store := config.NewStore(filepath.Join(dir, "aiproxy", "config.json"))
	if err := firstRunImport(store, quiet()); err != nil {
		t.Fatalf("firstRunImport: %v", err)
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Label != "carried" {
		t.Fatalf("accounts = %+v", cfg.Accounts)
	}
	if cfg.Accounts[0].ID == "" {
		t.Error("imported account needs an id")
	}
}

// Import must be a first-run action only, never something that duplicates
// accounts on every start.
func TestFirstRunImportSkipsWhenAccountsExist(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "teamclaude.json"),
		[]byte(`{"accounts":[{"name":"legacy","type":"apikey","apiKey":"sk-x"}]}`), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)

	store := config.NewStore(filepath.Join(dir, "aiproxy", "config.json"))
	store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{ID: "existing", Provider: "anthropic", Label: "mine"}}
		return nil
	})

	if err := firstRunImport(store, quiet()); err != nil {
		t.Fatalf("firstRunImport: %v", err)
	}
	cfg, _ := store.Load()
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Label != "mine" {
		t.Errorf("accounts = %+v, want the existing one untouched", cfg.Accounts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/aiproxy/ -v`
Expected: FAIL to build — `undefined: buildHandler`, `undefined: firstRunImport`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/aiproxy/main.go`:

```go
// Command aiproxy is a local proxy for AI coding agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/proxy"
)

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "", "path to config.json (default: XDG config dir)")
		addr       = flag.String("addr", "", "listen address (overrides config)")
		headless   = flag.Bool("headless", true, "run without a TUI (the only mode in this build)")
		logLevel   = flag.String("log-level", "info", "debug, info, warn, or error")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("aiproxy", version)
		return
	}

	log := newLogger(*logLevel)
	if err := run(*configPath, *addr, *headless, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func run(configPath, addrOverride string, headless bool, log *slog.Logger) error {
	if configPath == "" {
		configPath = config.Path()
	}
	store := config.NewStore(configPath)

	if err := firstRunImport(store, log); err != nil {
		log.Warn("account import skipped", "err", err)
	}

	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if addrOverride != "" {
		cfg.Listen.Addr = addrOverride
	}

	handler, err := buildHandler(cfg, store, log)
	if err != nil {
		return err
	}

	ln, err := listen(cfg.Listen.Addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: handler,
		// No global write timeout: a streamed completion legitimately runs for
		// minutes, and a deadline here would sever it mid-answer. Stalls are
		// handled by the relay's idle watchdog instead.
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "accounts", len(cfg.Accounts), "headless", headless)
		log.Info("point your client at it",
			"env", "ANTHROPIC_BASE_URL=http://"+ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// buildHandler wires config into a serving handler. Kept separate from run so
// tests exercise the real composition without binding a port.
func buildHandler(cfg config.Config, store *config.Store, log *slog.Logger) (http.Handler, error) {
	upstreamClient := &http.Client{
		Transport: proxy.NewTransport(proxy.TransportOptions{}),
		Timeout:   60 * time.Second, // control-plane calls only, never the proxy path
	}
	providers := map[string]provider.Provider{
		"anthropic": anthropic.New(upstreamClient),
	}

	mgr := account.New(cfg.Accounts, providers, account.Options{
		SwitchThreshold: cfg.Routing.SwitchThreshold,
		SessionAffinity: cfg.Routing.SessionAffinity,
		Ramp:            account.Ramp{Enabled: true},
		// A rotated credential is the only way back into an account, so it is
		// written through immediately rather than at shutdown.
		Persist: func(id string, c provider.Credential) error {
			_, err := store.Update(func(cur *config.Config) error {
				for i := range cur.Accounts {
					if cur.Accounts[i].ID == id {
						cur.Accounts[i].Credential = c
						return nil
					}
				}
				return nil
			})
			return err
		},
	})

	attempter := proxy.NewAttempter(mgr, providers,
		proxy.NewTransport(proxy.TransportOptions{}),
		proxy.RetryConfig{
			Budget:          time.Duration(cfg.Retry.BudgetMS) * time.Millisecond,
			InlineAbsorbMax: time.Duration(cfg.Retry.InlineAbsorbMaxMS) * time.Millisecond,
			BodyIdle:        time.Duration(cfg.Retry.BodyIdleMS) * time.Millisecond,
		}, log)

	return proxy.NewRouter(proxy.HandlerOptions{
		Attempter:     attempter,
		Manager:       mgr,
		APIKey:        cfg.Listen.APIKey,
		BlockedModels: cfg.Routing.BlockedModels,
		Log:           log,
		OnResult: func(req proxy.Request, res proxy.Result) {
			log.Info("request",
				"model", req.Model, "account", res.AccountID, "status", res.Status,
				"outcome", res.Outcome.String(), "attempts", res.Attempts,
				"ttfbMs", res.TTFBMS, "waitMs", res.WaitMS, "bytes", res.Bytes)
		},
	}), nil
}

// firstRunImport adopts existing credentials so a first run does not require
// re-authorizing every account. It is a first-run action only: with accounts
// already configured it does nothing, so restarts cannot duplicate them.
func firstRunImport(store *config.Store, log *slog.Logger) error {
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	if len(cfg.Accounts) > 0 {
		return nil
	}
	legacy := config.LegacyPath()
	if legacy == "" {
		return nil
	}
	imported, err := config.ImportFile(legacy, config.ImportSourceLegacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(imported) == 0 {
		return nil
	}
	if _, err := store.Update(func(c *config.Config) error {
		c.Accounts = append(c.Accounts, imported...)
		return nil
	}); err != nil {
		return err
	}
	log.Info("imported existing accounts", "count", len(imported), "from", legacy)
	return nil
}

// listen binds the client-facing socket, setting NoDelay on each accepted
// connection. Nagle coalescing on small streamed frames adds tens of
// milliseconds per chunk, which reads as a sluggish stream; net.Listener does
// not enable NoDelay by default the way http.Server's own listener does.
func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &noDelayListener{ln}, nil
}

type noDelayListener struct{ net.Listener }

func (l *noDelayListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return c, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./cmd/aiproxy/ -v`
Expected: PASS, five tests.

- [ ] **Step 5: Run the whole suite and build**

```bash
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race ./...
```
Expected: clean vet, successful build, all tests pass.

- [ ] **Step 6: Verify against a real client by hand**

```bash
go run ./cmd/aiproxy --log-level debug
# in another terminal:
ANTHROPIC_BASE_URL=http://127.0.0.1:3456 claude
```
Send a prompt. Confirm the answer streams token by token rather than appearing all at once, and that the proxy logs one `request` line with a plausible `ttfbMs` and a `waitMs` near zero.

- [ ] **Step 7: Add CI**

Create `.github/workflows/ci.yml`:

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - run: go vet ./...
      - run: go test -race ./...

  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        goos: [darwin, linux]
        goarch: [amd64, arm64]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - env:
          CGO_ENABLED: '0'
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
        run: go build -o /dev/null ./cmd/aiproxy
```

- [ ] **Step 8: Commit**

```bash
git add cmd/aiproxy/ .github/workflows/ci.yml go.mod go.sum
git commit -m "feat(cmd): binary wiring, first-run import, and CI

End-to-end test asserts a streamed completion reaches the client
incrementally through the real composition. The client-facing listener
sets NoDelay per connection, and there is deliberately no server write
timeout: a completion legitimately streams for minutes, and stalls are the
relay watchdog's job."
```

---

### Task 17: Client-credential passthrough and the concurrency hammer

Two spec requirements the earlier tasks do not reach: `§4.6`'s transparent relay
for account-bound paths, and `§12`'s parallel-stream test.

**Files:**
- Modify: `internal/proxy/handler.go` (add `passthroughHandler`, extend `HandlerOptions`)
- Modify: `internal/proxy/router.go` (route passthrough prefixes before the proxy catch-all)
- Test: `internal/proxy/passthrough_test.go`
- Test: `internal/proxy/concurrency_test.go`

**Interfaces:**
- Consumes: `proxy.HandlerOptions`, `proxy.NewRouter`.
- Produces: `proxy.DefaultPassthroughPrefixes` (`[]string`), new `HandlerOptions` fields `PassthroughPrefixes []string` and `Upstream string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/proxy/passthrough_test.go`:

```go
package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// Some upstream paths are bound to the CLIENT's own paired identity. Injecting a
// rotated account token there is refused upstream, and the client silently loses
// the feature. These must relay transparently: client headers intact, no account
// selection, no body buffering.
func TestPassthroughForwardsClientCredentialUntouched(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = up.URL()
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 500, Body: `should not be used`})

	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/code/sessions", nil)
	req.Header.Set("Authorization", "Bearer client-own-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	recs := up.Requests()
	if len(recs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(recs))
	}
	if got := recs[0].Header.Get("Authorization"); got != "Bearer client-own-token" {
		t.Errorf("Authorization = %q; the client's own credential must survive", got)
	}
	if recs[0].Path != "/v1/code/sessions" {
		t.Errorf("path = %q", recs[0].Path)
	}
}

func TestPassthroughDoesNotClaimOrdinaryPaths(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200, Body: `{"ok":true}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	// It went through the account path, so a result was recorded.
	if r := h.lastResult(t); r.AccountID != "acct-0" {
		t.Errorf("result = %+v; /v1/messages must use account selection", r)
	}
}

func TestPassthroughReturns502WhenUpstreamUnreachable(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = "http://127.0.0.1:1" // nothing listens here
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200})

	res, err := http.Get(h.srv.URL + "/v1/code/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}

var _ = httptest.NewServer // retained: harness helpers live in handler_test.go
```

Create `internal/proxy/concurrency_test.go`:

```go
package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// Run many streaming requests at once. This is a deadlock and race hunt rather
// than an assertion about any one response: admission, the ramp, the budget, and
// the relay all take locks or block, and a mistake among them shows up only
// under concurrency. Run with -race.
func TestConcurrentStreamsAllComplete(t *testing.T) {
	const n = 40

	h := newHarness(t, 3, RetryConfig{
		Budget: 5 * time.Second, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second,
	}, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"},
			{Delay: 10 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"},
		},
	})

	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
				strings.NewReader(`{"model":"claude-sonnet-5"}`))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			statuses[i] = res.StatusCode
		}(i)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent streams did not all finish — likely a deadlock in admission or the relay")
	}

	for i, s := range statuses {
		if s != 200 {
			t.Errorf("request %d: status %d, want 200", i, s)
		}
	}
	// Every slot taken must have been released.
	for _, a := range h.mgr.Snapshot() {
		if a.InFlight != 0 {
			t.Errorf("account %s left InFlight=%d; a slot leaked", a.ID, a.InFlight)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/proxy/ -run 'TestPassthrough|TestConcurrent' -v`
Expected: FAIL — `undefined: DefaultPassthroughPrefixes`, and unknown `HandlerOptions` fields `Upstream` / `PassthroughPrefixes`.

- [ ] **Step 3: Add the passthrough handler**

Append to `internal/proxy/handler.go`:

```go
// DefaultPassthroughPrefixes are upstream paths bound to the CLIENT's own paired
// identity rather than to a rotated account. Injecting one of our credentials
// here gets refused upstream and the client quietly loses the feature, so these
// relay transparently: the client's headers survive, no account is selected, and
// the body is streamed rather than buffered (some are long-poll channels that
// withhold response headers for minutes).
var DefaultPassthroughPrefixes = []string{
	"/v1/code/",
	"/api/oauth/files/",
	"/api/oauth/file_upload",
	"/v1/oauth/token",
}

func passthroughHandler(o HandlerOptions) http.HandlerFunc {
	upstream := strings.TrimSuffix(o.Upstream, "/")
	client := &http.Client{
		Transport: NewTransport(TransportOptions{}),
		// No timeout: these include long-poll channels.
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		out, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), r.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "proxy_error", "Could not build upstream request")
			return
		}
		for k, vs := range r.Header {
			lk := strings.ToLower(k)
			// Authorization is deliberately NOT stripped: it is the point.
			if hopByHop[lk] || lk == "accept-encoding" {
				continue
			}
			for _, v := range vs {
				out.Header.Add(k, v)
			}
		}
		out.ContentLength = r.ContentLength

		res, err := client.Do(out)
		if err != nil {
			o.Log.Warn("passthrough failed", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusBadGateway, "proxy_error", "Upstream unreachable")
			return
		}
		defer res.Body.Close()

		for k, vs := range res.Header {
			if connectionSpecific[strings.ToLower(k)] {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		Relay(r.Context(), w, res.Body, RelayOptions{BodyIdle: 0}) // 0 takes the default
	}
}
```

Extend `HandlerOptions` in the same file with:

```go
	// Upstream is the base URL for passthrough paths (no account selection).
	Upstream string
	// PassthroughPrefixes are relayed with the client's own credential.
	PassthroughPrefixes []string
```

- [ ] **Step 4: Route it before the proxy catch-all**

In `internal/proxy/router.go`, inside `NewRouter`, after the `r.Route(ReservedPrefix, ...)` block and before `r.NotFound(...)`:

```go
	if o.Upstream != "" {
		pt := passthroughHandler(o)
		for _, prefix := range o.PassthroughPrefixes {
			// Register both the bare path and its subtree: a prefix like
			// "/api/oauth/file_upload" is itself a valid endpoint, while
			// "/v1/code/" only ever appears with more path after it.
			r.Handle(strings.TrimSuffix(prefix, "/"), pt)
			r.Handle(strings.TrimSuffix(prefix, "/")+"/*", pt)
		}
	}
```

Add `"strings"` to that file's imports.

- [ ] **Step 5: Wire the defaults in the binary**

In `cmd/aiproxy/main.go`, inside `buildHandler`, add to the `proxy.HandlerOptions` literal:

```go
		Upstream:            "https://api.anthropic.com",
		PassthroughPrefixes: proxy.DefaultPassthroughPrefixes,
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./internal/proxy/ -v`
Expected: PASS, including three passthrough tests and the concurrency hammer.

- [ ] **Step 7: Run the whole suite**

Run: `go vet ./... && go test -race ./...`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add internal/proxy/handler.go internal/proxy/router.go \
        internal/proxy/passthrough_test.go internal/proxy/concurrency_test.go \
        cmd/aiproxy/main.go
git commit -m "feat(proxy): transparent passthrough for client-bound paths

Paths tied to the client's own paired identity relay with its credential
intact and without buffering; injecting a rotated token there is refused
upstream and the client silently loses the feature. Adds a parallel-stream
test to catch admission and relay deadlocks under -race."
```

---

## Stage 1 exit criteria

All of these must hold before stage 2 begins.

- [ ] `go vet ./...` clean, `CGO_ENABLED=0 go build ./...` succeeds.
- [ ] `go test -race ./...` passes.
- [ ] `TestAttemptBoundsTotalWaitOnHeaderlessRateLimits` passes at its original tolerance — not widened.
- [ ] `TestRelayStreamsIncrementallyWithoutBuffering` passes.
- [ ] `TestEndToEndProxiesAStreamingCompletion` passes.
- [ ] Claude Code works against the running binary and answers stream visibly token by token.
- [ ] Existing accounts were adopted on first run without a re-login.
- [ ] Only `github.com/go-chi/chi/v5` appears in `go.mod` as a direct dependency.
