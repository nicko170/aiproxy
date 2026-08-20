# aiproxy

A local proxy for AI coding agents.

It sits between your agent and one or more upstream provider accounts, rotates
between accounts as quota is consumed, and records every request so token usage
can be accounted for and graphed over time by model, account, and interval.

One binary. Run `aiproxy`, log in from the TUI, point your agent at it.

## Status

Working, and usable as the primary way to run an agent. Stages 1–4 of the
[design](docs/superpowers/specs/2026-08-17-aiproxy-design.md) are built and
tested:

| Stage | | |
|---|---|---|
| 1 | Core proxy — config, accounts, Anthropic provider, selection, retry engine, streaming relay | done |
| 2 | Accounting — SQLite store, ingestion, rollups, queries, retention | done |
| 3 | `view.Source` and the control API — the presentation seam plus JSON/SSE routes | done |
| 4 | TUI — five screens over the seam, in-TUI login | done |
| 5 | Dashboard — embedded assets and SVG charts over the same API | not started |
| 6 | MITM forward proxy — CONNECT handler and CA, for clients with a hardcoded upstream | not started |

Until stage 5 lands, the `o` key in the TUI opens a dashboard URL that nothing
is serving yet.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/nicko170/aiproxy/main/install.sh | sh
```

That fetches the latest release for your platform, verifies it against the
release's `checksums.txt`, and installs to `/usr/local/bin` — or to
`~/.local/bin` if the system path isn't writable, rather than reaching for
`sudo`. Set `BINDIR` to install elsewhere, or `AIPROXY_VERSION` to pin a
version — on `sh`, not on `curl`, or the pipeline hands them to the wrong
process and they are silently ignored:

```sh
curl -fsSL https://raw.githubusercontent.com/nicko170/aiproxy/main/install.sh | BINDIR=~/bin AIPROXY_VERSION=0.1.0 sh
```

Prebuilt binaries are on the [releases page](https://github.com/nicko170/aiproxy/releases)
for darwin and linux on amd64 and arm64. They are not codesigned, so a tarball
downloaded through a *browser* on macOS needs
`xattr -d com.apple.quarantine aiproxy`; the installer above is unaffected.

With a Go toolchain, either of these works too:

```sh
go install github.com/nicko170/aiproxy/cmd/aiproxy@latest
go build -o aiproxy ./cmd/aiproxy   # from a checkout
```

## Updating

aiproxy checks for a newer release once a day and says so in the TUI header:

```
● aiproxy  ·  on 127.0.0.1:3456  ·  up 4h12m  ·  2 in flight  ·  p95 240ms  ·  ↑ 0.2.0 available
```

Press `u` to install it, or from a shell:

```sh
aiproxy update           # install the latest release
aiproxy update --check   # report only; exits 1 when an update is available
```

Either way the download is verified against the release's `checksums.txt`
before anything is replaced, and the running process keeps serving on the old
binary until you restart it — an update never interrupts a request in flight.
Once one is installed the header says `↑ 0.2.0 installed · restart` until you do.

If aiproxy was installed somewhere it can't write (a Homebrew or root-owned
path), it says so and points you back at the installer rather than reaching for
`sudo`.

**Turning it off.** The check is one HTTPS request to github.com per day, which
tells GitHub this installation's IP address and version. To stop it, toggle it
on the Settings screen (`5`) or set it in `config.json`:

```json
"update": { "checkEnabled": false, "checkIntervalHours": 24 }
```

With `checkEnabled` false, aiproxy makes no outbound request at all.
`aiproxy update` still works, because running it is an explicit request.

**What is verified, and what isn't.** `checksums.txt` is fetched over TLS from
the same origin as the release asset, so it defends against a corrupted or
truncated download. It is not a signature: anyone who could publish a release
could publish its checksum too. Release signing isn't implemented yet.

## Privacy filter

aiproxy can scan request bodies for secrets and personal information before
they leave your machine, replacing each match with a stable placeholder and
restoring the original value in the response so the agent never sees the
substitution. **It is off by default.**

**Every key under `privacy` takes effect on restart, not immediately** — the
detector set, the allowlist and denylist, the failure modes, the cache's salt,
and (when the model is configured) its session are all built once at startup,
the same way `update.checkIntervalHours` is. Flipping `enabled` on does **not**
turn on protection for the request you send next; it schedules protection for
after you restart aiproxy. `enabled` and `denylist` are editable from the
Settings screen, and it marks each one `saved · restart to apply` the moment you
change it, so you're not left guessing there. `onScanFailure`,
`onUnresolvedPlaceholder` and `scanTimeoutMS` have no Settings-screen row yet —
edit them in `config.json` or through `POST /_aiproxy/api/v1/settings` — and
nothing in the TUI will tell you a restart is pending for those three. Either
way: restart before you trust it.

```json
"privacy": {
  "enabled": false,
  "onScanFailure": "closed",
  "onUnresolvedPlaceholder": "passthrough",
  "scanTimeoutMS": 10000,
  "rules": { "builtinSecrets": true, "entropy": true },
  "denylist": [],
  "allowlistExtra": [],
  "cacheEntries": 50000,
  "ner": {
    "enabled": false,
    "labels": [],
    "maxScanBytes": 4096
  }
}
```

Turning `enabled` on turns on the deterministic rules (`rules.builtinSecrets`
and `rules.entropy`), because those are the reason to enable the filter at
all — pattern-matched credentials (API keys, tokens, connection strings) and,
with `entropy` on, high-entropy strings assigned to a credential-shaped
variable. `denylist` adds your own literal strings or patterns to redact;
`allowlistExtra` exempts values the rules would otherwise catch.
`cacheEntries` bounds the LRU that remembers a string's findings so a repeated
value (a system prompt sent on every turn) is not rescanned.

The local NER model (`ner`) adds detection for prose PII — email addresses,
phone numbers, physical addresses, and personal names — but **every label is
individually opt-in, and the default set is empty even when `ner.enabled` is
true.** Two categories the model supports, `private_url` and `private_date`,
are deliberately left for you to enable rather than defaulted on: source code
is full of import URLs, API endpoints, documentation links, changelog dates,
and licence years, none of which are a privacy concern, and redacting them
corrupts the agent's context for almost no privacy gain. `maxScanBytes`
(default 4096) bounds how much of any one string the model looks at — see
"Cost and limits" below; the deterministic rules have no such cap.

### Installing the model

The NER model is not bundled — it's roughly **850 MB** (a 27.9 MB tokenizer,
an 809 MB quantized ONNX model and its weights file, and a 7–12 MB platform
ONNX Runtime library), so it is fetched on demand:

```sh
aiproxy privacy install   # downloads and verifies every asset (~850 MB)
aiproxy privacy status    # reports what's installed
```

Every download is checked against a SHA-256 digest compiled into the binary
before it's used. The model is pinned to Hugging Face revision
`7ffa9a043d54d1be65afb281eddf0ffbe629385b`, and the runtime to ONNX Runtime
v1.23.2, so an install today and an install next year fetch byte-identical
files. Supported platforms are darwin/arm64, darwin/amd64, linux/amd64, and
linux/arm64 — there's no ONNX Runtime build for Windows in that pin, so the
model tier is unavailable there. The deterministic rules have no such
restriction and work on every platform.

### Cost and limits

Running the model costs real time. Measured on an Apple M3 Pro (darwin/arm64,
CPU), with the benchmarks at the end of this section:

| Input | Tokens | Latency | Throughput |
|---|---|---|---|
| 512 B | 166 | 126 ms | 1,321 tok/s |
| 2 KB | 663 | 281 ms | 2,356 tok/s |
| 4 KB | 1,325 | 589 ms | 2,249 tok/s |
| 16 KB | 5,313 | 3.42 s | 1,553 tok/s |

Three things in that curve matter more than the headline number. There is a
**fixed cost of roughly 30–55 ms per scan**, so a short string is dominated by
setup rather than inference — which is much of why the cache earns its keep.
Throughput **peaks around 2,000 tokens and falls after**, because past one
2,048-token window the 25% overlap re-processes tokens: 16 KB pays about a 50%
tax. And **concurrency buys nothing** — inference is serialized, so twelve
cores deliver one core's throughput, and ~2,300 tok/s is the ceiling for the
whole process, not per request.

That last point is the one to size against: at 4 KB per newly-seen string,
that is under two fresh scans a second for the entire proxy. Resent
conversation history is served from the cache and costs nothing, so steady
state is far better than that — but a burst of new conversations queues.

By comparison the deterministic rules scan the same 4 KB in **0.91 ms**, about
650× faster, and have **no cap** — so a credential or denylisted value anywhere
in a large file is still caught. Tokenization is not the cost either: it runs
at 1.4M tok/s, under 0.2% of a scan, so a slow scan is always inference.

This is why `ner.maxScanBytes` defaults to 4096. A string longer than the cap
is scanned by the model only up to the cap, and the truncation is logged, never
silent, so it isn't mistaken for a full scan.

Numbers taken on one machine are worth exactly that, so the benchmarks are
committed — measure your own silicon:

```sh
AIPROXY_MODEL_DIR=~/.config/aiproxy/models/privacy-filter \
  go test ./internal/privacy/ner -run '^$' -bench . -benchtime 10x
