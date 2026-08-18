# aiproxy Stage 2 — Accounting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record every request's token usage and cost to SQLite, correctly, so usage can be queried and graphed by model, account, outcome and interval.

**Architecture:** The relay already parses usage off the wire but discards it. A per-request accumulator turns provider events into one correct total, the attempt loop carries those totals on `Result`, and a non-blocking ingestion channel batches rows into SQLite. A background aggregator maintains minute and hour rollups so long-window queries stay cheap, and a query layer exposes the shapes both future front-ends will read.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go, no cgo), `github.com/go-chi/chi/v5` (already present).

**Spec:** `docs/superpowers/specs/2026-08-17-aiproxy-design.md` — read §7 in full before starting, plus §2 invariant 3 and §13. The spec is the authority; where this plan and the spec disagree, the spec wins and the plan is wrong.

## Global Constraints

- Module path `github.com/nicko170/aiproxy`. Go 1.26+.
- Dependencies for this stage: add **`modernc.org/sqlite` only**. No cgo — `CGO_ENABLED=0 go build ./...` must succeed and CI cross-builds darwin/linux × amd64/arm64.
- **Accounting must never block, delay, or fail a proxied request** (spec §2 invariant 3). Under pressure it drops samples and counts the drops.
- Timestamps are unix **milliseconds** as `int64`.
- Never log or persist a token, API key, or `Authorization` value.
- The database lives at `<config dir>/metrics.db`, mode `0600`, in a directory at `0700`.
- `OutcomeKind` is persisted and append-only. Never renumber an existing value.
- All query access is via `internal/metrics` and later `view.Source` — no other package opens the database.
- Both stage-1 invariants stay green: `TestAttemptBoundsTotalWaitOnHeaderlessRateLimits`, `TestAttemptBoundsWaitWhenUpstreamWithholdsHeaders`, and `TestRelayStreamsIncrementallyWithoutBuffering` must pass and remain falsifiable throughout.
- Commit after every task, conventional-commit prefixes.

## File Structure

Created:

| File | Responsibility |
| --- | --- |
| `internal/proxy/usage.go` | Per-request usage accumulator (cumulative vs incremental) |
| `internal/metrics/schema.go` | DDL and forward-only migrations |
| `internal/metrics/store.go` | Open/configure SQLite (WAL, pragmas), lifecycle |
| `internal/metrics/sample.go` | `Sample` and `QuotaSample` value types |
| `internal/metrics/ingest.go` | Buffered channel, batching writer, drop counter |
| `internal/metrics/rollup.go` | Minute/hour aggregator |
| `internal/metrics/retention.go` | Pruning raw rows, never rollups |
| `internal/metrics/pricing.go` | Embedded model→price table, cost estimation |
| `internal/metrics/query.go` | `UsageSeries`, `Totals`, `LatencyPercentiles`, `AccountQuotaHistory` |

Modified:

| File | Change |
| --- | --- |
| `internal/provider/provider.go` | Split `OutcomeServerError`; add non-streaming usage parsing to the interface |
| `internal/provider/anthropic/anthropic.go` | Implement non-streaming usage parsing |
| `internal/proxy/relay.go` | Tee usage for non-streaming bodies too |
| `internal/proxy/attempt.go` | `Result` gains token fields; wire `OnUsage` to the accumulator |
| `internal/proxy/handler.go` | Pass the completed `Result` to the metrics sink |
| `internal/account/manager.go` | Emit `QuotaSample` when quota is observed |
| `internal/config/config.go` | `metrics.db` path helper |
| `cmd/aiproxy/main.go` | Open store, start ingester/rollup/retention, shut down cleanly |

Boundaries: `internal/metrics` imports neither `internal/proxy` nor `internal/account` — it takes plain value types, so it can be tested against an in-memory database with no HTTP anywhere.

---

### Task 1: The usage accumulator

The single easiest place in this stage to produce numbers that are plausible and wrong. Pure logic, no I/O, so it gets a thorough table test.

**Files:**
- Create: `internal/proxy/usage.go`
- Test: `internal/proxy/usage_test.go`

**Interfaces:**
- Consumes: `provider.UsageDelta`.
- Produces: `proxy.UsageAccumulator` with `func NewUsageAccumulator() *UsageAccumulator`, methods `Observe(d *provider.UsageDelta)`, `StartMessage()`, `Totals() UsageTotals`; and `proxy.UsageTotals{InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64}`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/usage_test.go`:

```go
package proxy

import (
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func d(in, out, cr, cw int64) *provider.UsageDelta {
	return &provider.UsageDelta{
		InputTokens: in, OutputTokens: out,
		CacheReadTokens: cr, CacheWriteTokens: cw,
	}
}

// THE trap this type exists for. message_delta reports output_tokens as a
// RUNNING TOTAL for the message, not a delta since the previous event. Summing
// them inflates output badly, and the longer the completion the worse it gets.
func TestAccumulatorTakesLastOutputNotTheSum(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(100, 1, 900, 5)) // message_start
	a.Observe(d(0, 10, 0, 0))    // message_delta, running total
	a.Observe(d(0, 250, 0, 0))   // message_delta, running total
	a.Observe(d(0, 812, 0, 0))   // message_delta, final running total

	got := a.Totals()
	if got.OutputTokens != 812 {
		t.Errorf("OutputTokens = %d, want 812 (the last running total, not 1+10+250+812=1073)",
			got.OutputTokens)
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.CacheReadTokens != 900 || got.CacheWriteTokens != 5 {
		t.Errorf("cache = read %d / write %d, want 900 / 5", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// A running total that goes backwards (retry, reordering) must not lower the
// count — take the high-water mark within a message.
func TestAccumulatorOutputNeverDecreasesWithinAMessage(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(0, 500, 0, 0))
	a.Observe(d(0, 300, 0, 0))

	if got := a.Totals().OutputTokens; got != 500 {
		t.Errorf("OutputTokens = %d, want 500 — a lower running total must not reduce the count", got)
	}
}

// Across messages, output ACCUMULATES: each message's final running total adds
// to the request's total. Only within one message is it a replacement.
func TestAccumulatorSumsAcrossMessages(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(100, 1, 0, 0))
	a.Observe(d(0, 40, 0, 0))
	a.StartMessage()
	a.Observe(d(50, 1, 0, 0))
	a.Observe(d(0, 60, 0, 0))

	got := a.Totals()
	if got.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100 (40 + 60)", got.OutputTokens)
	}
	if got.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150 (100 + 50)", got.InputTokens)
	}
}

// A non-streaming response reports one complete usage object with no
// message_start/message_delta split — one Observe must be recorded whole.
func TestAccumulatorHandlesASingleCompleteUsage(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(320, 77, 1200, 8))

	got := a.Totals()
	if got.InputTokens != 320 || got.OutputTokens != 77 ||
		got.CacheReadTokens != 1200 || got.CacheWriteTokens != 8 {
		t.Errorf("totals = %+v, want 320/77/1200/8", got)
	}
}

func TestAccumulatorZeroValueIsEmpty(t *testing.T) {
	if got := NewUsageAccumulator().Totals(); got != (UsageTotals{}) {
		t.Errorf("Totals() = %+v, want zero", got)
	}
}

// Observing without an explicit StartMessage must still record, so a provider
// that never emits a message boundary is not silently dropped.
func TestAccumulatorObserveWithoutStartMessageStillCounts(t *testing.T) {
	a := NewUsageAccumulator()
	a.Observe(d(10, 20, 0, 0))

	got := a.Totals()
	if got.InputTokens != 10 || got.OutputTokens != 20 {
		t.Errorf("totals = %+v, want 10/20", got)
	}
}

