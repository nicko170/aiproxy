# aiproxy — design

**Date:** 2026-08-17
**Status:** approved, pre-implementation

## 1. Purpose

`aiproxy` is a local proxy for AI coding agents. It sits between a client
(initially Claude Code) and one or more upstream provider accounts, rotating
between accounts as quota is consumed, and it records every request so token
usage can be accounted for and graphed over time by model, account, and
interval.

It is a single Go binary. Running `aiproxy` with no arguments starts the proxy
and opens a TUI; accounts are added by logging in from inside that TUI. There is
no separate login command and no wrapper that launches the agent for you.

### In scope for v1

- Reverse proxy on localhost that a client points at via `ANTHROPIC_BASE_URL`.
- MITM forward proxy (`HTTPS_PROXY`) so clients with a hardcoded upstream host
  are still routed and accounted for.
- Multi-account rotation with model-scoped quota awareness.
- A retry engine with a hard pre-first-byte budget (§4).
- OAuth (PKCE) and API-key credentials, refreshed automatically.
- Persistent per-request accounting in SQLite, with rollups for graphing.
- TUI: accounts, live activity, usage graphs, account management, settings.
- Web dashboard with higher-fidelity charts, served by the same process.
- Provider abstraction with one full implementation (Anthropic) plus a
  config-driven Anthropic-compatible variant.

### Out of scope for v1

- Launching or wrapping the client process.
- Translating between provider wire formats (e.g. OpenAI chat-completions).
- A detached daemon with an attachable TUI. The seam for it is built (§3), the
  client is not.
- Multi-user or networked deployment. This binds loopback by default and assumes
  a single operator.

## 2. Non-negotiable properties

These are the invariants the design exists to guarantee. Each has a test.

1. **No silent stalls.** Time from receiving a client request to writing the
   first response byte is bounded by two clocks working together (§4.2):
   `retry.budgetMs` bounds time the proxy itself adds, and `retry.headerTimeoutMs`
   bounds each attempt's wait on upstream response headers. Together they cap the
   worst case at `budgetMs + attempts × headerTimeoutMs` — loose, but always
   finite and known in advance, regardless of upstream behaviour. Exceeding
   either produces a prompt, honest error — never dead air.
2. **Streaming is streaming.** A chunk received from upstream is flushed to the
   client before the next one is read. No response is buffered whole in order to
   be relayed.
3. **Accounting never slows the proxy.** Metrics ingestion cannot block, delay,
   or fail a proxied request. Under pressure it drops samples and says so.
4. **One number, one source.** The TUI and the dashboard read identical values
   through a single query interface; neither computes its own aggregates.

## 3. Architecture

Single process. Three concerns — proxying, accounting, presentation — with
presentation reading through one interface so it can later be moved out of
process without restructuring.

```
cmd/aiproxy/            flag parsing, wiring, boots TUI or --headless
internal/config/        load/save, first-run import, serialized writer
internal/provider/      Provider interface + registry
  .../anthropic/        PKCE login, refresh, usage/profile, classification
internal/account/       account state, selection, quota, pause/ramp
internal/proxy/         Chi router, handlers, upstream transport, retry engine
  .../mitm/             CONNECT handler, certificate authority
internal/metrics/       SQLite store, ingestion, rollups, queries
internal/view/          view-model service — the presentation seam
internal/tui/           Bubble Tea program
internal/web/           embedded dashboard assets + control API handlers
```

### 3.1 The presentation seam

```go
package view

type Source interface {
    ServerStatus(ctx) (Status, error)
    Accounts(ctx) ([]Account, error)
    UsageSeries(ctx, SeriesQuery) (Series, error)
    Totals(ctx, Window) (Totals, error)
    LatencyPercentiles(ctx, Window) (Latency, error)
    AccountQuotaHistory(ctx, accountID string, w Window) (QuotaHistory, error)
    Subscribe(ctx) (<-chan Event, error)

    Login(ctx, provider string) (LoginSession, error)
    ImportCredentials(ctx, source ImportSource) (added int, err error)
    SetAccountEnabled(ctx, accountID string, enabled bool) error
    SetPriority(ctx, accountID string, priority int) error
    RemoveAccount(ctx, accountID string) error
    ProbeNow(ctx) error
    UpdateSettings(ctx, Settings) error
}
```

