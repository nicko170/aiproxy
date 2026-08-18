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
version:

```sh
BINDIR=~/bin AIPROXY_VERSION=0.1.0 curl -fsSL https://raw.githubusercontent.com/nicko170/aiproxy/main/install.sh | sh
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
proxy down with it). `l` starts a login, `p` forces a quota probe, and `o`
opens the dashboard.

Per-screen keys are shown in the footer: `space` pauses the activity feed,
`a`/`m`/`c` filter it by account/model/outcome and `v` toggles the log view;
`r` cycles the usage range (1h, 24h, 7d, 30d) and `g` cycles the grouping
(account, model, outcome); on Accounts, `e` toggles enabled, `+`/`-` change
priority, `x` removes, and `enter` opens detail.

## Configuration

Config lives at `~/.config/aiproxy/config.json` (honouring `XDG_CONFIG_HOME`),
with the accounting database beside it at `metrics.db`. Both are written for
you on first run; the Settings screen edits most of it live.

```json
{
  "listen": { "addr": "127.0.0.1:3456", "apiKey": "ap-..." },
  "routing": { "switchThreshold": 0.98, "sessionAffinity": true, "blockedModels": [] },
  "retry": { "budgetMs": 10000, "inlineAbsorbMaxMs": 5000, "bodyIdleMs": 120000, "headerTimeoutMs": 60000 },
  "quotaProbe": { "intervalSeconds": 300 },
  "metrics": { "retentionDays": 90 },
  "mitm": { "enabled": true }
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
  separate clocks on purpose; conflating them cancels healthy requests.
- **`quotaProbe.intervalSeconds`** defaults to 300 because the zero-spend usage
  endpoint is itself rate limited — polling it aggressively gets the probe
  throttled and leaves selection deciding on stale numbers. Set it to `0` to
  disable the background loop; `p` in the TUI still works.

## Flags

| Flag | |
|---|---|
| `--config <path>` | config location (default: XDG config dir) |
| `--addr <host:port>` | listen address, overriding config |
| `--headless` | run without the TUI, logging to stderr |
| `--log-level` | `debug`, `info`, `warn`, or `error` |
| `--version` | print version and exit |

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
(`aiproxy_<version>_<os>_<arch>.tar.gz`) is load-bearing — `install.sh`
constructs that exact string.

## License

MIT
