# Web dashboard

**Status:** approved for planning
**Date:** 2026-08-25
**Implements:** stage 5 of [2026-08-17-aiproxy-design.md](2026-08-17-aiproxy-design.md) §9,
with one change of premise recorded in §2.

## 1. Goal

Serve the dashboard §9 has always promised, and make it a complete substitute
for the TUI rather than a read-only companion to it.

The premise has changed since §9 was written. aiproxy is moving to a server, so
the operator loses day-to-day terminal access to the machine it runs on.
Everything reachable by pressing a key in the TUI must be reachable in a
browser: not only the charts, but adding an account, disabling one,
reprioritising, forcing a probe, and editing settings. A dashboard that only
draws graphs would strand account management on a box nobody logs into.

That change of premise brings two problems §9 never had to solve, and most of
this document is about them: a browser cannot present the `x-api-key` header the
control API expects, and the OAuth callback listener binds to the *server's*
loopback, not the operator's.

## 2. What already exists

Stage 3 built the whole control plane, and it is complete by construction:
`TestEveryViewSourceMethodHasAControlRoute` fails if a `view.Source` method ever
lacks a route. So this stage adds **no new query endpoints** — every screen
below is assembled from routes that exist and are already tested.

| Screen data | Route |
|---|---|
| header, uptime, in-flight, p95, probe, update, privacy | `GET /api/v1/status` |
| accounts, quota buckets, priority, in-flight | `GET /api/v1/accounts` |
| usage series | `GET /api/v1/usage?from&to&granularity&groupBy` |
| totals | `GET /api/v1/totals?from&to` |
| latency percentiles | `GET /api/v1/latency?from&to` |
| per-account quota history | `GET /api/v1/quota/history?account&from&to` |
| live activity | `GET /api/v1/events` (SSE) |
| settings read/write | `GET`/`POST /api/v1/settings` |
| account mutations | `POST .../{id}/enabled`, `.../{id}/priority`, `DELETE .../{id}`, `POST .../pin` |
| login | `POST .../accounts/login`, `POST .../login/{id}/code`, `GET .../login/{id}` |
| import, probe, update | `POST .../accounts/import`, `POST .../probe`, `POST .../update` |

Two additions are unavoidable and both are about serving a browser, not about
data: an asset handler, and a session endpoint (§4).

`view.Source`'s doc comment already anticipates this stage by name, and spec §2
invariant 4 — "the TUI and the dashboard read identical values" — is satisfied
structurally: the TUI reads `view.Source` in-process, the control API is a thin
adapter over the same seam, and the dashboard reads that. Neither computes an
aggregate of its own.

## 3. Architecture

A single-page app served from `embed.FS`. Vanilla JavaScript, hand-rolled SVG,
no build step, no CDN, no third-party runtime dependency — §9's constraint,
retained deliberately: the binary stays self-contained, the dashboard works
offline, and the repository stays a pure Go project that `CGO_ENABLED=0 go build`
produces in one step.

```
internal/web/
  web.go          embed.FS, asset handler, cache headers
  session.go      API key -> cookie exchange, cookie validation
  web_test.go
  assets/
    index.html    the shell: header, nav, one <main> the router fills
    app.js        router, fetch helpers, SSE client, screen registry
    chart.js      the shared chart primitives (§6)
    screens/      overview.js activity.js usage.js accounts.js settings.js login.js
    style.css     layout, theme, prefers-color-scheme
```

**Mount.** `GET /_aiproxy/` serves the shell and `GET /_aiproxy/assets/*` serves
the embedded files, both registered inside the existing reserved-prefix
subrouter in `router.go`. That subrouter already defines its own `NotFound` and
`MethodNotAllowed` — deliberately, so a future control path can never fall
through to the upstream — and the asset routes must be registered as siblings of
the API routes rather than by relaxing that handler. An unknown path under the
prefix stays a 404.

Routing is hash-based (`#/overview`, `#/usage`), which needs no server-side
catch-all and cannot be confused with a proxied upstream path. The SSE
connection is owned by `app.js`, not by the Activity screen, so it survives
navigation and the Activity view attaches to a buffer that is already filling.

`internal/web` imports `internal/view` and nothing below it — the same rule
`internal/tui` follows, enforced the same way by a test. It does not import
`internal/proxy`; the router mounts the web handler, not the other way round.

## 4. Authentication, and refusing to be open by default