`internal/tui` imports `internal/view` and nothing below it — not `account`, not
`metrics`, not `proxy`. The control API handlers in `internal/web` are thin
adapters over the same interface.

Two implementations are anticipated. `view.Local` (v1) calls the underlying
services directly and fans out events over channels. `view.HTTP` (future) calls
the control API and reads the SSE event stream. Moving to a detached daemon means
constructing a different `Source` in `cmd/aiproxy`; no other package changes.
This is the only reason the interface exists, and it is why every field it
returns is a plain serializable struct rather than a live pointer into proxy
state.

### 3.2 Dependencies

`github.com/go-chi/chi/v5`, `github.com/charmbracelet/bubbletea`,
`lipgloss`, `bubbles`, `modernc.org/sqlite`. No cgo, so cross-compilation
produces static binaries for darwin and linux on amd64 and arm64.

## 4. Request path and the retry engine

### 4.1 Path

For a proxied request (`/v1/messages`, `/v1/messages/count_tokens`, and any
other upstream path not reserved under `/_aiproxy`):

1. **Middleware.** Proxy-key gate (constant-time compare, loopback exempt),
   request id, timing.
2. **Buffer the body.** Required to replay the request on a different account.
   The requested `model` is extracted during buffering so the UI can show it
   immediately.
3. **Block if the model is blocked.** A configured blocklist match returns 400
   locally. A model no account can serve otherwise burns a rotation cycle.
4. **Select an account** (§4.4).
5. **Ensure the credential is fresh.** Single-flight per account.
6. **Admit.** Storm-control ramp caps concurrency onto a just-selected account.
7. **Forward** over the pooled transport (§4.5).
8. **Classify the response** via the provider (§5).
9. **Relay**, flushing each chunk, teeing SSE through the usage parser.

### 4.2 The budget invariant

Pre-first-byte time runs on **two separate clocks**, and conflating them is a
defect in its own right. One bounds time the proxy *adds*; the other bounds one
attempt's wait on the *upstream*.

**Clock 1 — the budget (`retry.budgetMs`, default 10 000).** All time the proxy
itself spends not transferring response bytes draws it down, and nothing else
does:

- retry backoff between attempts,
- waiting on a paused account inside `admit()`,
- inline absorption of a rate limit,
- token refresh,
- draining a response body the request is rotating away from.

The upstream round trip is **not** on that list and must never be charged to it.
Time-to-first-token on a large context with extended thinking is legitimate work,
not dead air. Bounding the round trip by the remaining budget cannot tell an
upstream that is silent from one that is thinking: observed in production with
`budgetMs: 1000` against a healthy account whose first token took ~1.3 s, every
attempt was cancelled and the client was answered 429 `no_account_ready` eleven
times over three minutes with nothing wrong upstream.

**Clock 2 — the header timeout (`retry.headerTimeoutMs`, default 60 000).** How
long a *single* attempt may wait for response headers. It covers the real hazard
the budget cannot express: an upstream that accepts a connection and then
withholds headers indefinitely. The attempt loop enforces this itself, by
cancelling the attempt's context — it does not rely on the transport's own
`ResponseHeaderTimeout` to catch this case. That field is set from
`headerTimeoutMs` plus a fixed safety margin purely as a backstop against a
wedged connection, so it can never fire before the attempt loop's own timer and
never substitute a generic transport error for the honest one below. 60 s is
generous enough for a slow first token and still finite. An attempt abandoned
here is a *failed attempt*: it rotates, and it is recorded as `server_error`,
never as `no_account_ready` — an account was selected, admitted, and sent to.

Together they give a worst case that is chosen rather than accidental:

```
pre-first-byte  <=  budgetMs + (attempts x headerTimeoutMs)
```

where `attempts` is capped by `maxSendsPerAccount` (2) times the number of
enabled accounts. That bound is deliberately loose: its job is to make an
indefinite hang unreachable, not to be tight. Tightness on the paths the proxy
controls is the budget's job, and the budget alone is what the reported `waitMs`
measures — a clean single-attempt success reports `waitMs` at or near zero
however long the upstream itself took.

