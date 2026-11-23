# aiproxy — long-lived (one-year) Anthropic login

**Date:** 2026-11-23
**Status:** proposed — awaiting approval
**Depends on:** `internal/provider/anthropic`'s PKCE flow and the shared login
seam (`view.Source.Login`, the control API's login registry, the TUI login
pane, and the web dashboard's login dialog).

## 1. Purpose

`claude setup-token` mints a one-year OAuth token for unattended use. The
installed Claude Code 2.1.330 binary does it in the ordinary PKCE flow with
two differences:

- the authorize request asks for an inference-only scope set, and
- the token exchange adds `expires_in: 31536000` (365 days).

aiproxy's own login asks for the full scope set and a one-hour token. That is
fine interactively, but a server install must be re-authenticated whenever
the claude.ai login reaches its time limit. This adds a **long-lived** option
to the Anthropic login so aiproxy can mint the same kind of token for itself.

The user has confirmed `user:profile` works with `expires_in` set. The
long-lived grant therefore keeps `user:inference` and `user:profile` and drops
the rest (`org:create_api_key`, `user:sessions:claude_code`,
`user:mcp_servers`, `user:file_upload`). Dropping `org:create_api_key` is the
load-bearing part: per public reimplementations of `setup-token`, profile and
API-key scopes do not allow a custom expiry.

### In scope

- A second **Anthropic login mode**: same flow, reduced scope, one-year token.
- A visible toggle on the Anthropic login UI in both the web dashboard and the
  TUI. Changing it regenerates the authorize URL, because the scope is part of
  that URL.
- A persisted `longLived` marker on the credential. It is a static bearer:
  no refresh token is stored, and the only renewal is a fresh login.
- Actual token lifetime shown on the account (web card and TUI detail) and
  logged when it is shorter than asked. If the server ignores `expires_in`,
  the operator learns it at login, not a year later.
- Normal Anthropic login is unchanged, byte for byte in its request shape.
- OpenAI login does not grow a long-lived mode; asking for one is an error.

### Out of scope

- Pasting a token minted by `claude setup-token` elsewhere. A later addition
  can reuse the same credential marker.
- Refreshing a long-lived token. Setup tokens come without a refresh token
  and do not refresh; this is the accepted tradeoff for a year between
  logins. Renewal is a fresh login, and the UI shows the expiry so that is
  never a surprise.
- Header-based quota accounting for inference-only tokens. Not needed: the
  long-lived grant keeps `user:profile`, so `/api/oauth/usage` and
  `/api/oauth/profile` keep working.
- Making long-lived the default, or changing existing accounts' credentials.
- Any change to how tokens are used on the wire: the credential is still an
  OAuth bearer.

## 2. Behaviour

### Standard login (unchanged)

- Scope: `org:create_api_key user:profile user:inference
  user:sessions:claude_code user:mcp_servers user:file_upload`.
- Code exchange: no `expires_in`.
- Refresh: current request shape, no `expires_in`, no `scope`.
- Credential `longLived` absent/false.

### Long-lived login

- Authorize URL scope: `user:inference user:profile`.
- Code exchange body adds `"expires_in": 31536000`.
- The credential is stored with an **empty refresh token**, whatever the
  response carries, and is marked `longLived: true`. Setup tokens do not
  refresh; storing a refresh token we must not use would only invite a later
  path to try.
- Profile read and account persistence are unchanged: `user:profile` is in the
  grant, and a login without an identity is the same failure it is today.
- No refresh path exists for it:
  - `anthropic.Refresh` returns a permanent rejection for a long-lived
    credential instead of calling the token endpoint.
  - `account.Manager.EnsureFresh(force)` marks a long-lived account errored
    with "long-lived token cannot be refreshed; log in again" and returns a
    credential-rejection error, so a 401 sidelines it in one attempt rather
    than retrying a dead token.
  - A fresh login through `UpdateCredential` clears that errored state, as it
    does today for any re-login.
- If the login's expiry is materially shorter than requested (under 300
  days), aiproxy logs a warning naming the actual lifetime. It still
  succeeds; the operator sees that the server downgraded it.

### Where the warning is emitted

`internal/provider/anthropic` stays silent, as it is today: `Login` never
logs, and the provider seam has no logger. The comparison happens in
`cmd/aiproxy`'s `onLoginSuccess` hook, which receives the credential and
profile and already has the process logger in scope.

### Lifetime visibility

`view.Account` gains `LongLived bool` and `ExpiresAt int64` (unix ms), both
derived from the credential's non-secret metadata; the token itself still
never crosses the seam. The web account card shows a `token` fact for a
long-lived account ("1-year · expires 2027-11-23"), and the TUI account
detail shows the same. Standard accounts render as they do today.

### UI toggle semantics

The toggle is per login session, not a global setting.

- **Web:** after choosing Claude, the dialog shows the URL plus a
  "Long-lived (1 year)" checkbox. Toggling it cancels the current pending
  session and begins a new one with the new mode, replacing the displayed
  URL; polling follows the new session. Closing the dialog cancels the
  pending session.