Today `Authorized` returns true when no API key is configured, and exempts
loopback unconditionally. On a laptop that is right. On a public interface it
means account removal and OAuth login are exposed to anyone who can reach the
port, which is the state this stage would otherwise ship into.

Three changes, in order of importance:

**A bind guard.** `cmd/aiproxy` refuses to start when the listen address is not
loopback and no API key is configured, naming the fix. This is a startup error,
not a warning: a warning scrolls past, and the failure mode it prevents is a
stranger holding your Claude subscription. An explicit
`--insecure-no-auth` flag exists for someone who genuinely means it, e.g. behind
their own authenticating proxy, and it logs on every start.

**A session endpoint.** `POST /_aiproxy/api/v1/session` takes the API key in a
JSON body, compares it in constant time, and sets an `HttpOnly`, `SameSite=Strict`,
`Secure`-when-TLS cookie holding a random 32-byte token. `DELETE` on the same
path clears it. Sessions live in memory with a 30-day sliding expiry; a restart
logs everyone out, which is acceptable for a single-operator tool and avoids
persisting anything credential-adjacent to disk.

**`Authorized` grows a third path.** It currently takes `(remoteAddr,
presentedKey, configuredKey)` and is called from four places — the proxy
handler, the passthrough handler, the models handler, and the control API — all
of which must be updated together; it gains the request's session token. Order:
valid session, then matching key, then loopback exemption. The loopback
exemption stays so nothing about local use changes.

The API key never reaches JavaScript beyond the single login POST. It is not put
in a URL, `localStorage`, or a query parameter — a key in a URL lands in logs and
`Referer` headers.

## 5. Screens

Five, mirroring the TUI so the two stay learnable together, plus login.

**Overview** — the landing screen. Account cards with quota bars per bucket
(5h, 7d, per-model), reset countdowns, priority, in-flight, and status. Header
strip: listen address, uptime, in-flight, p95 TTFB, dropped-sample count,
update availability. Pin control, since forcing all traffic through one account
is now a supported operation and the TUI already exposes it.

**Activity** — the live feed over SSE: time, model, account, status, outcome,
duration, TTFB, tokens. Pausable, filterable by account/model/outcome, capped at
a bounded ring so a long-lived tab cannot grow without limit.

**Usage** — range selector (1h/24h/7d/30d), group-by toggle (account/model/
outcome), the stacked series, the totals table, and cost. Where `Totals.UnpricedRequests`
is non-zero the cost figure is annotated as incomplete rather than presented as
final — spec §7.4's rule, and the TUI already honours it.

**Accounts** — add via login or import, enable/disable, reprioritise, remove,
pin. Per-account detail with quota history from `quota/history`. Removal and
disable are confirmed; both are easy to hit by accident and one of them ends a
session's affinity.

**Settings** — the `view.Settings` fields, edited and persisted through the
existing POST. Fields the seam documents as restart-gated (privacy enablement,
update-check interval) are labelled as such in the UI rather than silently
appearing live.

**Login** — §8.

Every screen renders three states explicitly: loading, empty, and failed. A
failed fetch shows what failed and offers a retry; it never renders an empty
chart that reads as "zero traffic" when the truth is "the request errored".
That distinction is the same one `Totals.UnpricedRequests` exists to preserve.

## 6. The chart module

The five chart types in §9 — tokens over time stacked by model and by account,
cost over time, TTFB p50/p95, outcome mix, quota utilisation with reset markers
— are compositions of a much smaller set of primitives, and `chart.js` provides
the primitives rather than five bespoke charts:

- a time axis with sensible tick selection across 1h to 30d
- a linear value axis with unit-aware formatting (tokens, dollars, milliseconds,
  percent)
- stacked area, line, bar, and horizontal gauge marks
- a shared categorical colour ramp, assigned by stable key hash so an account
  keeps its colour across screens and reloads
- one hover/tooltip and crosshair implementation, shared

This is where "nicer than the TUI" actually comes from. Not from a library —
from consistent scales, one colour assignment, real hover behaviour, and pixel
alignment fixed once.

Charts are rendered as SVG built through the DOM, not by string concatenation:
values from the API become text nodes and attributes, never markup. Every chart
is also readable without colour — series are labelled directly or in a legend
with shape/order cues — because a stacked area distinguished only by hue is
unreadable to a meaningful fraction of operators and unreadable to everyone in
a screenshot printed in greyscale.