func TestAccumulatorIgnoresNilDelta(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(nil)
	if got := a.Totals(); got != (UsageTotals{}) {
		t.Errorf("Totals() = %+v, want zero after a nil observation", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestAccumulator -v`
Expected: FAIL to build — `undefined: NewUsageAccumulator`, `undefined: UsageTotals`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/proxy/usage.go`:

```go
package proxy

import "github.com/nicko170/aiproxy/internal/provider"

// UsageTotals is one request's complete token accounting.
type UsageTotals struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// UsageAccumulator folds a stream of provider usage observations into one
// correct total.
//
// The subtlety it exists for: a streamed message reports output_tokens as a
// RUNNING TOTAL for that message, re-sent with every delta — not an increment
// since the previous event. Summing observations therefore inflates output
// roughly in proportion to the number of deltas, so a long completion is wrong
// by a large factor while still looking like a believable number. Input and
// cache figures are the opposite: they arrive once per message and must be
// summed across messages.
//
// So: output is a high-water mark WITHIN a message and a sum ACROSS messages;
// input and cache are always sums.
//
// Not safe for concurrent use. One accumulator belongs to one request, and the
// relay observes from a single goroutine.
type UsageAccumulator struct {
	// settled holds messages already closed out.
	settled UsageTotals
	// currentOutput is the high-water running total for the message in flight.
	currentOutput int64
}

func NewUsageAccumulator() *UsageAccumulator { return &UsageAccumulator{} }

// StartMessage closes out the message in flight and begins a new one. Safe to
// call before the first observation.
func (a *UsageAccumulator) StartMessage() {
	a.settled.OutputTokens += a.currentOutput
	a.currentOutput = 0
}

// Observe records one usage report. Input and cache counts add; output replaces
// the running total when it advances.
func (a *UsageAccumulator) Observe(d *provider.UsageDelta) {
	if d == nil {
		return
	}
	a.settled.InputTokens += d.InputTokens
	a.settled.CacheReadTokens += d.CacheReadTokens
	a.settled.CacheWriteTokens += d.CacheWriteTokens

	// A running total that goes backwards (retry, reordering) must not reduce
	// the count.
	if d.OutputTokens > a.currentOutput {
		a.currentOutput = d.OutputTokens
	}
}

// Totals returns the request's accounting, including the message in flight.
func (a *UsageAccumulator) Totals() UsageTotals {
	out := a.settled
	out.OutputTokens += a.currentOutput
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/proxy/ -run TestAccumulator -v`
Expected: PASS, seven tests.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/usage.go internal/proxy/usage_test.go
git commit -m "feat(proxy): per-request usage accumulator

Output tokens arrive as a running total per message, not an increment,
so summing observations inflates long completions while still producing
a believable number. Output is a high-water mark within a message and a
sum across messages; input and cache counts always sum."
```

---

### Task 2: Capture usage from non-streaming responses

**Files:**
- Modify: `internal/provider/provider.go` (extend the `Provider` interface)
- Modify: `internal/provider/anthropic/anthropic.go`
- Test: `internal/provider/anthropic/usage_body_test.go`

**Interfaces:**
- Consumes: `provider.UsageDelta`.
- Produces: a new `Provider` method `ParseUsageBody(body []byte) (*UsageDelta, bool)`, implemented by `*anthropic.Anthropic`.

- [ ] **Step 1: Write the failing test**

Create `internal/provider/anthropic/usage_body_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/anthropic/ -run TestParseUsageBody -v`
Expected: FAIL to build — `p.ParseUsageBody undefined`.

- [ ] **Step 3: Add the method to the interface**

In `internal/provider/provider.go`, add to the `Provider` interface, immediately after `ParseUsage`:

```go
	// ParseUsageBody extracts token counts from a complete non-streaming
	// response body. A streamed response reports usage through ParseUsage
	// instead; covering only one shape silently loses every non-streaming
	// request's accounting.
	ParseUsageBody(body []byte) (*UsageDelta, bool)
```

- [ ] **Step 4: Implement it**

Append to `internal/provider/anthropic/anthropic.go`:

```go
// nonStreamingUsage is the envelope a complete (non-SSE) message response uses.
type nonStreamingUsage struct {
	Usage *usage `json:"usage"`
}

// ParseUsageBody extracts token counts from a complete response body.
func (a *Anthropic) ParseUsageBody(body []byte) (*provider.UsageDelta, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, false
	}
	var env nonStreamingUsage
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return nil, false
	}
	u := env.Usage
	return &provider.UsageDelta{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}, true
}
```

- [ ] **Step 5: Fix the other implementations of the interface**

Any test double implementing `provider.Provider` now fails to compile. Add the method to each (`internal/account/manager_test.go`'s `stubProvider` is the known one; search for others):

```go
func (s *stubProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool) { return nil, false }
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./... `
Expected: PASS across all packages.

- [ ] **Step 7: Commit**

```bash
git add internal/provider/ internal/account/manager_test.go
git commit -m "feat(provider): parse usage from non-streaming response bodies

The relay tees usage only for text/event-stream, so a JSON response's
usage object was never read and count_tokens plus any non-streaming call
contributed nothing to accounting."
```

---

### Task 3: Wire token capture end to end

Replaces the no-op `OnUsage` closure with real accounting that reaches `Result`.

**Files:**
- Modify: `internal/provider/provider.go` (`UsageDelta` gains `StartsMessage`)
- Modify: `internal/provider/anthropic/anthropic.go` (set it on `message_start`)
- Modify: `internal/proxy/relay.go` (capture non-streaming bodies)
- Modify: `internal/proxy/attempt.go` (`Result` token fields, accumulator wiring)
- Modify: `internal/proxy/handler.go` (log tokens)
- Test: `internal/proxy/usage_wiring_test.go`

**Interfaces:**
- Consumes: `proxy.UsageAccumulator`, `provider.UsageDelta`, `(*Anthropic).ParseUsageBody`.
- Produces: `provider.UsageDelta.StartsMessage bool`; `RelayOptions.ParseBody func([]byte) (*provider.UsageDelta, bool)`; `Result` gains `InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64`.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/usage_wiring_test.go`:

```go
package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// A streamed completion must land on Result with the LAST running output total,
// not the sum of the deltas.
func TestResultCarriesStreamedTokenTotals(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":" +
				"{\"input_tokens\":120,\"output_tokens\":1,\"cache_read_input_tokens\":4000," +
				"\"cache_creation_input_tokens\":16}}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":250}}\n\n"},
		},
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	got := h.last()
	if got.OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250 (last running total, not 1+40+250)", got.OutputTokens)
	}
	if got.InputTokens != 120 {
		t.Errorf("InputTokens = %d, want 120", got.InputTokens)
	}
	if got.CacheReadTokens != 4000 || got.CacheWriteTokens != 16 {
		t.Errorf("cache = %d/%d, want 4000/16", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// The gap this task closes: a non-streaming response previously contributed
// nothing at all.
func TestResultCarriesNonStreamingTokenTotals(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{"type":"message","content":[{"type":"text","text":"hi"}],
		        "usage":{"input_tokens":55,"output_tokens":12,
		                 "cache_read_input_tokens":700,"cache_creation_input_tokens":3}}`,
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	got := h.last()
	if got.InputTokens != 55 || got.OutputTokens != 12 ||
		got.CacheReadTokens != 700 || got.CacheWriteTokens != 3 {
		t.Errorf("tokens = %d/%d/%d/%d, want 55/12/700/3",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// A response with no usage at all must report zeros, not garbage.
func TestResultTokensAreZeroWhenUpstreamReportsNone(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"ok":true}`,
	})

	h.post()
	got := h.last()
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0", got.InputTokens, got.OutputTokens)
	}
}

// Streaming must still stream — capturing usage must not buffer the response.
func TestUsageCaptureDoesNotBufferAStream(t *testing.T) {
	const gap = 80 * time.Millisecond
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"},
			{Delay: gap, Data: "data: {\"type\":\"content_block_delta\"}\n\n"},
			{Delay: gap, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"},
		},
	})

	res, elapsed := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if elapsed < 2*gap {
		t.Errorf("completed in %v, faster than the upstream produced it — impossible unless buffered", elapsed)
	}
	if got := h.last().OutputTokens; got != 5 {
		t.Errorf("OutputTokens = %d, want 5", got)
	}
}
```

Add this helper to the existing `harness` in `internal/proxy/attempt_test.go` if it is not already present:

```go
func (h *harness) last() Result {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastRes
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run 'TestResultCarries|TestResultTokens|TestUsageCapture' -v`
Expected: FAIL — `Result` has no `InputTokens` field.

- [ ] **Step 3: Mark message starts on the provider side**

In `internal/provider/provider.go`, extend `UsageDelta`:

```go
type UsageDelta struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// StartsMessage is true for the event that opens a message. The accumulator
	// needs it because output_tokens is a running total scoped to one message:
	// without a boundary it cannot tell a new message's counter from a
	// continuation of the previous one.
	StartsMessage bool
}
```

In `internal/provider/anthropic/anthropic.go`'s `ParseUsage`, set it when the event type is `message_start`:

```go
		return &provider.UsageDelta{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
			StartsMessage:    ev.Type == "message_start",
		}, true
```

And in `ParseUsageBody`, a complete response is exactly one message:

```go
		StartsMessage: true,
```

- [ ] **Step 4: Capture non-streaming bodies in the relay**

In `internal/proxy/relay.go`, add to `RelayOptions`:

```go
	// ParseBody extracts usage from a complete non-streaming body. When set and
	// Streaming is false, Relay retains up to maxUsageCapture bytes and parses
	// once the body ends.
	ParseBody func(body []byte) (*provider.UsageDelta, bool)
```

Add the cap and the capture. Keep it bounded — a response body is not something to hold without a limit:

```go
// maxUsageCapture bounds how much of a non-streaming body is retained to read
// its usage envelope. Usage sits near the end of a message response, but a
// pathological body must not be held in memory without limit.
const maxUsageCapture = 1 << 20 // 1 MiB
```

In `Relay`, declare `var captured []byte` alongside `pending`, and inside the chunk branch, after the write and flush:

```go
			if !opts.Streaming && opts.ParseBody != nil && len(captured) < maxUsageCapture {
				room := maxUsageCapture - len(captured)
				if room > len(c.buf) {
					room = len(c.buf)
				}
				captured = append(captured, c.buf[:room]...)
			}
```

On the EOF path, next to `flushRemainingUsage(pending, opts)`, add:

```go
				flushCapturedBody(captured, opts)
```

and define:

```go
// flushCapturedBody parses a complete non-streaming body for usage once it has
// finished arriving.
func flushCapturedBody(captured []byte, opts RelayOptions) {
	if opts.Streaming || opts.ParseBody == nil || len(captured) == 0 {
		return
	}
	if d, ok := opts.ParseBody(captured); ok && opts.OnUsage != nil {
		opts.OnUsage(d)
	}
}
```

- [ ] **Step 5: Carry the totals on Result**

In `internal/proxy/attempt.go`, add to `Result`:

```go
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
```

In `relay`, replace the no-op `OnUsage` with the accumulator and copy the totals onto the result:

```go
	acc := NewUsageAccumulator()
	streaming := strings.Contains(upstreamRes.Header.Get("Content-Type"), "text/event-stream")
	n, err := Relay(ctx, w, upstreamRes.Body, RelayOptions{
		BodyIdle:   a.cfg.BodyIdle,
		Streaming:  streaming,
		ParseUsage: prov.ParseUsage,
		ParseBody:  prov.ParseUsageBody,
		OnUsage: func(d *provider.UsageDelta) {
			if d != nil && d.StartsMessage {
				acc.StartMessage()
			}
			acc.Observe(d)
		},
	})
	res.Bytes = n
	totals := acc.Totals()
	res.InputTokens = totals.InputTokens
	res.OutputTokens = totals.OutputTokens
	res.CacheReadTokens = totals.CacheReadTokens
	res.CacheWriteTokens = totals.CacheWriteTokens
```

Note the ordering: the totals are read **before** the error branch that panics with `http.ErrAbortHandler`, so a truncated stream still reports what it managed to consume.

- [ ] **Step 6: Show tokens in the request log**

In `cmd/aiproxy/main.go`'s `OnResult` handler, add the counts:

```go
			log.Info("request",
				"model", req.Model, "account", res.AccountID, "status", res.Status,
				"outcome", res.Outcome.String(), "attempts", res.Attempts,
				"ttfbMs", res.TTFBMS, "waitMs", res.WaitMS, "bytes", res.Bytes,
				"in", res.InputTokens, "out", res.OutputTokens,
				"cacheRead", res.CacheReadTokens, "cacheWrite", res.CacheWriteTokens)
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test -race ./... `
Expected: PASS. `TestRelayStreamsIncrementallyWithoutBuffering` must still pass unchanged — confirm it does, since this task touches the relay.

- [ ] **Step 8: Verify against the real API**

```bash
go build -o /tmp/aiproxy ./cmd/aiproxy && /tmp/aiproxy --log-level info
# another terminal:
ANTHROPIC_BASE_URL=http://127.0.0.1:3456 claude -p "Count from 1 to 20"
```

Confirm the request log line reports non-zero `in`, `out` and `cacheRead`, and that `out` is a believable answer length rather than an inflated one. Record the observed line in your report.

- [ ] **Step 9: Commit**

```bash
git add internal/provider/ internal/proxy/ cmd/aiproxy/main.go
git commit -m "feat(proxy): capture token usage onto Result

Replaces the no-op OnUsage closure. Streaming responses fold through the
accumulator, non-streaming bodies are captured under a 1 MiB cap and
parsed once complete, and the totals ride on Result so the log line and
the metrics row read the same numbers from one place."
```

---

### Task 4: Split `OutcomeServerError` before any rows exist

`OutcomeKind` is persisted and append-only. Splitting it after ingestion starts means migrating rows; now it is free.

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/proxy/attempt.go`
- Test: `internal/proxy/outcome_test.go`

**Interfaces:**
- Produces: `provider.OutcomeUpstreamError` and `provider.OutcomeAdmissionError`, appended after the existing kinds.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/outcome_test.go`:

```go
package proxy

import (
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

// The enum is persisted. Renumbering an existing value silently rewrites the
// meaning of every row already written.
func TestOutcomeKindNumberingIsStable(t *testing.T) {
	want := map[provider.OutcomeKind]int{
		provider.OutcomeOK:                 0,
		provider.OutcomeQuotaRejected:      1,
		provider.OutcomeThrottledWithHint:  2,
		provider.OutcomeThrottledNoHint:    3,
		provider.OutcomeCredentialStale:    4,
		provider.OutcomeCredentialRefused:  5,
		provider.OutcomeClientError:        6,
		provider.OutcomeServerError:        7,
		provider.OutcomeNoAccountReady:     8,
	}
	for kind, n := range want {
		if int(kind) != n {
			t.Errorf("%v = %d, want %d — existing values must never be renumbered", kind, int(kind), n)
		}
	}
	// New kinds append after the existing ones.
	if int(provider.OutcomeUpstreamError) <= 8 || int(provider.OutcomeAdmissionError) <= 8 {
		t.Error("new outcome kinds must be appended, not inserted")
	}
}

func TestOutcomeKindStringsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for k := provider.OutcomeOK; k <= provider.OutcomeAdmissionError; k++ {
		s := k.String()
		if s == "unknown" {
			t.Errorf("%d has no String() case", int(k))
		}
		if seen[s] {
			t.Errorf("duplicate outcome string %q", s)
		}
		seen[s] = true
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestOutcome -v`
Expected: FAIL to build — `undefined: provider.OutcomeUpstreamError`.

- [ ] **Step 3: Append the new kinds**

In `internal/provider/provider.go`, append to the `iota` block and extend `String()`:

```go
	// OutcomeUpstreamError is a transport-level failure reaching the upstream:
	// connection reset, TLS failure, or the per-attempt header timeout.
	OutcomeUpstreamError
	// OutcomeAdmissionError is a local failure before any request was sent.
	OutcomeAdmissionError
```

```go
	case OutcomeUpstreamError:
		return "upstream_error"
	case OutcomeAdmissionError:
		return "admission_error"
```

- [ ] **Step 4: Use them at the right sites**

In `internal/proxy/attempt.go`, replace the blanket `OutcomeServerError` assignments:

- the failed-send branch (transport error or per-attempt header timeout) sets `OutcomeUpstreamError`;
- the non-budget `Admit` failure sets `OutcomeAdmissionError`;
- `OutcomeServerError` is retained for a genuine upstream 5xx **response**, which is what its name means.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./... `
Expected: PASS. Existing tests asserting `server_error` on a header timeout now expect `upstream_error` — update those assertions, and say in your report which you changed and why. Do not weaken any other assertion in them.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/provider.go internal/proxy/
git commit -m "feat(provider): split upstream and admission failures from 5xx

OutcomeKind is persisted and append-only, so granularity has to be right
before rows exist. A transport failure, a local admission failure and an
upstream 5xx are three different operational stories and an outcome
breakdown must tell them apart."
```

---

### Task 5: The SQLite store and schema

**Files:**
- Create: `internal/metrics/schema.go`
- Create: `internal/metrics/store.go`
- Test: `internal/metrics/store_test.go`

**Interfaces:**
- Consumes: nothing from this repo.
- Produces: `metrics.Open(path string) (*Store, error)`, `metrics.OpenMemory() (*Store, error)`, `(*Store).DB() *sql.DB`, `(*Store).Close() error`, `metrics.SchemaVersion` constant.

- [ ] **Step 1: Add the dependency**

```bash
go get modernc.org/sqlite@latest
go mod tidy
```

- [ ] **Step 2: Write the failing test**

Create `internal/metrics/store_test.go`:

```go
package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchemaAndSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, table := range []string{"requests", "quota_samples", "usage_buckets", "schema_version"} {
		var name string
		err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}

	var mode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	var version int
	if err := s.DB().QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("schema version = %d, want %d", version, SchemaVersion)
	}
}

// The database holds a usage history; it must not be world-readable, and the
// directory must be created if absent.
func TestOpenEnforcesPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := filepath.Join(dir, "metrics.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("db perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

// Opening an existing database must be idempotent, not destructive.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.DB().Exec(
		`INSERT INTO requests(started_at, account_id, provider) VALUES (1, 'a', 'p')`); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	var n int
	if err := s2.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 — reopening must not recreate the schema", n)
	}
}

func TestOpenMemoryWorksForTests(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(
		`INSERT INTO requests(started_at, account_id, provider) VALUES (1, 'a', 'p')`); err != nil {
		t.Errorf("insert into in-memory store: %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/metrics/ -v`
Expected: FAIL to build — package does not exist.

- [ ] **Step 4: Write the schema**

Create `internal/metrics/schema.go`:

```go
package metrics

// SchemaVersion is bumped whenever migrations are appended. Migrations are
// forward-only and each must be safe to run against a database already at a
// later version (they are skipped), so an older binary never rewrites a newer
// schema.
const SchemaVersion = 1

// migrations are applied in order; index+1 is the version each produces.
var migrations = []string{
	`
CREATE TABLE IF NOT EXISTS requests (
  id             INTEGER PRIMARY KEY,
  started_at     INTEGER NOT NULL,
  duration_ms    INTEGER,
  ttfb_ms        INTEGER,
  account_id     TEXT NOT NULL,
  provider       TEXT NOT NULL,
  model          TEXT,
  upstream_model TEXT,
  session_id     TEXT,
  endpoint       TEXT,
  status         INTEGER,
  outcome        TEXT,
  stream         INTEGER,
  attempts       INTEGER,
  rotated        INTEGER,
  wait_ms        INTEGER,
  input_tokens        INTEGER,
  output_tokens       INTEGER,
  cache_read_tokens   INTEGER,
  cache_write_tokens  INTEGER,
  cost_micros    INTEGER
);
CREATE INDEX IF NOT EXISTS requests_started       ON requests(started_at);
CREATE INDEX IF NOT EXISTS requests_acct_started  ON requests(account_id, started_at);
CREATE INDEX IF NOT EXISTS requests_model_started ON requests(model, started_at);

CREATE TABLE IF NOT EXISTS quota_samples (
  at          INTEGER NOT NULL,
  account_id  TEXT    NOT NULL,
  bucket      TEXT    NOT NULL,
  utilization REAL,
  resets_at   INTEGER,
  PRIMARY KEY (at, account_id, bucket)
);

CREATE TABLE IF NOT EXISTS usage_buckets (
  bucket_start INTEGER NOT NULL,
  granularity  TEXT    NOT NULL,
  account_id   TEXT    NOT NULL,
  model        TEXT    NOT NULL,
  requests     INTEGER NOT NULL,
  input_tokens       INTEGER NOT NULL,
  output_tokens      INTEGER NOT NULL,
  cache_read_tokens  INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_micros        INTEGER,
  PRIMARY KEY (bucket_start, granularity, account_id, model)
);
`,
}
```

Note `usage_buckets.model` is `NOT NULL` with `''` standing for "unknown": it is part of the primary key, and SQLite treats NULLs in a primary key as distinct, which would silently create a new row per request instead of aggregating.

- [ ] **Step 5: Write the store**

Create `internal/metrics/store.go`:

```go
// Package metrics owns the accounting database: its schema, its ingestion
// path, its rollups and its queries. Nothing else opens the database.
package metrics

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store is the accounting database.
type Store struct {
	db *sql.DB
}

// Open creates or opens the database at path, creating its directory, applying
// migrations, and enforcing permissions.
func Open(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create metrics dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("chmod metrics dir: %w", err)
	}

	// WAL keeps readers (queries, the UI) from blocking the writer;
	// synchronous=NORMAL is the right trade for accounting data, which is
	// valuable but not worth an fsync per transaction. busy_timeout stops a
	// concurrent reader from turning into an immediate SQLITE_BUSY error.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	s, err := open(dsn)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		s.Close()
		return nil, fmt.Errorf("chmod metrics db: %w", err)
	}
	return s, nil
}

// OpenMemory returns an in-memory store for tests.
func OpenMemory() (*Store, error) {
	return open("file::memory:?cache=shared&_pragma=busy_timeout(5000)")
}

func open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open metrics db: %w", err)
	}
	// One writer. SQLite serializes writes anyway, and the ingester is
	// single-goroutine by design, so a larger pool only invites lock contention.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&current)
	if err == sql.ErrNoRows {
		if _, err := s.db.Exec(`INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return fmt.Errorf("seed schema_version: %w", err)
		}
		current = 0
	} else if err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	for i := current; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -v`
Expected: PASS, four tests.

- [ ] **Step 7: Confirm no cgo crept in**

Run: `CGO_ENABLED=0 go build ./...`
Expected: success. If this fails the wrong SQLite driver was pulled in.

- [ ] **Step 8: Commit**

```bash
git add internal/metrics/ go.mod go.sum
git commit -m "feat(metrics): SQLite store with forward-only migrations

WAL so queries never block the writer, one connection because the
ingester is single-goroutine and a larger pool only invites lock
contention, and 0600/0700 permissions on a file holding usage history."
```

---

### Task 6: Non-blocking ingestion

Invariant 3 lives here: accounting must never block, delay, or fail a proxied request.

**Files:**
- Create: `internal/metrics/sample.go`
- Create: `internal/metrics/ingest.go`
- Test: `internal/metrics/ingest_test.go`

**Interfaces:**
- Consumes: `metrics.Store`.
- Produces: `metrics.Sample`, `metrics.QuotaSample`, `metrics.NewIngester(s *Store, opts IngestOptions) *Ingester`, `IngestOptions{BufferSize int, FlushInterval time.Duration, BatchSize int}`, methods `Record(Sample)`, `RecordQuota(QuotaSample)`, `Dropped() int64`, `Flush() error`, `Close() error`.

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/ingest_test.go`:

```go
package metrics

import (
	"sync"
	"testing"
	"time"
)

func sample(at int64, acct, model string, in, out int64) Sample {
	return Sample{
		StartedAt: at, DurationMS: 10, TTFBMS: 5,
		AccountID: acct, Provider: "anthropic", Model: model,
		Endpoint: "/v1/messages", Status: 200, Outcome: "ok",
		Stream: true, Attempts: 1,
		InputTokens: in, OutputTokens: out,
	}
}

func drainInto(t *testing.T, s *Store, ing *Ingester) {
	t.Helper()
	if err := ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestRecordPersistsRows(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	ing.Record(sample(1000, "acct-1", "claude-opus-5", 100, 20))
	ing.Record(sample(2000, "acct-1", "claude-opus-5", 50, 10))
	drainInto(t, s, ing)

	var n int
	var totalIn, totalOut int64
	if err := s.DB().QueryRow(
		`SELECT count(*), sum(input_tokens), sum(output_tokens) FROM requests`).
		Scan(&n, &totalIn, &totalOut); err != nil {
		t.Fatal(err)
	}
	if n != 2 || totalIn != 150 || totalOut != 30 {
		t.Errorf("rows=%d in=%d out=%d, want 2/150/30", n, totalIn, totalOut)
	}
}

// Invariant 3: Record must never block, even when the writer cannot keep up.
// A full buffer drops and counts, it does not wait.
func TestRecordNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A tiny buffer and a writer that is not running: every Record after the
	// buffer fills must drop rather than block.
	ing := NewIngester(s, IngestOptions{BufferSize: 4, FlushInterval: time.Hour, BatchSize: 1000})
	defer ing.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			ing.Record(sample(int64(i), "a", "m", 1, 1))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked — accounting must never delay a proxied request")
	}

	if ing.Dropped() == 0 {
		t.Error("expected drops to be counted when the buffer overflows")
	}
}

func TestRecordQuotaPersistsSamples(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	ing.RecordQuota(QuotaSample{At: 1000, AccountID: "a", Bucket: "5h", Utilization: 0.42, ResetsAt: 9999})
	drainInto(t, s, ing)

	var util float64
	if err := s.DB().QueryRow(
		`SELECT utilization FROM quota_samples WHERE account_id='a' AND bucket='5h'`).Scan(&util); err != nil {
		t.Fatal(err)
	}
	if util != 0.42 {
		t.Errorf("utilization = %v, want 0.42", util)
	}
}

// The same (at, account, bucket) arriving twice must not fail the whole batch.
func TestRecordQuotaToleratesDuplicates(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	q := QuotaSample{At: 1000, AccountID: "a", Bucket: "5h", Utilization: 0.5}
	ing.RecordQuota(q)
	ing.RecordQuota(q)
	if err := ing.Flush(); err != nil {
		t.Fatalf("a duplicate quota sample must not fail the batch: %v", err)
	}

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM quota_samples`).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestConcurrentRecordersAllLand(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{BufferSize: 4096})
	defer ing.Close()

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ing.Record(sample(int64(w*1000+i), "a", "m", 1, 1))
			}
		}(w)
	}
	wg.Wait()
	drainInto(t, s, ing)

	var n int64
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n)
	if n+ing.Dropped() != writers*each {
		t.Errorf("persisted %d + dropped %d != %d recorded", n, ing.Dropped(), writers*each)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run 'TestRecord|TestConcurrent' -v`
Expected: FAIL to build — `undefined: NewIngester`.

- [ ] **Step 3: Write the sample types**

Create `internal/metrics/sample.go`:

```go
package metrics

// Sample is one completed request, ready to persist. Plain data: the metrics
// package depends on neither the proxy nor the account registry.
type Sample struct {
	StartedAt  int64 // unix ms
	DurationMS int64
	TTFBMS     int64
	WaitMS     int64

	AccountID     string
	Provider      string
	Model         string
	UpstreamModel string
	SessionID     string
	Endpoint      string

	Status   int
	Outcome  string
	Stream   bool
	Attempts int
	Rotated  bool

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	// CostMicros is nil when the model has no known price, so an unpriced model
	// records NULL rather than a plausible wrong number.
	CostMicros *int64
}

// QuotaSample is one observation of an account's quota window.
type QuotaSample struct {
	At          int64 // unix ms
	AccountID   string
	Bucket      string
	Utilization float64
	ResetsAt    int64
}
```

- [ ] **Step 4: Write the ingester**

Create `internal/metrics/ingest.go`:

```go
package metrics

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// IngestOptions tunes the writer. Zero values take defaults.
type IngestOptions struct {
	BufferSize    int
	FlushInterval time.Duration
	BatchSize     int
}

type entry struct {
	sample *Sample
	quota  *QuotaSample
}

// Ingester accepts samples without ever blocking the caller and writes them in
// batches on a single goroutine.
//
// Spec invariant 3: accounting must never block, delay, or fail a proxied
// request. That is why Record does a non-blocking send and increments a drop
// counter when the buffer is full. Dropping accounting data is a real cost, but
// it is a smaller one than adding latency to the request path, and a visible
// drop count is honest in a way that silent backpressure is not.
type Ingester struct {
	store   *Store
	ch      chan entry
	opts    IngestOptions
	dropped atomic.Int64

	// flushed signals the writer has drained everything queued before it.
	flushReq chan chan error

	closeOnce sync.Once
	done      chan struct{}
	stopped   chan struct{}
}

func NewIngester(s *Store, opts IngestOptions) *Ingester {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 4096
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 200 * time.Millisecond
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	ing := &Ingester{
		store:    s,
		ch:       make(chan entry, opts.BufferSize),
		opts:     opts,
		flushReq: make(chan chan error, 1),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go ing.run()
	return ing
}

// Record queues a completed request. It never blocks: a full buffer drops the
// sample and increments the drop counter.
func (i *Ingester) Record(s Sample) {
	select {
	case i.ch <- entry{sample: &s}:
	default:
		i.dropped.Add(1)
	}
}

// RecordQuota queues a quota observation under the same no-blocking contract.
func (i *Ingester) RecordQuota(q QuotaSample) {
	select {
	case i.ch <- entry{quota: &q}:
	default:
		i.dropped.Add(1)
	}
}

// Dropped is the number of samples discarded because the buffer was full.
func (i *Ingester) Dropped() int64 { return i.dropped.Load() }

// Flush blocks until everything queued at call time has been written.
func (i *Ingester) Flush() error {
	reply := make(chan error, 1)
	select {
	case i.flushReq <- reply:
	case <-i.stopped:
		return nil
	}
	select {
	case err := <-reply:
		return err
	case <-i.stopped:
		return nil
	}
}

// Close flushes and stops the writer.
func (i *Ingester) Close() error {
	var err error
	i.closeOnce.Do(func() {
		err = i.Flush()
		close(i.done)
		<-i.stopped
	})
	return err
}

func (i *Ingester) run() {
	defer close(i.stopped)
	ticker := time.NewTicker(i.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]entry, 0, i.opts.BatchSize)
	writeBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := i.write(batch)
		batch = batch[:0]
		return err
	}

	for {
		select {
		case <-i.done:
			i.drain(&batch)
			writeBatch()
			return

		case reply := <-i.flushReq:
			i.drain(&batch)
			reply <- writeBatch()

		case <-ticker.C:
			writeBatch()

		case e := <-i.ch:
			batch = append(batch, e)
			if len(batch) >= i.opts.BatchSize {
				writeBatch()
			}
		}
	}
}

