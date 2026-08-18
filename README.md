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

Running the model costs time: measured on darwin/arm64 CPU, roughly 0.5 ms
per token and about 4 bytes per token, so a 4 KB string takes on the order of
520 ms to scan. That is why `ner.maxScanBytes` defaults to 4096 — before the
cap existed, a 16 KB string took 2.84 s, serialized behind a mutex on every
other request using the model at the time. A string longer than the cap is
scanned by the model only up to the cap, and the truncation is logged, never
silent, so it isn't mistaken for a full scan. This only affects the model
tier: **the deterministic rules have no cap and always scan the whole
string**, so a credential or a denylisted value anywhere in a large file is
still caught.

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
count in the header, because a count is reassurance and you should never be
shown reassurance in place of a fault.

`scanTimeoutMS` (default 10000) bounds the whole request's scan. It is the only
ceiling on the latency the filter adds — scanning happens before the retry loop,
so `retry.budgetMS` doesn't cover it, and `ner.maxScanBytes` bounds one string
rather than one request. With the model tier off (the default) the rules are
microseconds and this never fires. With it on, 10 s is roughly nineteen
freshly-seen 4 KB strings; past that the scan expires and `onScanFailure`
decides what happens. Raise it if you'd rather wait than refuse. There's no
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

On a first run with no accounts configured, aiproxy adopts credentials it can
already find (`~/.config/teamclaude.json`) so you don't have to re-authorize
everything. You can also import on demand from the Accounts screen with `i`:
`c` reads Claude Code's own credential file (`~/.claude/.credentials.json`),
`g` reads a legacy config.

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
  "retry": { "budgetMs": 10000, "inlineAbsorbMaxMs": 5000, "bodyIdleMs": 120000, "headerTimeoutMs": 60000 },
  "quotaProbe": { "intervalSeconds": 300 },
  "metrics": { "retentionDays": 90 },
  "mitm": { "enabled": true },
  "update": { "checkEnabled": true, "checkIntervalHours": 24 },
  "privacy": { "enabled": false }
}
```

- **`listen.apiKey`** is generated on first run and only enforced for
  non-loopback callers, so a local agent needs no credential.
- **`routing.switchThreshold`** is the utilization at which an account stops
  being selected. **`sessionAffinity`** keeps one conversation on one account
  while that account still has room. **`blockedModels`** takes globs.
- **`retry.budgetMs`** bounds only the time *aiproxy* adds before the first
  byte — backoff, waiting on a paused account, absorbing a rate limit,
  refreshing a credential. **`headerTimeoutMs`** bounds one attempt's wait for
  upstream headers, which is the model's own thinking time. They are two
  separate clocks on purpose; conflating them cancels healthy requests. Note
  that the privacy scan is a *third*: it runs before the retry loop, so
  `budgetMs` does not cover it and `privacy.scanTimeoutMS` bounds it instead.
- **`quotaProbe.intervalSeconds`** defaults to 300 because the zero-spend usage
  endpoint is itself rate limited — polling it aggressively gets the probe
  throttled and leaves selection deciding on stale numbers. Set it to `0` to
  disable the background loop; `p` in the TUI still works.
- **`update.checkEnabled`** turns the daily release check on or off and takes
  effect immediately; **`checkIntervalHours`** takes effect on restart, because
  the checker's ticker is built once at startup. See [Updating](#updating).
- **`privacy`** is shown collapsed above; every key in it, and the fact that all
  of them are restart-gated, is in [Privacy filter](#privacy-filter).

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