Implementation of this section follows the `dataviz` skill; this spec fixes
*what* is drawn and the properties it must hold, not the palette values.

## 7. Live data and refresh

Activity is SSE, using the existing endpoint. `app.js` owns the connection,
reconnects with backoff on drop, and shows a visible "reconnecting" state rather
than silently going stale — a live feed that has quietly stopped is worse than
one that says it stopped.

Every other screen polls its routes on an interval that runs **only when the tab
is visible** (`document.visibilityState`), and stops otherwise. A dashboard left
open on a second monitor overnight should not spend the night querying SQLite.
Intervals: status and accounts every 5s, usage and totals every 30s, settings
never (read on entry).

## 8. Login without a usable callback

`provider.Login` binds a loopback listener on port 1455 **of the aiproxy host**,
and the `redirect_uri` is fixed in the provider's registered OAuth app, so it
cannot be pointed at a browser on another machine. For a server deploy the
callback can never complete.

The web login flow therefore always uses the paste-the-code path, which already
exists for exactly this reason (`LoginSession.SubmitCode`, built for SSH):

1. `POST /api/v1/accounts/login` with the provider; the response carries the
   session id and the authorize URL.
2. The UI shows the URL as a link and a copyable string, and a field for the
   code the provider's page returns.
3. `POST /api/v1/accounts/login/{id}/code` submits it.
4. `GET /api/v1/accounts/login/{id}` polls to completion, surfacing the profile
   on success and the error text on failure.

One flow, identical local and remote, nothing to detect. The TUI keeps its
callback flow unchanged, so local behaviour does not regress.

## 9. Failure, degradation, and the shape of errors

The control API returns the proxy's own error envelope; the UI surfaces
`error.message` verbatim rather than replacing it with a generic string. That
message is frequently the only actionable thing available — spec §11's stance,
and today's `no eligible account` incident is the argument for it.

A `401` from any fetch means the session expired: the UI routes to login,
preserving the intended destination, rather than rendering a broken screen.

Without JavaScript the dashboard does not work, and says so in a `<noscript>`
block that points at the TUI. Progressive enhancement is not attempted; a
live-updating charting UI is not usefully renderable server-side, and pretending
otherwise would mean the second presentation layer §2 exists to prevent.

## 10. Testing

**Go side** — asset handler serves and sets cache headers; the embed contains
every file the shell references (a test walks `index.html` and `app.js` for
asset paths and asserts each resolves, so a renamed file cannot 404 only in
production); session create/validate/expire/delete; `Authorized` accepts a valid
session, rejects a forged or expired one, and still honours key and loopback;
the bind guard refuses a non-loopback listen with no key and permits it under
the explicit flag; `internal/web` imports only `internal/view`.

**JS side** — the chart primitives are written as pure functions from data to
geometry, which is what makes them testable at all: scale mapping, tick
selection at each range, stacking arithmetic, and the empty / single-point /
all-zero cases that divide by zero if unguarded.

They are exercised by `chart_test.js`, run through `node --test`. A Go test
shells out to `node` and **skips with a clear message when it is absent**, so
`go test ./...` stays green on a machine with no node and CI can install node to
get the coverage. Node is a test-time convenience, never a build dependency: the
shipped binary and `go build` remain untouched by it.

**End to end** — one test drives the real router with a stub `view.Source`,
fetches `/_aiproxy/`, and asserts the shell loads and a session round-trips.
Browser-level testing is out of scope; it would need a headless browser this
project deliberately does not depend on.

## 11. Risks and open questions

**Scope.** Five screens plus auth plus a chart module is the largest single
stage since the core proxy. It decomposes cleanly along screen boundaries and
the plan should sequence it so something useful ships early: assets and session
first, then Overview, then the chart module, then the remaining screens.

**The TUI and the dashboard now duplicate presentation logic** — in different
languages, against one seam. They cannot disagree about *numbers*, which is what
§2 protects, but they can disagree about labels, states and affordances. Adding
a `view.Source` method still requires a control route; it now also implies a
dashboard surface, and nothing enforces that third leg. Accepted for now, noted
so it is a decision rather than a drift.

**Sessions are in memory.** A restart logs the operator out. Correct for a
single-user tool; revisit only if aiproxy grows real multi-user access, which
would change more than this.

**`--insecure-no-auth` is a foot-gun by construction.** It is included because
refusing every non-loopback bind would break a legitimate reverse-proxy deploy.
It logs on every start, and the README will say plainly what it gives away.