// drain moves everything currently queued into the batch without waiting.
func (i *Ingester) drain(batch *[]entry) {
	for {
		select {
		case e := <-i.ch:
			*batch = append(*batch, e)
		default:
			return
		}
	}
}

func (i *Ingester) write(batch []entry) error {
	ctx := context.Background()
	tx, err := i.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metrics tx: %w", err)
	}
	defer tx.Rollback()

	reqStmt, err := tx.PrepareContext(ctx, `
INSERT INTO requests (started_at, duration_ms, ttfb_ms, wait_ms, account_id, provider,
  model, upstream_model, session_id, endpoint, status, outcome, stream, attempts, rotated,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare request insert: %w", err)
	}
	defer reqStmt.Close()

	// A repeated (at, account, bucket) is an ordinary consequence of polling on
	// a fixed interval; it must not fail the batch and take unrelated request
	// rows down with it.
	quotaStmt, err := tx.PrepareContext(ctx, `
INSERT OR REPLACE INTO quota_samples (at, account_id, bucket, utilization, resets_at)
VALUES (?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare quota insert: %w", err)
	}
	defer quotaStmt.Close()

	for _, e := range batch {
		switch {
		case e.sample != nil:
			s := e.sample
			var cost any
			if s.CostMicros != nil {
				cost = *s.CostMicros
			}
			if _, err := reqStmt.ExecContext(ctx,
				s.StartedAt, s.DurationMS, s.TTFBMS, s.WaitMS, s.AccountID, s.Provider,
				nullIfEmpty(s.Model), nullIfEmpty(s.UpstreamModel), nullIfEmpty(s.SessionID),
				nullIfEmpty(s.Endpoint), s.Status, s.Outcome, s.Stream, s.Attempts, s.Rotated,
				s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens, cost,
			); err != nil {
				return fmt.Errorf("insert request row: %w", err)
			}
		case e.quota != nil:
			q := e.quota
			if _, err := quotaStmt.ExecContext(ctx,
				q.At, q.AccountID, q.Bucket, q.Utilization, nullIfZero(q.ResetsAt),
			); err != nil {
				return fmt.Errorf("insert quota row: %w", err)
			}
		}
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
```

Do not import `database/sql` here — this file names none of its types. If the
compiler reports it unused, remove the import rather than adding a blank
reference to keep it.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -v`
Expected: PASS, all store and ingest tests.

- [ ] **Step 6: Commit**

```bash
git add internal/metrics/
git commit -m "feat(metrics): non-blocking batched ingestion

Record does a non-blocking send and counts drops, because accounting must
never add latency to a proxied request. Duplicate quota samples upsert
rather than failing the batch and taking unrelated request rows with them."
```

---

### Task 7: Rollups

**Files:**
- Create: `internal/metrics/rollup.go`
- Test: `internal/metrics/rollup_test.go`

**Interfaces:**
- Consumes: `metrics.Store`.
- Produces: `metrics.Granularity` (`GranularityMinute`, `GranularityHour`), `metrics.RollupOnce(ctx context.Context, s *Store, now time.Time, lookback time.Duration) error`, `metrics.NewRoller(s *Store, interval time.Duration, log *slog.Logger) *Roller` with `Start()`/`Stop()`.

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/rollup_test.go`:

```go
package metrics

import (
	"context"
	"testing"
	"time"
)

func insertRaw(t *testing.T, s *Store, at int64, acct, model string, in, out, cr, cw int64, cost *int64) {
	t.Helper()
	var c any
	if cost != nil {
		c = *cost
	}
	_, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, model, status, outcome,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		at, acct, "anthropic", model, 200, "ok", in, out, cr, cw, c)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollupAggregatesByMinuteAndHour(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC).UnixMilli()
	insertRaw(t, s, base+1000, "a", "opus", 100, 10, 5, 1, nil)
	insertRaw(t, s, base+2000, "a", "opus", 200, 20, 5, 1, nil)
	insertRaw(t, s, base+61000, "a", "opus", 7, 3, 0, 0, nil) // next minute, same hour

	now := time.UnixMilli(base + 120000)
	if err := RollupOnce(context.Background(), s, now, time.Hour); err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}

	var reqs, in, out int64
	err := s.DB().QueryRow(`
SELECT requests, input_tokens, output_tokens FROM usage_buckets
WHERE granularity='minute' AND bucket_start=? AND account_id='a' AND model='opus'`,
		base).Scan(&reqs, &in, &out)
	if err != nil {
		t.Fatalf("minute bucket: %v", err)
	}
	if reqs != 2 || in != 300 || out != 30 {
		t.Errorf("minute bucket = %d reqs / %d in / %d out, want 2/300/30", reqs, in, out)
	}

	err = s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='hour' AND bucket_start=? AND account_id='a' AND model='opus'`,
		base).Scan(&reqs, &in)
	if err != nil {
		t.Fatalf("hour bucket: %v", err)
	}
	if reqs != 3 || in != 307 {
		t.Errorf("hour bucket = %d reqs / %d in, want 3/307", reqs, in)
	}
}

// Running twice must not double-count. The aggregator recomputes a bucket from
// the raw rows rather than incrementing it, which is what makes it safe to run
// on a timer, after a crash, or twice by accident.
func TestRollupIsIdempotent(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC).UnixMilli()
	insertRaw(t, s, base+500, "a", "opus", 50, 5, 0, 0, nil)

	now := time.UnixMilli(base + 60000)
	for i := 0; i < 3; i++ {
		if err := RollupOnce(context.Background(), s, now, time.Hour); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var reqs, in int64
	s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model='opus'`).Scan(&reqs, &in)
	if reqs != 1 || in != 50 {
		t.Errorf("after 3 runs: %d reqs / %d in, want 1/50 — rollup is not idempotent", reqs, in)
	}
}