go test ./internal/privacy/rules -run '^$' -bench . -benchtime 200x
```

They skip without `AIPROXY_MODEL_DIR`, so an ordinary `go test ./...` still
needs no download.

### Failure modes

`onScanFailure` (default `closed`) governs the request side: if the filter
can't scan a request — the model isn't installed, a detector errors, the scan
runs past `scanTimeoutMS` — `closed` refuses the request rather than sending it
upstream unfiltered, because a privacy filter that silently degrades is worse
than no filter: you'd believe you were protected exactly when you weren't.
Setting it to `open` sends the request unfiltered and records that it happened.

Either way it is recorded, and that recording is the point: the header shows
`⊘ filter error` while a scan is failing, and `⊘ 3 unfiltered` for as long as
the session has sent anything upstream unscanned. Both outrank the redaction
count, because a count is reassurance and you should never be shown reassurance
in place of a fault.

One thing outranks both of those: `⊘ 2 unresolved`, a placeholder that came
back from upstream with no entry in this request's restore table. A filter
error tells you a scan didn't happen; an unresolved placeholder means one did,
and the reversal it promised didn't hold — so it wins the header outright.

`scanTimeoutMS` (default 10000) bounds the whole request's scan. It is the only
ceiling on the latency the filter adds — scanning happens before the retry loop,
so `retry.budgetMS` doesn't cover it, and `ner.maxScanBytes` bounds one string
rather than one request. With the model tier off (the default) the rules are
microseconds and this never fires. With it on, 10 s is roughly seventeen
freshly-seen 4 KB strings at the measured 589 ms each; past that the scan
expires and `onScanFailure` decides what happens. Raise it if you'd rather wait than refuse. There's no
"unbounded" value: a 0 is read as unset and replaced by the default.

`onUnresolvedPlaceholder` (default `passthrough`) governs the response side:
if a response contains a placeholder that isn't in this request's restore
table, `passthrough` emits it verbatim and counts it, because the
alternative — guessing at the original value — means writing a wrong value
into your files. Setting it to `error` severs the stream instead.

### What is and isn't protected

Deterministic rules catch credentials and internal identifiers, anywhere in a
request, regardless of size: recognised vendor token formats, PEM blocks, JWTs,
connection strings, credential-shaped assignments, and your own `denylist`
entries. Entropy is a *qualifier* on those rules, not a detector of its own —
a bare high-entropy string with no keyword, assignment or vendor prefix around
it is not flagged, because in real source code that shape is far more often a
checksum, a UUID, or a minified asset than a secret, and replacing one with a
placeholder derails the agent. The NER model, where enabled, catches prose PII like names and contact
details. Neither protects **the source code itself** — the agent needs to
read and write your code to do its job, and no redaction scheme can change
that. This filter is about what leaves your machine as an incidental,
extractable secret or personal detail, not about hiding your codebase from
the model you're paying to work on it.

Two shapes of request are **never filtered**, by design, and both are worth
knowing about:

- **The passthrough paths** — `/api/oauth/file_upload`, `/api/oauth/files/`,
  `/v1/code/`, and `/v1/oauth/token`. These carry your own paired credential
  rather than a rotated account's, and they're relayed byte-for-byte: redacting
  a credential the far end has to verify just breaks authentication. **File
  uploads go through `/api/oauth/file_upload`, so a file you attach is not
  scanned.** The filter covers what your agent *sends as model context*, not
  what you hand the provider directly.
- **Any request body that isn't JSON.** The filter works by walking JSON string
  values, so a multipart or form-encoded body has nothing it can read. Those
  pass through unfiltered rather than being refused — refusing would break
  uploads outright under the default failure mode — and each one increments the
  same `unfiltered` counter the header shows.

There is one intentional, residual disclosure even when everything works:
each placeholder is a keyed hash of the original value, so the same secret
always produces the same placeholder within an install. That's what keeps
the provider's prompt cache working across turns — a value that changed
identity on every request would bust the cache on every request. It does
tell the provider "this is the same value as before, whatever it is": a
hash, not the value, but a hash is still a bit of information they didn't
have.

## Quick start

```sh
aiproxy
```

That starts the proxy and opens the TUI. Press `l` to log in — a browser opens
for the OAuth flow, and if it can't reach one you can paste the authorization
code back in. Repeat for each account you want in the rotation.

Then point your agent at it:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:3456
export _CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1
claude
```