When the budget is exhausted, the request is answered immediately with the most
informative status available (usually 429 plus a `Retry-After` derived from real
observed reset times, else 503) and the outcome is recorded.

Once response headers are written, no retry is possible and neither clock is
consulted again: a streamed completion legitimately runs for minutes, so no
pre-first-byte deadline may survive into the response body. Between them the two
clocks govern exactly one thing: how long a client can be left with no bytes.
This makes an unbounded silent hang unreachable by construction rather than by
tuning, which is the point — every constant below can be wrong without producing
a multi-minute stall.

### 4.3 429 taxonomy

Upstream rate limiting is not one condition, and treating it as one is what
produces pathological waits. The provider classifies each response into an
`Outcome`; for Anthropic:

| Outcome | Detected by | Policy |
| --- | --- | --- |
| `QuotaRejected` | `anthropic-ratelimit-unified-*-status: rejected` | A quota bucket is spent. Waiting cannot help. Mark the account throttled until its reset, **rotate** to another account. A model-scoped rejection (e.g. the per-model weekly bucket) throttles only that model for that account. |
| `ThrottledWithHint` | 429 with a parseable `Retry-After` | Upstream stated a duration, so waiting works. Pause the account for `min(hint, 300s)`. Absorb inline on the **same** account only if `hint <= retry.inlineAbsorbMaxMs` (default 5 000) **and** the budget allows; rotating here would just move a burst and discard a warm cache. |
| `ThrottledNoHint` | 429 with no `Retry-After` and no ratelimit headers | Upstream said nothing. **Do not invent a duration.** Back off briefly (250 ms, 500 ms, 1 s) and — unlike a hinted throttle — allow this request to **rotate away** to another account, so remaining budget is spent somewhere that might answer. |
| `CredentialStale` | 401 on an OAuth account | Force one refresh, retry the same account once. A second 401 rotates. |
| `CredentialRefused` | 403 | Upstream refuses this account outright; a refresh cannot fix it. Skip the account for this request and rotate. If *every* account is refused, answer 502 naming them — a 403 relayed to the client makes it discard its own unrelated session. |

Inventing a duration for `ThrottledNoHint` is the specific defect this taxonomy
exists to prevent: a default of 60 s absorbed inline across N accounts converts a
sub-second upstream rejection into an N×60 s silent hold.

### 4.4 Account selection