// A request whose model was never identified must still be accounted for, not
// silently vanish into a NULL primary-key column.
func TestRollupCountsRowsWithNoModel(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
	_, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, status, outcome,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES (?,?,?,?,?,?,?,?,?)`, base+100, "a", "anthropic", 200, "ok", 9, 4, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}

	var reqs, in int64
	if err := s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model=''`).Scan(&reqs, &in); err != nil {
		t.Fatalf("a row with no model should aggregate under '': %v", err)
	}
	if reqs != 1 || in != 9 {
		t.Errorf("= %d reqs / %d in, want 1/9", reqs, in)
	}
}

func TestRollupSumsCostAndToleratesUnpriced(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC).UnixMilli()
	priced := int64(1500)
	insertRaw(t, s, base+100, "a", "opus", 1, 1, 0, 0, &priced)
	insertRaw(t, s, base+200, "a", "opus", 1, 1, 0, 0, nil) // unpriced

	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}

	var cost int64
	if err := s.DB().QueryRow(`
SELECT cost_micros FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model='opus'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 1500 {
		t.Errorf("cost = %d, want 1500 — an unpriced row contributes nothing, it does not void the bucket", cost)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run TestRollup -v`
Expected: FAIL to build — `undefined: RollupOnce`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/metrics/rollup.go`:

```go
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Granularity names a rollup grain.
type Granularity string

const (
	GranularityMinute Granularity = "minute"
	GranularityHour   Granularity = "hour"
)

func (g Granularity) millis() int64 {
	if g == GranularityHour {
		return 3600_000
	}
	return 60_000
}

// RollupOnce recomputes every bucket touched in the lookback window.
//
// Recompute, not increment: each bucket is derived in full from the raw rows it
// covers and written with INSERT OR REPLACE. That is what makes it safe to run
// on a timer, twice by accident, or again after a crash mid-write — an
// incrementing aggregator has none of those properties and drifts silently.
func RollupOnce(ctx context.Context, s *Store, now time.Time, lookback time.Duration) error {
	if lookback <= 0 {
		lookback = time.Hour
	}
	from := now.Add(-lookback).UnixMilli()
	to := now.UnixMilli()

	for _, g := range []Granularity{GranularityMinute, GranularityHour} {
		if err := rollupGrain(ctx, s, g, from, to); err != nil {
			return fmt.Errorf("rollup %s: %w", g, err)
		}
	}
	return nil
}

func rollupGrain(ctx context.Context, s *Store, g Granularity, from, to int64) error {
	span := g.millis()
	// Widen to whole buckets so a partially covered bucket is recomputed in
	// full rather than truncated to the window.
	start := (from / span) * span
	end := (to/span)*span + span

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// coalesce(model,'') because model is part of the primary key and SQLite
	// treats NULLs in a key as distinct — a NULL model would create a fresh row
	// per request instead of aggregating.
	_, err = tx.ExecContext(ctx, `
INSERT OR REPLACE INTO usage_buckets
  (bucket_start, granularity, account_id, model, requests,
   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
SELECT
  (started_at / ?) * ?            AS bucket_start,
  ?                               AS granularity,
  account_id,
  coalesce(model, '')             AS model,
  count(*)                        AS requests,
  coalesce(sum(input_tokens), 0),
  coalesce(sum(output_tokens), 0),
  coalesce(sum(cache_read_tokens), 0),
  coalesce(sum(cache_write_tokens), 0),
  sum(cost_micros)
FROM requests
WHERE started_at >= ? AND started_at < ?
GROUP BY bucket_start, account_id, coalesce(model, '')`,
		span, span, string(g), start, end)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Roller runs RollupOnce on a timer.
type Roller struct {
	store    *Store
	interval time.Duration
	log      *slog.Logger
	stop     chan struct{}
	stopped  chan struct{}
}

func NewRoller(s *Store, interval time.Duration, log *slog.Logger) *Roller {
	if interval <= 0 {
		interval = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Roller{
		store: s, interval: interval, log: log,
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func (r *Roller) Start() {
	go func() {
		defer close(r.stopped)
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case now := <-t.C:
				// Look back further than the interval so a bucket straddling a
				// tick, or one missed while the process was down, is still
				// recomputed.
				if err := RollupOnce(context.Background(), r.store, now, 2*time.Hour); err != nil {
					r.log.Warn("rollup failed", "err", err)
				}
			}
		}
	}()
}

func (r *Roller) Stop() {
	close(r.stop)
	<-r.stopped
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -run TestRollup -v`
Expected: PASS, four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/rollup.go internal/metrics/rollup_test.go
git commit -m "feat(metrics): idempotent minute and hour rollups

Each bucket is recomputed in full from its raw rows rather than
incremented, so running on a timer, twice, or again after a crash all
converge on the same answer."
```

---

### Task 8: Retention

**Files:**
- Create: `internal/metrics/retention.go`
- Test: `internal/metrics/retention_test.go`

**Interfaces:**
- Produces: `metrics.PruneOnce(ctx context.Context, s *Store, now time.Time, retain time.Duration) (int64, error)`, `metrics.NewPruner(s *Store, retain time.Duration, log *slog.Logger) *Pruner` with `Start()`/`Stop()`.

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/retention_test.go`:

```go
package metrics

import (
	"context"
	"testing"
	"time"
)

func TestPruneRemovesOldRawRowsButKeepsRollups(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-1 * time.Hour).UnixMilli()

	insertRaw(t, s, old, "a", "opus", 10, 1, 0, 0, nil)
	insertRaw(t, s, recent, "a", "opus", 20, 2, 0, 0, nil)
	if err := RollupOnce(context.Background(), s, now, 200*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var bucketsBefore int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&bucketsBefore)

	deleted, err := PruneOnce(context.Background(), s, now, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	var raw int
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&raw)
	if raw != 1 {
		t.Errorf("raw rows = %d, want 1 (the recent one)", raw)
	}

	var bucketsAfter int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&bucketsAfter)
	if bucketsAfter != bucketsBefore {
		t.Errorf("rollups = %d, want %d — rollups are never pruned; they are what makes a long window cheap",
			bucketsAfter, bucketsBefore)
	}
}

func TestPruneAlsoTrimsQuotaSamples(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization) VALUES (?,?,?,?)`,
		now.Add(-100*24*time.Hour).UnixMilli(), "a", "5h", 0.5)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization) VALUES (?,?,?,?)`,
		now.Add(-time.Hour).UnixMilli(), "a", "5h", 0.6)

	if _, err := PruneOnce(context.Background(), s, now, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM quota_samples`).Scan(&n)
	if n != 1 {
		t.Errorf("quota samples = %d, want 1", n)
	}
}

// Retention of zero or less means keep everything — an operator who clears the
// setting must not silently lose their history.
func TestPruneWithZeroRetentionKeepsEverything(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	insertRaw(t, s, now.Add(-1000*24*time.Hour).UnixMilli(), "a", "opus", 1, 1, 0, 0, nil)

	deleted, err := PruneOnce(context.Background(), s, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 when retention is disabled", deleted)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run TestPrune -v`
Expected: FAIL to build — `undefined: PruneOnce`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/metrics/retention.go`:

```go
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PruneOnce deletes raw rows older than the retention window and returns how
// many request rows went.
//
// Rollups are never pruned. They are tiny and they are what makes a 90-day
// query cheap; deleting them would trade the only cheap path for disk space
// that was never the problem. Raw rows exist for drill-down, which has a much
// shorter useful life.
//
// Ordering matters operationally: rollups run every minute and retention runs
// daily, so by the time a row is old enough to prune it has long since been
// aggregated. A retention window shorter than the rollup interval would lose
// data, which is why retain is measured in days.
func PruneOnce(ctx context.Context, s *Store, now time.Time, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil // retention disabled: keep everything
	}
	cutoff := now.Add(-retain).UnixMilli()

	res, err := s.DB().ExecContext(ctx, `DELETE FROM requests WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	deleted, _ := res.RowsAffected()

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM quota_samples WHERE at < ?`, cutoff); err != nil {
		return deleted, fmt.Errorf("prune quota samples: %w", err)
	}
	return deleted, nil
}

// Pruner runs PruneOnce on a timer.
type Pruner struct {
	store   *Store
	retain  time.Duration
	log     *slog.Logger
	stop    chan struct{}
	stopped chan struct{}
}

func NewPruner(s *Store, retain time.Duration, log *slog.Logger) *Pruner {
	if log == nil {
		log = slog.Default()
	}
	return &Pruner{
		store: s, retain: retain, log: log,
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func (p *Pruner) Start() {
	go func() {
		defer close(p.stopped)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-t.C:
				n, err := PruneOnce(context.Background(), p.store, now, p.retain)
				if err != nil {
					p.log.Warn("retention prune failed", "err", err)
					continue
				}
				if n > 0 {
					p.log.Info("pruned raw request rows", "deleted", n,
						"retainDays", int(p.retain.Hours()/24))
				}
			}
		}
	}()
}

func (p *Pruner) Stop() {
	close(p.stop)
	<-p.stopped
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -run TestPrune -v`
Expected: PASS, three tests.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/retention.go internal/metrics/retention_test.go
git commit -m "feat(metrics): retention prunes raw rows and never rollups

Rollups are what make a long window cheap; deleting them would trade the
only fast path for disk space that was never the constraint. Zero
retention keeps everything rather than silently discarding history."
```

---

### Task 9: Pricing and cost estimation

**Files:**
- Create: `internal/metrics/pricing.go`
- Test: `internal/metrics/pricing_test.go`

**Interfaces:**
- Produces: `metrics.Price{InputPerMTok, OutputPerMTok, CacheReadPerMTok, CacheWritePerMTok float64}`, `metrics.PriceFor(model string) (Price, bool)`, `metrics.CostMicros(model string, t TokenCounts) *int64`, `metrics.TokenCounts{Input, Output, CacheRead, CacheWrite int64}`.

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/pricing_test.go`:

```go
package metrics

import "testing"

func TestCostMicrosIsNilForAnUnknownModel(t *testing.T) {
	if got := CostMicros("some-model-we-have-never-seen", TokenCounts{Input: 1000}); got != nil {
		t.Errorf("cost = %v, want nil — an unpriced model must record NULL, never a plausible wrong number", *got)
	}
}

func TestCostMicrosChargesEachTokenClassSeparately(t *testing.T) {
	// A model priced at $3/MTok in, $15/MTok out, $0.30/MTok cache read,
	// $3.75/MTok cache write.
	p := Price{InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75}
	got := costFromPrice(p, TokenCounts{Input: 1_000_000, Output: 1_000_000,
		CacheRead: 1_000_000, CacheWrite: 1_000_000})

	// 3 + 15 + 0.30 + 3.75 = 22.05 dollars = 22_050_000 micros
	if got != 22_050_000 {
		t.Errorf("cost = %d micros, want 22050000", got)
	}
}

// Cache reads dominate under an agent workload; charging them at the input rate
// would overstate cost by an order of magnitude.
func TestCacheReadIsNotChargedAtTheInputRate(t *testing.T) {
	p := Price{InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75}
	cacheHeavy := costFromPrice(p, TokenCounts{Input: 1000, CacheRead: 1_000_000})
	asIfInput := costFromPrice(p, TokenCounts{Input: 1_001_000})

	if cacheHeavy >= asIfInput {
		t.Errorf("cache-heavy cost %d should be far below the input-rate cost %d", cacheHeavy, asIfInput)
	}
}

func TestPriceForMatchesKnownModelsIncludingDatedVariants(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "claude-sonnet-4-5-20250929"} {
		if _, ok := PriceFor(model); !ok {
			t.Errorf("PriceFor(%q) = not found; the embedded table should cover current models", model)
		}
	}
}

func TestCostMicrosIsZeroForZeroTokens(t *testing.T) {
	got := CostMicros("claude-opus-5", TokenCounts{})
	if got == nil {
		t.Fatal("a known model with zero tokens should cost 0, not NULL")
	}
	if *got != 0 {
		t.Errorf("cost = %d, want 0", *got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run 'TestCost|TestPrice|TestCache' -v`
Expected: FAIL to build — `undefined: CostMicros`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/metrics/pricing.go`:

```go
package metrics

import "strings"

// Price is a model's rate card in US dollars per million tokens.
type Price struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// TokenCounts is one request's usage.
type TokenCounts struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// prices is a small embedded table keyed by a model-family prefix, so a dated
// variant (claude-sonnet-4-5-20250929) matches its family without needing an
// entry per release. Verify these against current published pricing before
// relying on the figures; they are an estimate, and the schema records NULL
// rather than a guess when a model is absent.
var prices = map[string]Price{
	"claude-opus-5":    {InputPerMTok: 15, OutputPerMTok: 75, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
	"claude-opus-4":    {InputPerMTok: 15, OutputPerMTok: 75, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
	"claude-sonnet-5":  {InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-sonnet-4":  {InputPerMTok: 3, OutputPerMTok: 15, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-haiku-4-5": {InputPerMTok: 1, OutputPerMTok: 5, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},
	"claude-fable-5":   {InputPerMTok: 5, OutputPerMTok: 25, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
}

// PriceFor resolves a model name to a rate card, matching the longest known
// prefix so a dated variant resolves to its family.
func PriceFor(model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	best, bestLen, found := Price{}, 0, false
	for prefix, p := range prices {
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			best, bestLen, found = p, len(prefix), true
		}
	}
	return best, found
}

// CostMicros estimates a request's cost in millionths of a dollar, or nil when
// the model has no known price.
//
// nil is deliberate: an unpriced model must record NULL rather than 0, because
// a zero would silently understate a cost total and look like a real answer.
func CostMicros(model string, t TokenCounts) *int64 {
	p, ok := PriceFor(model)
	if !ok {
		return nil
	}
	v := costFromPrice(p, t)
	return &v
}

// costFromPrice charges each token class at its own rate. Cache reads are
// roughly a tenth of the input rate and dominate agent workloads by volume, so
// folding them into the input count would overstate cost by an order of
// magnitude.
func costFromPrice(p Price, t TokenCounts) int64 {
	const perMTok = 1_000_000.0
	dollars := float64(t.Input)/perMTok*p.InputPerMTok +
		float64(t.Output)/perMTok*p.OutputPerMTok +
		float64(t.CacheRead)/perMTok*p.CacheReadPerMTok +
		float64(t.CacheWrite)/perMTok*p.CacheWritePerMTok
	return int64(dollars * 1_000_000)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -v`
Expected: PASS, all metrics tests.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/pricing.go internal/metrics/pricing_test.go
git commit -m "feat(metrics): cost estimation with per-class token rates

Cache reads are charged at their own rate, not the input rate — they
dominate agent workloads by volume and folding them in overstates cost by
an order of magnitude. An unknown model records NULL, never zero."
```

---

### Task 10: The query surface

**Files:**
- Create: `internal/metrics/query.go`
- Test: `internal/metrics/query_test.go`

**Interfaces:**
- Produces: `metrics.Window{From, To int64}`, `metrics.SeriesQuery{Window, Granularity, GroupBy}`, `metrics.GroupBy` (`GroupByAccount`, `GroupByModel`, `GroupByOutcome`), `metrics.Point`, `metrics.Series`, `metrics.Totals`, `metrics.Latency`, `metrics.QuotaPoint`; methods `(*Store).UsageSeries(ctx, SeriesQuery) (Series, error)`, `(*Store).Totals(ctx, Window) (Totals, error)`, `(*Store).LatencyPercentiles(ctx, Window) (Latency, error)`, `(*Store).AccountQuotaHistory(ctx, accountID string, w Window) ([]QuotaPoint, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/metrics/query_test.go`:

```go
package metrics

import (
	"context"
	"testing"
	"time"
)

func seedForQueries(t *testing.T, s *Store, base int64) {
	t.Helper()
	insertRaw(t, s, base+1000, "acct-a", "claude-opus-5", 100, 10, 500, 5, nil)
	insertRaw(t, s, base+2000, "acct-a", "claude-sonnet-5", 200, 20, 0, 0, nil)
	insertRaw(t, s, base+3000, "acct-b", "claude-opus-5", 300, 30, 0, 0, nil)
	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSeriesGroupsByModel(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base, To: base + 60000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByModel,
	})
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}

	byKey := map[string]int64{}
	for _, p := range got.Points {
		byKey[p.Key] += p.InputTokens
	}
	if byKey["claude-opus-5"] != 400 {
		t.Errorf("opus input = %d, want 400", byKey["claude-opus-5"])
	}
	if byKey["claude-sonnet-5"] != 200 {
		t.Errorf("sonnet input = %d, want 200", byKey["claude-sonnet-5"])
	}
}

func TestUsageSeriesGroupsByAccount(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base, To: base + 60000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int64{}
	for _, p := range got.Points {
		byKey[p.Key] += p.Requests
	}
	if byKey["acct-a"] != 2 || byKey["acct-b"] != 1 {
		t.Errorf("requests by account = %v, want acct-a 2 / acct-b 1", byKey)
	}
}

// The window must actually bound the result, or every chart silently shows all
// of history.
func TestUsageSeriesRespectsTheWindow(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base + 3_600_000, To: base + 7_200_000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 0 {
		t.Errorf("got %d points outside the window, want 0", len(got.Points))
	}
}

func TestTotalsSumsTheWindow(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.Totals(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 3 || got.InputTokens != 600 || got.OutputTokens != 60 {
		t.Errorf("totals = %+v, want 3 requests / 600 in / 60 out", got)
	}
	if got.CacheReadTokens != 500 {
		t.Errorf("cache read = %d, want 500", got.CacheReadTokens)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	// ttfb 10..1000 so p50 and p95 are clearly distinguishable.
	for i := 1; i <= 100; i++ {
		if _, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms, duration_ms)
VALUES (?,?,?,?,?,?,?)`, base+int64(i), "a", "anthropic", 200, "ok", i*10, i*20); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.LatencyPercentiles(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFBP50 < 400 || got.TTFBP50 > 600 {
		t.Errorf("TTFB p50 = %d, want ~500", got.TTFBP50)
	}
	if got.TTFBP95 < 900 || got.TTFBP95 > 1000 {
		t.Errorf("TTFB p95 = %d, want ~950", got.TTFBP95)
	}
	if got.TTFBP95 <= got.TTFBP50 {
		t.Error("p95 must exceed p50")
	}
}

// A request that never produced a first byte records ttfb_ms = -1; including it
// would drag the percentile toward zero and hide a real latency problem.
func TestLatencyPercentilesIgnoreRequestsWithNoFirstByte(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	s.DB().Exec(`INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms)
	             VALUES (?,?,?,?,?,?)`, base+1, "a", "anthropic", 200, "ok", 400)
	s.DB().Exec(`INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms)
	             VALUES (?,?,?,?,?,?)`, base+2, "a", "anthropic", 429, "throttled_no_hint", -1)

	got, err := s.LatencyPercentiles(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFBP50 != 400 {
		t.Errorf("p50 = %d, want 400 — a request with no first byte must not be counted", got.TTFBP50)
	}
}

func TestAccountQuotaHistory(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+1000, "acct-a", "5h", 0.10, base+9999)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+2000, "acct-a", "5h", 0.35, base+9999)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+3000, "acct-b", "5h", 0.90, base+9999)

	got, err := s.AccountQuotaHistory(context.Background(), "acct-a", Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d points, want 2 (only acct-a)", len(got))
	}
	if got[0].At > got[1].At {
		t.Error("history must be ordered oldest first")
	}
	if got[1].Utilization != 0.35 {
		t.Errorf("latest utilization = %v, want 0.35", got[1].Utilization)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run 'TestUsageSeries|TestTotals|TestLatency|TestAccountQuota' -v`
Expected: FAIL to build — `undefined: SeriesQuery`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/metrics/query.go`:

```go
package metrics

import (
	"context"
	"database/sql"
	"fmt"
)

// Window is a closed-open time range in unix ms.
type Window struct {
	From int64
	To   int64
}

// GroupBy names the dimension a series is split along.
type GroupBy string

const (
	GroupByAccount GroupBy = "account"
	GroupByModel   GroupBy = "model"
	GroupByOutcome GroupBy = "outcome"
)

type SeriesQuery struct {
	Window      Window
	Granularity Granularity
	GroupBy     GroupBy
}

// Point is one bucket of one series.
type Point struct {
	BucketStart      int64
	Key              string
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
}

type Series struct {
	Granularity Granularity
	GroupBy     GroupBy
	Points      []Point
}

type Totals struct {
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
	// UnpricedRequests is how many rows had no known price, so a cost total is
	// never presented as complete when it is not.
	UnpricedRequests int64
}

type Latency struct {
	TTFBP50     int64
	TTFBP95     int64
	DurationP50 int64
	DurationP95 int64
}

type QuotaPoint struct {
	At          int64
	Bucket      string
	Utilization float64
	ResetsAt    int64
}

// UsageSeries reads rollups, not raw rows — that is what keeps a 90-day query
// cheap. Grouping by outcome falls back to raw rows, since outcome is not a
// rollup dimension (adding it would multiply bucket cardinality for a breakdown
// that is only ever read over short windows).
func (s *Store) UsageSeries(ctx context.Context, q SeriesQuery) (Series, error) {
	out := Series{Granularity: q.Granularity, GroupBy: q.GroupBy}
	if q.Granularity == "" {
		q.Granularity = GranularityHour
		out.Granularity = q.Granularity
	}

	var query string
	switch q.GroupBy {
	case GroupByAccount:
		query = `
SELECT bucket_start, account_id, sum(requests), sum(input_tokens), sum(output_tokens),
       sum(cache_read_tokens), sum(cache_write_tokens), coalesce(sum(cost_micros), 0)
FROM usage_buckets
WHERE granularity = ? AND bucket_start >= ? AND bucket_start < ?
GROUP BY bucket_start, account_id
ORDER BY bucket_start, account_id`
	case GroupByModel:
		query = `
SELECT bucket_start, model, sum(requests), sum(input_tokens), sum(output_tokens),
       sum(cache_read_tokens), sum(cache_write_tokens), coalesce(sum(cost_micros), 0)
FROM usage_buckets
WHERE granularity = ? AND bucket_start >= ? AND bucket_start < ?
GROUP BY bucket_start, model
ORDER BY bucket_start, model`
	case GroupByOutcome:
		span := q.Granularity.millis()
		query = fmt.Sprintf(`
SELECT (started_at / %d) * %d, outcome, count(*), coalesce(sum(input_tokens),0),
       coalesce(sum(output_tokens),0), coalesce(sum(cache_read_tokens),0),
       coalesce(sum(cache_write_tokens),0), coalesce(sum(cost_micros),0)
FROM requests
WHERE started_at >= ? AND started_at < ?
GROUP BY 1, outcome
ORDER BY 1, outcome`, span, span)
	default:
		return out, fmt.Errorf("unknown group-by %q", q.GroupBy)
	}

	var rows *sql.Rows
	var err error
	if q.GroupBy == GroupByOutcome {
		rows, err = s.db.QueryContext(ctx, query, q.Window.From, q.Window.To)
	} else {
		rows, err = s.db.QueryContext(ctx, query, string(q.Granularity), q.Window.From, q.Window.To)
	}
	if err != nil {
		return out, fmt.Errorf("usage series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.BucketStart, &p.Key, &p.Requests, &p.InputTokens,
			&p.OutputTokens, &p.CacheReadTokens, &p.CacheWriteTokens, &p.CostMicros); err != nil {
			return out, fmt.Errorf("scan usage point: %w", err)
		}
		out.Points = append(out.Points, p)
	}
	return out, rows.Err()
}

// Totals reads raw rows so it can also report how many were unpriced.
func (s *Store) Totals(ctx context.Context, w Window) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx, `
SELECT count(*),
       coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
       coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_write_tokens),0),
       coalesce(sum(cost_micros),0),
       sum(CASE WHEN cost_micros IS NULL THEN 1 ELSE 0 END)
FROM requests
WHERE started_at >= ? AND started_at < ?`, w.From, w.To).
		Scan(&t.Requests, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens,
			&t.CacheWriteTokens, &t.CostMicros, &t.UnpricedRequests)
	if err != nil {
		return t, fmt.Errorf("totals: %w", err)
	}
	return t, nil
}

// LatencyPercentiles computes p50/p95 by offset. Requests that never produced a
// first byte record ttfb_ms = -1 and are excluded: counting them would drag the
// percentile toward zero and hide the very latency problem it exists to show.
func (s *Store) LatencyPercentiles(ctx context.Context, w Window) (Latency, error) {
	var l Latency
	pick := func(column string, pct float64) (int64, error) {
		var n int64
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT count(*) FROM requests WHERE started_at >= ? AND started_at < ? AND %s >= 0`, column),
			w.From, w.To).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
		offset := int64(float64(n-1) * pct)
		var v int64
		err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT %s FROM requests
WHERE started_at >= ? AND started_at < ? AND %s >= 0
ORDER BY %s LIMIT 1 OFFSET ?`, column, column, column),
			w.From, w.To, offset).Scan(&v)
		return v, err
	}

	var err error
	if l.TTFBP50, err = pick("ttfb_ms", 0.50); err != nil {
		return l, fmt.Errorf("ttfb p50: %w", err)
	}
	if l.TTFBP95, err = pick("ttfb_ms", 0.95); err != nil {
		return l, fmt.Errorf("ttfb p95: %w", err)
	}
	if l.DurationP50, err = pick("duration_ms", 0.50); err != nil {
		return l, fmt.Errorf("duration p50: %w", err)
	}
	if l.DurationP95, err = pick("duration_ms", 0.95); err != nil {
		return l, fmt.Errorf("duration p95: %w", err)
	}
	return l, nil
}