Both variables matter. Claude Code otherwise treats every custom base URL as a
third-party provider and locally caps even native-1M Claude models at 200K,
triggering auto-compaction at about 180K. aiproxy relays the Anthropic API and
preserves the model and beta headers, so the first-party hint is the accurate
setting here.

You can import an existing credential on demand from the Accounts screen with
`i`, then a second key for the source: `c` reads Claude Code's own file
(`~/.claude/.credentials.json`), `x` reads Codex's (`~/.codex/auth.json`) —
see [ChatGPT accounts](#chatgpt-accounts) for the latter. Either way you don't
have to re-authorize an account you have already logged into there.

## The TUI

| Key | Screen | |
|---|---|---|
| `1` | Overview | per-account quota gauges, reset countdowns, throughput sparklines |
| `2` | Activity | live request feed and the log ring |
| `3` | Usage | stacked usage charts and a ledger table |
| `4` | Accounts | add, import, enable, reprioritize, remove |
| `5` | Settings | edit runtime settings in place |

`tab` / `shift+tab` cycle screens. `?` opens help, `q` quits (and shuts the
proxy down with it). `l` starts a login, `p` forces a quota probe, `o`
opens the dashboard, and `u` installs an available update (see
[Updating](#updating)).

Per-screen keys are shown in the footer: `space` pauses the activity feed,
`a`/`m`/`c` filter it by account/model/outcome and `v` toggles the log view;
`r` cycles the usage range (1h, 24h, 7d, 30d) and `g` cycles the grouping
(account, model, outcome); on Accounts, `e` toggles enabled, `+`/`-` change
priority, `x` removes, and `enter` opens detail.

## Warming idle accounts

Anthropic anchors the 5-hour window to an account's **first request**, not to a
wall clock. Two accounts on the same plan here reset at 21:00 and 00:20, and a
window that had never been used reported `resetsAt=0` — no clock running at all.

That costs you throughput whenever traffic concentrates on one account. The
standby's window does not begin until the moment you fail over onto it, so its
reset is pushed a full five hours past the handover instead of having been
counting down all along. Over a day that is fewer windows per account than the
4.8 the limit actually allows.

So when the busiest account passes `warming.threshold` (default 0.5) of its own
5-hour window, aiproxy sends **one** minimal request to the standby whose clock
is stopped — cheapest model, `max_tokens: 1`. Measured against the live API that
is 8 input and 1 output token, and it needs no Claude Code system prompt. The
response's own rate-limit headers are recorded immediately, which is also what
stops the next cycle warming the same account again.

Only accounts that need it are touched: OAuth credentials only, only when the
5-hour window is genuinely unstarted, and the highest-priority standby first —
the one traffic would actually fail over to. A warm that fails backs off for 15
minutes, so one dead credential cannot turn into a steady stream of billable
attempts, and the failure is visible rather than silent.

**This is the one setting that makes aiproxy spend money without a client asking
it to.** It is on by default because the amount is a fraction of a cent per
account per five hours and the throughput gain is real, but it is a different
category from everything else here, so it is worth knowing about rather than
discovering on a bill. `"warming": {"enabled": false}` turns it off entirely.

Note the interaction with [account selection](#which-account-gets-picked): a
freshly warmed account has a nearly-full window five hours out, so it ranks
*low* on expiring allowance and will not steal traffic from the account you are
deliberately draining. Warming prepares the standby; it does not promote it.

ChatGPT accounts are never warmed, even when idle and past threshold. Whether
OpenAI's own rate-limit window is anchored to first use the same way
Anthropic's is has not been verified, and warming a window that turns out to
be fixed to a wall clock would just be a billable request every cycle for
nothing.

## ChatGPT accounts

Codex CLI subscriptions work the same way Anthropic accounts do: log in once,
and aiproxy rotates, tracks quota, and ranks the account alongside whatever
else is configured.

There is no in-TUI OAuth flow for ChatGPT yet — only Anthropic's `l` does that.
Instead, import the credential Codex already has: log in with `codex login` as
usual, then from the Accounts screen press `i`, then `x`, to read
`~/.codex/auth.json` (an `apikey`-mode file, with no OAuth tokens to adopt, is
skipped rather than imported broken). `c` on the same menu still imports from
Claude Code's own credential file, as before.

Point Codex at the proxy by adding a provider to `~/.codex/config.toml`:

```toml
model_provider = "aiproxy"
[model_providers.aiproxy]
base_url = "http://127.0.0.1:3456/v1"
wire_api = "responses"
env_key = "AIPROXY_API_KEY"
```

`env_key` names an environment variable Codex insists on finding set, even
though aiproxy does not enforce its own API key for loopback callers (see
[Configuration](#configuration)) — `export AIPROXY_API_KEY=unused` before
running Codex is enough.

`GET /v1/models` is synthetic: aiproxy does not relay it anywhere. It answers
with the union of every enabled account's own discovered catalogue —
Anthropic's from `/v1/models`, ChatGPT's from the private
`wham/models?client_version=...` endpoint (there is no public one an OAuth
token has scope to call) — deduplicated by model id and shaped to satisfy
both an Anthropic-style and an OpenAI-style parser at once, so neither client
needs to be sniffed. `providers.openai.clientVersion` (default `"0.147.0"`,
see [Configuration](#configuration)) is sent on that private catalogue
request, which otherwise rejects the call outright; it is configurable
because a server-side version gate is a plausible way for catalogue discovery
to break between releases with no code change on this end to explain it.

Routing follows what each account's own catalogue says it can reach, not a
hardcoded table: a request naming a model gets routed only to accounts whose
discovered catalogue actually lists it, so plan differences (a model your
ChatGPT plan cannot see) are handled without any per-model configuration.

One consequence is deliberate rather than an oversight: routing decides which
account to use by model name alone, so a Messages-format request (the shape
Claude Code sends) that happens to name a GPT model will still select a
ChatGPT account — and then fail once it reaches the upstream Responses API,
which does not speak that shape. There is no Messages↔Responses translation
yet; Codex already speaks Responses and Claude Code already speaks Messages,
so each is served natively by the matching vendor's account today, and
cross-protocol routing is a separate piece of future work.

## Which account gets picked

Selection sorts eligible accounts by, in order: unpaused before paused, then
priority ascending, then **how much unused allowance is about to be wasted**,
then account ID as a deterministic backstop.

That third key is the interesting one. Each quota window is scored by the rate
you would have to spend at to avoid wasting it — headroom divided by the time
left before it resets — and an account is ranked by its most urgent window. The
effect is use-it-or-lose-it: given two accounts at the same priority, traffic
concentrates on whichever one is closest to throwing capacity away, rather than
being spread evenly or pinned to whichever happens to sort first by name.

Headroom is weighted by the window's own length, because `utilization` is a
fraction of that window's limit and the limits are not the same size. Losing 90%
of a weekly allowance is a much larger loss than 90% of a five-hour one, and the
weekly one is gone for a week where the 5h regenerates before the day is out.
The upstream never reports absolute limits, so window duration stands in for
relative capacity — an approximation that deliberately errs toward draining long
windows. Without the weighting the 5h window dominates the ranking, since it
almost always resets sooner, and an expiring weekly window is invisible.

Windows with no headroom left, unknown reset times, or reset timestamps already
in the past score nothing, so an imminent reset on an exhausted window is not
mistaken for an opportunity and an account carrying no quota data at all sorts
last instead of first.

This ranking is only as good as the quota data behind it. An account with no
observed buckets scores zero, which is why the probe renews credentials and runs
once at startup — see `quotaProbe.intervalSeconds` under
[Configuration](#configuration).

## Overloaded upstreams

Anthropic answers `529` with `overloaded_error` when the API itself is out of
capacity. It used to be relayed straight to your agent. It is now absorbed:
aiproxy waits and retries rather than handing the failure on.

The handling is deliberately different from a `429`, because the two mean
opposite things about what to do next. A `429` is about *your* account, so
rotating to another one is the fix. A `529` is about *Anthropic*, and every
account you own reaches the same exhausted upstream — so rotating just spends
another account's attempts on a problem none of them can solve, and throws away
the warm prompt cache on the account you were already using. A 529 therefore
retries **in place**: same account, no hold, no rotation.

Waits honour `Retry-After` when upstream sends one, and otherwise back off
1s → 2s → 4s → 8s, with the last step repeating. A `Retry-After: 0` on a 529 is
ignored in favour of the schedule — retrying an out-of-capacity API instantly is
what turns an overload into a stampede, so the one case where upstream asks for
exactly that is the case to override.

`retry.overloadedBudgetMs` (default 30000) caps the total. It is deliberately
*not* part of `budgetMs`: that budget protects the paths that recover a request
— rotation, credential refresh, throttle absorption — and those compete with
each other, which is why they share an allowance. Waiting out an overload
competes with nothing, so charging it to the same pool would let one 529 consume
the budget a later `401` needs to rotate, turning a transient overload into a
failure for an unrelated reason.

When the overload budget does run out, your agent receives Anthropic's real 529
and body rather than a proxy-invented error — Claude Code understands 529 and
has its own retry, and replacing it would hide what actually happened. Overloads
appear in the outcome breakdown as `overloaded`, separate from `server_error`,
because during an incident an overload storm and a genuine upstream fault are
the two things worth telling apart.

**Not covered:** Anthropic can also report `overloaded_error` as an SSE `error`
event *mid-stream* under a `200`. Once bytes have reached your agent the request
cannot be retried, so that case still surfaces. Only the 529 status is absorbed.

## Configuration

Config lives at `~/.config/aiproxy/config.json` (honouring `XDG_CONFIG_HOME`),
with the accounting database beside it at `metrics.db`. Both are written for
you on first run. The Settings screen edits a subset of it: `switchThreshold`,
`sessionAffinity` and `update.checkEnabled` apply immediately, everything else
is written to disk and marked `saved · restart to apply`, because the objects
that read those values are built once at startup.

```json
{
  "listen": { "addr": "127.0.0.1:3456", "apiKey": "ap-..." },
  "routing": { "switchThreshold": 0.98, "sessionAffinity": true, "blockedModels": [] },
  "retry": { "budgetMs": 10000, "inlineAbsorbMaxMs": 5000, "bodyIdleMs": 120000, "headerTimeoutMs": 60000, "overloadedBudgetMs": 30000 },
  "quotaProbe": { "intervalSeconds": 300 },
  "warming": { "enabled": true, "threshold": 0.5, "model": "claude-haiku-4-5-20251001" },
  "metrics": { "retentionDays": 90 },
  "mitm": { "enabled": true },
  "update": { "checkEnabled": true, "checkIntervalHours": 24 },
  "privacy": { "enabled": false },
  "providers": { "openai": { "clientVersion": "0.147.0" } }
}
```

- **`listen.apiKey`** is generated on first run and only enforced for
  non-loopback callers, so a local agent needs no credential.
- **`routing.switchThreshold`** is the utilization at which an account stops
  being selected. **`sessionAffinity`** keeps one conversation on one account
  while that account still has room. **`blockedModels`** takes globs. Accounts
  of equal priority are ordered by how much allowance they are about to waste —
  see [Which account gets picked](#which-account-gets-picked).
- **`retry.budgetMs`** bounds only the time *aiproxy* adds before the first
  byte — backoff, waiting on a paused account, absorbing a rate limit,
  refreshing a credential. **`headerTimeoutMs`** bounds one attempt's wait for
  upstream headers, which is the model's own thinking time. They are separate
  clocks on purpose; conflating them cancels healthy requests.
- **`retry.overloadedBudgetMs`** (default 30000) is a clock of its own, for
  waiting out Anthropic 529s. See [Overloaded upstreams](#overloaded-upstreams).
  Two more clocks sit outside `budgetMs` for the same reason: the privacy scan
  runs before the retry loop, bounded by `privacy.scanTimeoutMS`, and
  `bodyIdleMs` governs the stream after the first byte.
- **`quotaProbe.intervalSeconds`** defaults to 300 because the zero-spend usage
  endpoint is itself rate limited — polling it aggressively gets the probe
  throttled and leaves selection deciding on stale numbers. One probe runs at
  startup and then every interval, because quota is not persisted across
  restarts and selection cannot apply `switchThreshold` to an account whose
  utilization it does not yet know. Each probe renews the account's credential
  first, so a token that expired while the proxy was idle does not freeze the
  numbers until traffic resumes. Set it to `0` to disable the background loop
  entirely; `p` in the TUI still works.
- **`update.checkEnabled`** turns the daily release check on or off and takes
  effect immediately; **`checkIntervalHours`** takes effect on restart, because
  the checker's ticker is built once at startup. See [Updating](#updating).
- **`privacy`** is shown collapsed above; every key in it, and the fact that all
  of them are restart-gated, is in [Privacy filter](#privacy-filter).
- **`providers.openai.clientVersion`** is sent on every ChatGPT model-catalogue
  read; see [ChatGPT accounts](#chatgpt-accounts) for why it exists and what
  breaks without it.

## Flags

| Flag | |
|---|---|
| `--config <path>` | config location (default: XDG config dir) |
| `--addr <host:port>` | listen address, overriding config |
| `--headless` | run without the TUI, logging to stderr |
| `--log-level` | `debug`, `info`, `warn`, or `error` |
| `--version` | print version and exit |

Two subcommands: `aiproxy update` (see [Updating](#updating)) and `aiproxy
privacy install` / `aiproxy privacy status` (see [Privacy filter](#privacy-filter)).
Everything else is flags.

`--headless` is how you run it as a service. The TUI and stderr logging can't
share a terminal, so under the TUI logs feed the Activity screen's ring buffer
instead.

## Control API

Everything the TUI shows is read through `view.Source`, and every method on it
has exactly one route under the reserved `/_aiproxy/` prefix — which is also
what a stage-5 dashboard or a future detached daemon will read.

```
GET    /_aiproxy/api/v1/status
GET    /_aiproxy/api/v1/accounts
GET    /_aiproxy/api/v1/usage
GET    /_aiproxy/api/v1/totals
GET    /_aiproxy/api/v1/latency
GET    /_aiproxy/api/v1/quota/history
GET    /_aiproxy/api/v1/events          (SSE)
POST   /_aiproxy/api/v1/accounts/{id}/enabled
POST   /_aiproxy/api/v1/accounts/{id}/priority
DELETE /_aiproxy/api/v1/accounts/{id}
GET    /_aiproxy/api/v1/settings
POST   /_aiproxy/api/v1/settings
POST   /_aiproxy/api/v1/probe
POST   /_aiproxy/api/v1/update
POST   /_aiproxy/api/v1/accounts/import
POST   /_aiproxy/api/v1/accounts/login
```

Anything under the prefix that isn't a control route is a 404, never a proxied
request. Everything else on the listener is forwarded upstream.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
staticcheck ./...
```

CI runs vet, staticcheck, and the race suite, and cross-builds for
darwin/linux on amd64/arm64.

Releases are cut by tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` runs the suite, cross-compiles the four
targets with the version stamped in via `-ldflags`, writes `checksums.txt`,
and publishes the lot to a GitHub release. The asset naming
(`aiproxy_<version>_<os>_<arch>.tar.gz`) is load-bearing in three places:
the workflow writes it, `install.sh` constructs it, and so does
`internal/updater`'s `release` method. Change one and change all three.

## License

MIT