A candidate must be: not disabled, not errored, credential present, model
eligible (the requested model's quota buckets are not spent for that account),
and not excluded for this request. A request maintains one exclusion set,
recording every account that has already been attempted and failed, or that
upstream refused outright. Among remaining candidates, lowest `priority` wins;
ties break toward the account whose relevant quota resets soonest.

Session affinity (`routing.sessionAffinity`) pins a client session id to the
account that served it, so the upstream prompt cache stays warm; affinity yields
when the pinned account becomes ineligible.

`routing.switchThreshold` (default 0.98) is the utilization at which an account
is considered spent for selection purposes before upstream rejects it.

### 4.5 Upstream transport

One `http.Transport` per provider origin with `ForceAttemptHTTP2: false`,
`MaxConnsPerHost` bounded (default 256), keep-alive on, and a response-header
timeout that is cleared once headers arrive.

That per-attempt header timeout is `retry.headerTimeoutMs` (default 60 000) —
clock 2 of §4.2 — and it is applied by the attempt loop, not left to the
transport's coarser `ResponseHeaderTimeout`. Three properties are load-bearing:

1. It is derived from `headerTimeoutMs` and **never** from the remaining budget.
   The budget covers time the proxy adds; an upstream's time-to-first-token is
   not that, and charging it there severs healthy requests (§4.2).
2. It is armed around the round trip **only**. The moment response headers
   arrive the timer is stopped *without* cancelling, and cancellation is
   transferred to the response body so the attempt's context is released when the
   body is closed and not a moment sooner. A streamed completion runs for
   minutes; a deadline leaking into the body is a worse version of the stall this
   proxy exists to remove.
3. It bounds one attempt, so the total is `attempts x headerTimeoutMs` and the
   attempt count is capped independently (§4.2). It is not a request-level bound
   and must not be read as one.
4. The transport's own `ResponseHeaderTimeout` is derived from
   `headerTimeoutMs` plus a fixed safety margin rather than left at a
   hardcoded default. Raising `headerTimeoutMs` (§6.2) for a slower model
   therefore never trips a coarser, unlabelled cutoff underneath it — the two
   knobs cannot silently disagree.

Everything after headers is the relay's problem, governed by `retry.bodyIdleMs`
(§4.6). The two windows abut exactly: neither leaves a gap in which an upstream
can be silent without something eventually giving up.

HTTP/2 is deliberately disabled. A single h2 connection multiplexes all requests
to an origin and shares one flow-control window; agent clients post large context
payloads concurrently, and those uploads then queue behind `WINDOW_UPDATE`
frames, so a trivial request can wait minutes for headers. Independent HTTP/1.1
connections have no application-layer flow control and each fills its own socket
at TCP speed. `ForceAttemptHTTP2: false` is load-bearing, not incidental.

TCP sockets on both the client-facing listeners and the upstream transport set
`NoDelay`. Nagle coalescing on small SSE frames adds tens of milliseconds per
chunk, which reads to a user as a sluggish stream. `tls.Server` does not enable
`NoDelay` by default, so the MITM listener must set it explicitly.

### 4.6 Relay

Streaming responses are copied chunk-by-chunk with an explicit `Flush()` after
each write, using `http.NewResponseController` for write deadlines.
Non-streaming responses are read fully and written once. `Content-Encoding` and
`Content-Length` are stripped when the body is transformed, and
`Accept-Encoding` is normalized so response framing always matches what the
client is told.

A body-idle watchdog covers the window the header timeout does not: if upstream
produces no bytes for `retry.bodyIdleMs` (default 120 000) after headers, the
connection is torn down rather than left hanging, and the client sees a broken
response it can retry. A truncated stream is never ended cleanly, because a
clean end looks like a complete answer and suppresses the client's retry.

Requests that must carry the client's own credential rather than a rotated one —
paths bound to the client's paired identity, and file/attachment transfers — are
relayed transparently with no account logic and no buffering.

## 5. Provider seam

```go
type Provider interface {
    Name() string

    Login(ctx) (Credential, error)
    Refresh(ctx, Credential) (Credential, error)
    Profile(ctx, Credential) (Profile, error)
    Quota(ctx, Credential) (Quota, error)   // zero-spend; ErrUnsupported allowed

    Endpoint(Account) *url.URL
    Authorize(*http.Request, Credential)
    RewriteBody([]byte, Account) ([]byte, error)
    ClassifyResponse(*http.Response) Outcome
    ParseUsage(sseEvent []byte) (*UsageDelta, bool)
}
```

`ClassifyResponse` and `ParseUsage` are the two methods that keep the retry
engine and the accounting pipeline provider-agnostic. A provider with different
rate-limit headers or a different usage envelope is a new package, not a change
to `internal/proxy` or `internal/metrics`.

v1 implementations:

- **`anthropic`** — OAuth (PKCE) and API-key credentials, `/api/oauth/profile`
  and `/api/oauth/usage` for identity and zero-spend quota, full 429
  classification, SSE usage parsing including cache token counts. Rewrites the
  body's account identifier to match the injected credential.
- **`anthropicCompatible`** — config-driven: custom base URL, API-key auth,
  optional model-name mapping, no quota endpoint (`Quota` returns
  `ErrUnsupported`, and such accounts are selected only by priority). Covers
  Anthropic-shaped third-party endpoints and exists to keep the seam honest.

## 6. Accounts, auth, config

### 6.1 Auth

OAuth uses PKCE with S256. The TUI starts a loopback callback listener on an
ephemeral port, opens the browser to the provider's authorize URL, and displays
that URL plus a paste-the-code field — the fallback matters the first time this
runs over SSH, where no browser can open. State is verified on callback. On
success `Profile()` supplies the account label, organisation, and plan, and the
account serves traffic without a restart.

Refresh is single-flight per account: proactive at expiry minus five minutes,
synchronous only when already expired. Rotated refresh tokens are persisted
through the serialized config writer (§6.2) — concurrent read-modify-write on
the config file drops rotated tokens and invalidates accounts on next start.

### 6.2 Config

`~/.config/aiproxy/` (honouring `XDG_CONFIG_HOME`), mode 0700, holding
`config.json` (0600), `metrics.db`, and `ca/`.

```json
{
  "listen":   { "addr": "127.0.0.1:3456", "apiKey": "ap-…" },
  "accounts": [
    { "id": "01J…",
      "provider": "anthropic",
      "label": "person@example.com (Org)",
      "priority": 0,
      "disabled": false,
      "credential": { "type": "oauth", "accessToken": "…",
                      "refreshToken": "…", "expiresAt": 0 },
      "identity":   { "accountUuid": "…", "orgUuid": "…", "orgName": "…" },
      "upstream":   null,
      "modelMap":   null }
  ],
  "routing": { "switchThreshold": 0.98, "sessionAffinity": true,
               "blockedModels": [] },
  "retry":   { "budgetMs": 10000, "inlineAbsorbMaxMs": 5000,
               "bodyIdleMs": 120000, "headerTimeoutMs": 60000 },
  "quotaProbe": { "intervalSeconds": 300 },
  "metrics":    { "retentionDays": 90 },
  "mitm":       { "enabled": true }
}
```

Accounts carry a **stable ULID `id`**, assigned once. Identity is never array
position: reordering or renaming must not repoint anything, and metrics rows
reference `id` so history survives a relabel.

`retry.budgetMs` and `retry.headerTimeoutMs` are the two clocks of §4.2 and are
tuned independently. `budgetMs` is the only one an operator should shorten to
make the proxy give up sooner; shortening it does **not** shorten how long a
single upstream attempt may take, which is deliberate — a small `budgetMs` used
to cancel healthy slow-first-token requests, and that is the defect the split
removes. `headerTimeoutMs` is a safety net against an upstream that never
answers, so it is set generously (60 000) rather than tightly.

`quotaProbe.intervalSeconds` defaults to **300**. The zero-spend usage endpoint
has its own rate limit; polling it every 30 s gets the probe itself throttled,
after which quota data goes stale and rotation decides on outdated numbers. The
prober backs off exponentially on its own 429s and reports probe health in the
UI, so a throttled probe is visible rather than silently wrong.

All config writes go through a single serialized writer that re-reads from disk,
applies the mutation, and writes atomically (temp file plus rename), so an
external edit is never clobbered wholesale.

### 6.3 First-run import

On first start with no accounts configured, if `~/.config/teamclaude.json`
exists, its accounts and credentials are copied in and assigned ids. One log
line reports the count. The source file is never written to. Additionally,
`~/.claude/.credentials.json` is offered as an import source from the accounts
screen at any time.

## 7. Accounting

Counters cannot be graphed, so the store is event-sourced with rollups above it.

### 7.1 Schema

```sql
CREATE TABLE requests (
  id             INTEGER PRIMARY KEY,
  started_at     INTEGER NOT NULL,      -- unix ms
  duration_ms    INTEGER,
  ttfb_ms        INTEGER,
  account_id     TEXT NOT NULL,
  provider       TEXT NOT NULL,
  model          TEXT,                  -- as requested by the client
  upstream_model TEXT,                  -- after model mapping
  session_id     TEXT,
  endpoint       TEXT,
  status         INTEGER,
  outcome        TEXT,                  -- ok|quota_rejected|throttled|error|blocked
  stream         INTEGER,
  attempts       INTEGER,
  rotated        INTEGER,
  wait_ms        INTEGER,               -- budget consumed before first byte
  input_tokens        INTEGER,
  output_tokens       INTEGER,
  cache_read_tokens   INTEGER,
  cache_write_tokens  INTEGER,
  cost_micros    INTEGER                -- NULL when the model is unpriced
);
CREATE INDEX requests_started       ON requests(started_at);
CREATE INDEX requests_acct_started  ON requests(account_id, started_at);
CREATE INDEX requests_model_started ON requests(model, started_at);

CREATE TABLE quota_samples (
  at          INTEGER NOT NULL,
  account_id  TEXT    NOT NULL,
  bucket      TEXT    NOT NULL,         -- 5h | 7d | 7d_<model>
  utilization REAL,
  resets_at   INTEGER,
  PRIMARY KEY (at, account_id, bucket)
);

CREATE TABLE usage_buckets (
  bucket_start INTEGER NOT NULL,
  granularity  TEXT    NOT NULL,        -- minute | hour
  account_id   TEXT    NOT NULL,
  model        TEXT,
  requests     INTEGER NOT NULL,
  input_tokens       INTEGER NOT NULL,
  output_tokens      INTEGER NOT NULL,
  cache_read_tokens  INTEGER NOT NULL,
  cache_write_tokens INTEGER NOT NULL,
  cost_micros        INTEGER,
  PRIMARY KEY (bucket_start, granularity, account_id, model)
);
```

Cache reads and writes are separate columns because under an agent workload
cache reads dominate input tokens by an order of magnitude; summing them
together makes every cost figure wrong.

`ttfb_ms` and `wait_ms` are recorded because they are what makes the property in
§2.1 observable. A p95 TTFB line surfaces a stalling proxy in seconds; `wait_ms`
attributes the stall to budget consumption specifically.

### 7.2 Capturing token counts

Three requirements that stage 1 surfaced and this section was previously silent
on. Each is a way to produce numbers that look plausible and are wrong, which is
worse than having none.

**Output tokens are cumulative, not incremental.** A streamed message reports
usage twice: `message_start` carries the one-shot input, cache-read and
cache-creation counts, and each `message_delta` carries `output_tokens` as a
running total **for that message**, not a delta since the last event. Summing
`UsageDelta.OutputTokens` across events therefore inflates output badly on long
completions — the longer the answer, the worse the error. The accumulator takes
the **last** value seen for output within a message and **sums** the one-shot
input and cache figures across messages. A test asserts a multi-`message_delta`
stream reports the final count, not the sum of the running totals.

**Non-streaming responses count too.** The relay tees usage only when the
response is `text/event-stream`, so a JSON response's `usage` object is never
read. `count_tokens` and any non-streaming call currently contribute nothing.
Usage capture must cover both shapes, with the provider parsing whichever
envelope it receives.

**The sink is real, not a hook.** Stage 1 wired `OnUsage` to a no-op closure and
`Result` carries no token fields. Both change here: `Result` gains the four token
counts so the request log line and the metrics row read the same numbers from one
place, without a second lookup or a second parse.

### 7.3 Ingestion

The proxy hands a `metrics.Sample` to a buffered channel (capacity 4096). One
writer goroutine batches inserts inside a transaction every 200 ms or 100 rows.
SQLite runs in WAL mode with `synchronous=NORMAL`.

If the channel is full the sample is **dropped** and a counter increments; the
proxy never blocks on accounting, and the TUI displays the drop count so
degradation is visible rather than invisible.

A background aggregator maintains `usage_buckets` at minute and hour grain.
Charts read rollups; `requests` backs drill-down and is pruned at
`metrics.retentionDays`. Rollups are never pruned — they are small, and they are
what makes a 90-day query cheap.

### 7.4 Queries

Exposed only through `view.Source`: `UsageSeries` (window, granularity,
`groupBy` ∈ {account, model, outcome}), `Totals`, `LatencyPercentiles` (p50/p95
TTFB and duration), `AccountQuotaHistory`. Both front-ends read these, so they
cannot disagree.

`outcome` is a persisted, append-only enumeration, so its granularity is fixed
the moment rows exist. Before ingestion is switched on, `OutcomeServerError` is
split into distinct kinds — it currently conflates an upstream 5xx, a local
admission failure, and a failed send, which are three different operational
stories that an outcome breakdown must be able to tell apart. Splitting later
means migrating rows; splitting now is free.

Cost estimation uses a small embedded model→price table. Unknown models record
`NULL` cost rather than a plausible wrong number, and the UI shows unpriced
usage as such.

## 8. TUI

Bubble Tea with Lipgloss. Five screens, reachable by `1`–`5` or tab:

1. **Overview** — accounts with quota bars (5h, 7d, per-model), reset
   countdowns, and a one-hour throughput sparkline each. Header shows listen
   address, uptime, in-flight count, p95 TTFB, and any metrics drop count.
2. **Activity** — live feed: time, model, account, status, duration, TTFB,
   tokens. Colored per session, scrollable, pausable, filterable by account,
   model, or outcome.
3. **Usage** — range selector (1h/24h/7d/30d), group-by toggle, braille-rendered
   stacked series, totals and cost table, top models and top sessions.
4. **Accounts** — add via login, import, or API key; disable, reprioritise,
   remove; per-account detail with quota history.
5. **Settings** — switch threshold, retry budget, inline absorb max, body idle
   timeout, probe interval, blocked models, session affinity, MITM on/off.
   Edited in place and persisted immediately.

Keys are vim-flavoured with a `?` overlay and a persistent footer; `l` login,
`p` probe now, `o` open dashboard. Quota bars render the reset time inside the
bar rather than beside it, which keeps a row readable at narrow widths.

Rendering degrades explicitly: truecolor → 256 → 16 colors, `NO_COLOR`
respected, and narrowing drops sparklines then columns rather than wrapping.
`slog` output feeds a ring buffer the Activity screen renders; under
`--headless` it goes to stderr.

## 9. Dashboard and control API

Assets are embedded via `embed.FS` — vanilla JS with hand-rolled SVG charts, no
build step and no CDN, so the dashboard works offline and the repository stays a
pure Go project. Charts: tokens over time stacked by model and by account, cost
over time, TTFB p50/p95, outcome mix, and quota utilization with reset markers.
Theme follows `prefers-color-scheme`.

Everything control-plane is reserved under `/_aiproxy`, which cannot collide with
an upstream path:

```
GET  /_aiproxy/                            dashboard
GET  /_aiproxy/api/v1/status
GET  /_aiproxy/api/v1/accounts
GET  /_aiproxy/api/v1/usage?from&to&granularity&groupBy
GET  /_aiproxy/api/v1/quota/history?account
GET  /_aiproxy/api/v1/events               SSE live activity
POST   /_aiproxy/api/v1/accounts/{id}/enabled
POST   /_aiproxy/api/v1/accounts/{id}/priority
DELETE /_aiproxy/api/v1/accounts/{id}
POST   /_aiproxy/api/v1/accounts/login          begins a login session
POST   /_aiproxy/api/v1/accounts/import
POST   /_aiproxy/api/v1/probe
POST   /_aiproxy/api/v1/settings
```

The proxy API key gates these, with loopback exempt. This set covers every
method on `view.Source`, which is the requirement that makes it a usable control
plane for a detached-daemon mode: that mode then costs a client implementation
rather than a redesign. Whenever a method is added to `view.Source`, a route is
added here — the two are kept in lockstep deliberately, and a test asserts every
interface method has a corresponding route.

## 10. MITM forward proxy

`mitm.enabled` (default true) makes the listener answer `CONNECT`. A CONNECT to a
managed provider host is terminated locally with a minted leaf certificate and
handed to the same Chi handler the reverse-proxy path uses, so account logic,
retry policy, and accounting are identical on both entry points. A CONNECT to any
other host is blind-tunneled untouched — hosts we do not manage must never be
hijacked.

A CA key and certificate are generated once into `~/.config/aiproxy/ca/`
(0700/0600) using `crypto/x509`. Leaf certificates are minted lazily per host
and cached in memory. Installing the CA into the system or client trust store is
the operator's action; the accounts screen prints the exact command for the
platform and the TUI shows whether the CA is currently trusted. Failure to mint
certificates resets the memo so a transient error cannot permanently wedge the
CONNECT path.

The terminating TLS listener sets `NoDelay` explicitly (§4.5).

## 11. Error handling

- **Transient upstream failures** (connection reset, header timeout, body idle
  timeout) fail fast and let the client retry, which also evicts the dead socket
  from the pool. Failing over to another account does not help a transport
  fault and wastes budget.
- **Transport errors are not credential errors.** A thrown connection error
  never sidelines an account; a bad credential arrives as a 401 or 403
  *response*.
- **All accounts unavailable** returns 429 with a `Retry-After` computed from
  observed reset times. When every account was *refused* (403) rather than out
  of quota, it returns 502 naming them, because waiting cannot help.
- **Client disconnect** tears down the upstream request rather than leaking it.
- **Panics** in a request goroutine are recovered by Chi middleware, logged, and
  answered 500; the process survives.
- **A dropped WebSocket or upgraded connection** destroys its peer rather than
  raising an unhandled error, so one flapping session cannot take the process
  down.

## 12. Testing

A scriptable fake upstream is the backbone: it controls status, headers, body,
and SSE chunk timing. Every classification and retry case is a table test —
bare 429, hinted 429, quota-rejected 429, model-scoped rejection, 401 then
refresh, 403 rotation, mid-stream stall.

Four tests are load-bearing and are written before the code they cover:

1. **Budget enforcement.** Fake upstream returns a bare 429 with no headers on
   every account. Assert the client is answered within a small multiple of
   `retry.budgetMs`, and that the measured proxy-added wait never exceeds the
   budget. This is the acceptance criterion for the retry engine.
2. **The budget does not bound the upstream.** Fake upstream withholds response
   *headers* for longer than `retry.budgetMs` and then answers 200. Assert the
   request succeeds in one attempt and streams its body. A healthy account with a
   slow first token must not be severed by the retry budget, and `waitMs` for
   that request must be at or near zero — the round trip is not time the proxy
   added.
3. **The header timeout bounds one attempt.** Fake upstream withholds headers
   indefinitely under a short `retry.headerTimeoutMs` and a long `retry.budgetMs`.
   Assert the attempt is abandoned on the header timeout, well inside the budget,
   and that the outcome is `server_error` rather than `no_account_ready`.
4. **Streaming fidelity.** Fake upstream emits SSE chunks 100 ms apart. Assert
   the client observes them incrementally, with arrival times tracking the
   upstream's, proving no whole-response buffering.

Beyond those: in-memory SQLite for metrics (rollups must equal raw aggregates;
pruning must retain rollups; drop counting must be accurate under a saturated
channel), golden-file classification tests seeded with captured real response
headers including a bare 429, `teatest` frame snapshots at several widths, and
`go test -race ./...` plus a parallel-stream hammer test to catch pool and ramp
deadlocks.

CI on GitHub Actions: `go vet`, `staticcheck`, race tests, and cross-builds for
darwin and linux on amd64 and arm64.

## 13. Delivery order

This spec covers more than one sitting of work, so it is built in stages that
each end somewhere usable rather than half-wired.

1. **Core proxy.** Config, accounts, the Anthropic provider, selection, the retry
   engine and budget, streaming relay. Runs `--headless` with logging to stderr.
   Ends when Claude Code works through it and the two load-bearing tests in §12
   pass. This is the stage that fixes the behaviour the design exists for.
2. **Accounting.** SQLite store, ingestion, rollups, queries, retention. Ends
   with usage queryable and verified against raw aggregates.
3. **`view.Source` and the control API.** The seam plus the JSON/SSE routes. Ends
   with the lockstep test in §9 passing.
4. **TUI.** Five screens over `view.Source`, in-TUI login. Ends when the binary
   with no arguments is the primary way to use the tool.
5. **Dashboard.** Embedded assets and SVG charts over the same API.
6. **MITM.** CONNECT handler and CA, reusing the stage-1 handler.

Stage 1 depends on nothing later; stages 2 and 3 are prerequisites for 4 and 5;
stage 6 is independent of 2–5 and can move earlier if a hardcoded-endpoint client
becomes the pressing need.

## 14. Deferred, deliberately

- **Detached daemon with attachable TUI.** Seam built (§3.1), client not.
  Until then, service-mode instances run `--headless` and are observed via the
  dashboard.
- **Additional providers and wire-format translation.** The seam takes them;
  v1 ships Anthropic and an Anthropic-compatible variant.
- **Request/response body logging to disk.** Metrics cover accounting; full
  transcript capture is a separate feature with its own retention and privacy
  questions.
- **Egress pinning and outbound proxy chaining.** Both are real needs in
  restricted networks and neither affects the core design; they slot into
  `internal/proxy` transport construction later.