// AccountQuotaHistory returns one account's observed quota, oldest first.
func (s *Store) AccountQuotaHistory(ctx context.Context, accountID string, w Window) ([]QuotaPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT at, bucket, utilization, coalesce(resets_at, 0)
FROM quota_samples
WHERE account_id = ? AND at >= ? AND at < ?
ORDER BY at`, accountID, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("quota history: %w", err)
	}
	defer rows.Close()

	var out []QuotaPoint
	for rows.Next() {
		var p QuotaPoint
		if err := rows.Scan(&p.At, &p.Bucket, &p.Utilization, &p.ResetsAt); err != nil {
			return nil, fmt.Errorf("scan quota point: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/metrics/ -v`
Expected: PASS, all metrics tests.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/query.go internal/metrics/query_test.go
git commit -m "feat(metrics): usage, totals, latency and quota queries

Series read rollups so a long window stays cheap; outcome breakdowns read
raw rows because adding outcome to the rollup key would multiply bucket
cardinality for a view only ever read over short windows. Percentiles
exclude requests that never produced a first byte."
```

---

### Task 11: Record quota observations

**Files:**
- Modify: `internal/account/manager.go`
- Test: `internal/account/quota_hook_test.go`

**Interfaces:**
- Produces: `account.Options.OnQuota func(accountID string, buckets []provider.QuotaBucket, at int64)`.

- [ ] **Step 1: Write the failing test**

Create `internal/account/quota_hook_test.go`:

```go
package account

import (
	"sync"
	"testing"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

func TestUpdateQuotaNotifiesTheHook(t *testing.T) {
	var mu sync.Mutex
	var gotID string
	var gotBuckets []provider.QuotaBucket
	var gotAt int64

	m := New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{
			SwitchThreshold: 0.98,
			OnQuota: func(id string, b []provider.QuotaBucket, at int64) {
				mu.Lock()
				defer mu.Unlock()
				gotID, gotBuckets, gotAt = id, b, at
			},
		})

	m.UpdateQuota("a", []provider.QuotaBucket{
		{Name: "5h", Utilization: 0.25, ResetsAt: 1787025600000},
	})

	mu.Lock()
	defer mu.Unlock()
	if gotID != "a" {
		t.Errorf("account id = %q, want a", gotID)
	}
	if len(gotBuckets) != 1 || gotBuckets[0].Name != "5h" || gotBuckets[0].Utilization != 0.25 {
		t.Errorf("buckets = %+v", gotBuckets)
	}
	if gotAt == 0 {
		t.Error("observation timestamp should be set")
	}
}

func TestUpdateQuotaWithNoHookDoesNotPanic(t *testing.T) {
	m := New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{SwitchThreshold: 0.98})

	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.5}})
}