- **TUI:** the Anthropic login pane shows `ctrl+t long-lived: off/on`.
  `ctrl+t` is used rather than a bare letter because the paste-the-code input
  has focus and would otherwise swallow the keypress. Pressing it cancels the
  current flow and starts another in the other mode. The pane title says when
  the mode is on.
- **Control API:** `POST /accounts/login` gains an optional `longLived`
  boolean; `DELETE /accounts/login/{sessionId}` cancels and releases a pending
  session. Both are transport-level additions, like the existing code/poll
  routes: there is no new `view.Source` method for the delete.

## 3. Architecture and changes

| Layer | Change |
|---|---|
| `internal/provider` | `LoginOptions{LongLived bool}`; `Provider.Login(ctx, LoginOptions)`. `Credential` gains `LongLived bool` (`json:"longLived,omitempty"`). |
| `internal/provider/anthropic` | `Login` chooses the scope set from options, adds `expires_in` to the exchange when long-lived, stores `LongLived` with no refresh token; `Refresh` refuses a long-lived credential. |
| `internal/provider/openai` | `Login` accepts the options and returns `ErrUnsupported` when `LongLived` is set. |
| `internal/view` | `Source.Login(ctx, providerName, provider.LoginOptions)`. `Local` passes them through. `Account` gains `LongLived` and `ExpiresAt`, mapped from the credential. |
| `internal/account` | `Manager.EnsureFresh(force)` sidelines a long-lived credential instead of attempting a refresh it cannot perform. |
| `cmd/aiproxy` | `onLoginSuccess` warns when a long-lived login comes back short. |
| `internal/proxy` | Begin body accepts `longLived`; registry passes options; new `DELETE` route cancels a session. |
| `internal/web/assets/screens/login.js` | Long-lived checkbox, session restart, delete on close. |
| `internal/web/assets/screens/overview.js` | Long-lived token fact on the account card. |
| `internal/tui` | `loginState.longLived`; `ctrl+t` restarts the flow; title line; token line in account detail. |
| `docs`/README | One paragraph under logging in. |

No new package. The reduced scope and `expires_in` field are the only request
differences; the empty refresh token and the `longLived` marker are the only
persisted differences.

## 4. Invariants

1. **Normal login is untouched.** Its authorize URL, exchange body, refresh
   body, and resulting credential are exactly what they are today.
2. **The mode is visible in the URL.** The long-lived authorize URL contains
   `user:inference user:profile` and no other scope.
3. **A long-lived credential is never refreshed.** It is stored without a
   refresh token, `Refresh` refuses it, and a forced refresh sidelines the
   account with a re-login message. Only a fresh login renews it.
4. **A mode change never leaves two live login sessions.** The old session is
   cancelled before the new one begins.
5. **The credential never crosses the UI seam.** Only the profile, the URL,
   and the credential's non-secret expiry/long-lived metadata cross it; the
   token strings never do.
6. **The provider layer still never logs.** Warning emission lives in the
   caller that owns a logger.

## 5. Credential and identity

`Credential.LongLived` is persisted in `config.json` and copied by every
credential copy path. A long-lived credential's `RefreshToken` is empty by
construction, which is also why `Manager.EnsureFresh`'s ordinary "nothing to
refresh" branch never invokes the token endpoint for it. Identity is read from
`/api/oauth/profile` exactly as it is for a standard login, so dedupe on
`Identity.AccountUUID` and the TUI/web labels are unchanged. `view.Account`
carries only `ExpiresAt` and `LongLived` from the credential; access and
refresh tokens remain below the seam.

## 6. Testing

- Provider: long-lived authorize URL scope; exchange body contains
  `expires_in`; credential is marked long-lived **and has no refresh token
  even when the response includes one**; profile is still read and
  `OnLoginSuccess` still runs; standard login bodies are unchanged.
- Refresh: `anthropic.Refresh` rejects a long-lived credential without a
  network call; standard refresh is unchanged.
- Manager: a forced `EnsureFresh` on a long-lived account marks it errored
  with the re-login message and returns a credential rejection; a successful
  `UpdateCredential` clears that state.
- OpenAI: `LongLived` returns `ErrUnsupported`.
- Control API: begin passes `longLived` through; DELETE cancels; unknown
  session is 404.
- Config: `longLived` round-trips.
- View: `Local` maps `LongLived`/`ExpiresAt` into `view.Account` and still
  never exposes the tokens.
- TUI: `ctrl+t` restarts with the other mode and cancels the old session;
  title reflects it; account detail shows the token expiry.
- Manual: build, log in on the server with the toggle on, confirm the account
  card shows a roughly one-year expiry and that quota still reads.

## 7. Release

Merge to `main`, then tag `v0.6.0`; the existing release workflow builds
darwin/linux amd64/arm64 and publishes checksums. The operator updates with
`aiproxy update` (or the installer) and tests the toggle on the server.