// The hook must not run while the registry lock is held, or a slow observer
// stalls account selection for every in-flight request.
func TestUpdateQuotaHookRunsWithoutHoldingTheLock(t *testing.T) {
	done := make(chan struct{})
	var m *Manager
	m = New([]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{
			SwitchThreshold: 0.98,
			OnQuota: func(string, []provider.QuotaBucket, int64) {
				// Re-entering the manager from inside the hook deadlocks if the
				// lock is still held.
				m.Snapshot()
				close(done)
			},
		})

	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.5}})
	<-done
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/account/ -run TestUpdateQuota -v`
Expected: FAIL to build — `Options` has no `OnQuota` field.

- [ ] **Step 3: Write minimal implementation**

Add to `Options` in `internal/account/manager.go`:

```go
	// OnQuota observes a quota update. Called without the registry lock held,
	// so an observer may call back into the Manager. Must not block for long:
	// the metrics sink it is wired to is non-blocking by contract.
	OnQuota func(accountID string, buckets []provider.QuotaBucket, at int64)
```

Rewrite `UpdateQuota` so the hook fires outside the lock:

```go
func (m *Manager) UpdateQuota(id string, buckets []provider.QuotaBucket) {
	m.mu.Lock()
	a := m.byID[id]
	if a == nil {
		m.mu.Unlock()
		return
	}
	if a.Buckets == nil {
		a.Buckets = map[string]provider.QuotaBucket{}
	}
	for _, b := range buckets {
		a.Buckets[b.Name] = b
	}
	at := m.opts.Now().UnixMilli()
	hook := m.opts.OnQuota
	m.mu.Unlock()

	// Outside the lock deliberately: a slow observer must not stall account
	// selection for every in-flight request, and it must be able to call back
	// into the Manager without deadlocking.
	if hook != nil && len(buckets) > 0 {
		hook(id, buckets, at)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/account/ -v`
Expected: PASS, all account tests.

- [ ] **Step 5: Commit**

```bash
git add internal/account/
git commit -m "feat(account): notify an observer when quota is updated

Fired outside the registry lock so a slow observer cannot stall account
selection, and so it may call back into the Manager without deadlocking."
```

---

### Task 12: Wire it up and verify end to end

**Files:**
- Modify: `internal/config/config.go` (database path helper)
- Modify: `cmd/aiproxy/main.go`
- Test: `cmd/aiproxy/metrics_wiring_test.go`

**Interfaces:**
- Produces: `config.DBPath() string`; `buildHandler` gains a metrics sink.

- [ ] **Step 1: Write the failing test**

Create `cmd/aiproxy/metrics_wiring_test.go`:

```go
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// The whole point of the stage: a real request through the real wiring lands as
// a row with the right token counts.
func TestRequestLandsInTheMetricsStore(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":" +
				"{\"input_tokens\":210,\"output_tokens\":1,\"cache_read_input_tokens\":3000," +
				"\"cache_creation_input_tokens\":12}}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":88}}\n\n"},
		},
	})

	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
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
		t.Fatal(err)
	}

	db, err := metrics.Open(filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	defer ing.Close()

	h, err := buildHandler(cfg, store, quiet(), ing)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if err := ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var model, outcome string
	var in, out, cr, cw int64
	var status int
	err = db.DB().QueryRow(`
SELECT model, outcome, status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM requests ORDER BY id DESC LIMIT 1`).
		Scan(&model, &outcome, &status, &in, &out, &cr, &cw)
	if err != nil {
		t.Fatalf("no row was written: %v", err)
	}

	if model != "claude-opus-5" || status != 200 || outcome != "ok" {
		t.Errorf("row = model %q / status %d / outcome %q", model, status, outcome)
	}
	if in != 210 || out != 88 || cr != 3000 || cw != 12 {
		t.Errorf("tokens = %d/%d/%d/%d, want 210/88/3000/12", in, out, cr, cw)
	}

	// And it is queryable after a rollup.
	if err := metrics.RollupOnce(context.Background(), db, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	totals, err := db.Totals(context.Background(),
		metrics.Window{From: time.Now().Add(-time.Hour).UnixMilli(), To: time.Now().Add(time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 1 || totals.OutputTokens != 88 {
		t.Errorf("totals = %+v, want 1 request / 88 out", totals)
	}
}

// A metrics failure must never surface to the client.
func TestMetricsFailureDoesNotFailTheRequest(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	cfg, _ := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "t", Upstream: up.URL(),
			Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "k"},
		}}
		return nil
	})

	db, _ := metrics.Open(filepath.Join(dir, "metrics.db"))
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	// Close the store out from under the ingester: writes now fail.
	db.Close()

	h, err := buildHandler(cfg, store, quiet(), ing)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200 — a metrics failure must never reach the client", res.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/aiproxy/ -run TestRequestLands -v`
Expected: FAIL to build — `buildHandler` takes three arguments.

- [ ] **Step 3: Add the database path helper**

In `internal/config/config.go`:

```go
// DBPath is the accounting database, a sibling of the config.
func DBPath() string { return filepath.Join(Dir(), "metrics.db") }
```

- [ ] **Step 4: Wire the sink**

In `cmd/aiproxy/main.go`, change `buildHandler` to accept a `*metrics.Ingester` and record on every completed request:

```go
func buildHandler(cfg config.Config, store *config.Store, log *slog.Logger, ing *metrics.Ingester) (http.Handler, error) {
```

Add to the `account.Options` literal:

```go
		OnQuota: func(id string, buckets []provider.QuotaBucket, at int64) {
			for _, b := range buckets {
				ing.RecordQuota(metrics.QuotaSample{
					At: at, AccountID: id, Bucket: b.Name,
					Utilization: b.Utilization, ResetsAt: b.ResetsAt,
				})
			}
		},
```

Replace the `OnResult` handler body so it both logs and records:

```go
		OnResult: func(req proxy.Request, res proxy.Result) {
			log.Info("request",
				"model", req.Model, "account", res.AccountID, "status", res.Status,
				"outcome", res.Outcome.String(), "attempts", res.Attempts,
				"ttfbMs", res.TTFBMS, "waitMs", res.WaitMS, "bytes", res.Bytes,
				"in", res.InputTokens, "out", res.OutputTokens,
				"cacheRead", res.CacheReadTokens, "cacheWrite", res.CacheWriteTokens)

			ing.Record(metrics.Sample{
				StartedAt: res.StartedAt, DurationMS: res.DurationMS,
				TTFBMS: res.TTFBMS, WaitMS: res.WaitMS,
				AccountID: res.AccountID, Provider: "anthropic",
				Model: req.Model, SessionID: req.SessionID,
				Endpoint: endpointOf(req.Path), Status: res.Status,
				Outcome: res.Outcome.String(), Stream: res.Bytes > 0,
				Attempts: res.Attempts, Rotated: res.Rotated,
				InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
				CacheReadTokens: res.CacheReadTokens, CacheWriteTokens: res.CacheWriteTokens,
				CostMicros: metrics.CostMicros(req.Model, metrics.TokenCounts{
					Input: res.InputTokens, Output: res.OutputTokens,
					CacheRead: res.CacheReadTokens, CacheWrite: res.CacheWriteTokens,
				}),
			})
		},
```

`Result` needs `StartedAt` and `DurationMS` for this — add both in `internal/proxy/attempt.go`, stamping `StartedAt` at the top of `Do` and `DurationMS` in the same deferred block that sets `WaitMS`.

Add the endpoint helper, which strips the query string so `/v1/messages?beta=true` and `/v1/messages` aggregate together:

```go
func endpointOf(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}
```

In `run`, open the store, start the background workers, and shut them down in order — ingester last, so anything the rollup or a late request queued still lands:

```go
	db, err := metrics.Open(config.DBPath())
	if err != nil {
		return fmt.Errorf("open metrics db: %w", err)
	}
	defer db.Close()

	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	defer ing.Close()

	roller := metrics.NewRoller(db, time.Minute, log)
	roller.Start()
	defer roller.Stop()

	pruner := metrics.NewPruner(db, time.Duration(cfg.Metrics.RetentionDays)*24*time.Hour, log)
	pruner.Start()
	defer pruner.Stop()
```

- [ ] **Step 5: Surface the drop count**

Spec §7.3 requires the drop count to be visible, so degradation is honest rather
than silent. The TUI that will eventually show it is stage 4, so for now expose
it on the existing status endpoint.

Add a `Dropped func() int64` field to `proxy.HandlerOptions`, wire it from
`buildHandler` as `ing.Dropped`, and include it in `statusHandler`'s JSON as
`metricsDropped`. Add a test asserting the field appears and reflects a non-zero
count.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test -race ./... `
Expected: PASS across all packages.

- [ ] **Step 7: Verify against the real API**

```bash
go build -o /tmp/aiproxy ./cmd/aiproxy && /tmp/aiproxy --log-level info
# another terminal:
ANTHROPIC_BASE_URL=http://127.0.0.1:3456 claude -p "Count from 1 to 30"
# then:
sqlite3 ~/.config/aiproxy/metrics.db \
  "SELECT model, status, outcome, input_tokens, output_tokens, cache_read_tokens, cost_micros FROM requests ORDER BY id DESC LIMIT 5;"
```

Confirm the row's `output_tokens` is a believable answer length rather than an inflated sum, and that `cache_read_tokens` dominates `input_tokens` as expected for an agent workload. Record the observed rows in your report.

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/proxy/ cmd/aiproxy/
git commit -m "feat(cmd): record every request to the accounting store

Wires the ingester, rollup and retention workers into the binary and
records tokens and estimated cost per request. Shutdown order stops the
ingester last so anything queued late still lands."
```

---

## Stage 2 exit criteria

- [ ] `go vet ./...`, `staticcheck ./...`, `gofmt -l .` clean; `CGO_ENABLED=0 go build ./...` succeeds.
- [ ] `go test -race ./...` passes.
- [ ] The three stage-1 gates still pass and remain falsifiable: `TestAttemptBoundsTotalWaitOnHeaderlessRateLimits`, `TestAttemptBoundsWaitWhenUpstreamWithholdsHeaders`, `TestRelayStreamsIncrementallyWithoutBuffering`.
- [ ] `TestAccumulatorTakesLastOutputNotTheSum` passes — output totals are not inflated.
- [ ] A real Claude Code request writes a row with non-zero, believable token counts, verified by hand against the live API.
- [ ] Non-streaming responses record usage too, not just SSE.
- [ ] `Record` never blocks: `TestRecordNeverBlocksWhenTheBufferIsFull` passes.
- [ ] Only `modernc.org/sqlite` was added to `go.mod`.
