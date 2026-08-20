# ChatGPT provider, per-account model catalogue, and a synthetic /v1/models

**Status:** approved for planning
**Date:** 2026-08-20
**Supersedes nothing. Precedes:** a separate spec for Messages↔Responses translation.

## 1. Goal

Run Codex through aiproxy on ChatGPT subscriptions, with the same account
rotation, quota tracking, ranking and warming that Anthropic accounts already
get. Add a synthetic `/v1/models` that lists what every logged-in account can
actually reach.

Explicitly NOT in this spec: translating between the Anthropic Messages API and
the OpenAI Responses API. Codex already speaks Responses and Claude Code already
speaks Messages, so each client can be served natively by an account of the
matching vendor without any translation existing. Cross-protocol routing is the
next spec.

A consequence, accepted deliberately: routing is by model name, so a Messages
request naming a GPT model will select a ChatGPT account and fail upstream. No
special handling is built for that. It stops being reachable when translation
lands, and a clear failure now is cheaper than a mechanism that gets deleted.

## 2. Evidence

Every endpoint below was exercised against live accounts on 2026-08-20 rather
than taken from documentation. Two of the four assumptions this design started
with were wrong, and were corrected here before any code was written.

**Confirmed working:**

| Purpose | Call | Result |
|---|---|---|
| ChatGPT quota | `GET chatgpt.com/backend-api/wham/usage` | full rate-limit + credits payload |
| ChatGPT models | `GET chatgpt.com/backend-api/wham/models?client_version=0.147.0` | 8 models for a Plus account |
| Anthropic models | `GET /v1/models` | 10 models |
| Minimal inference | `POST /v1/messages`, `max_tokens:1` | 200, 8 in / 1 out, no Claude Code system prompt needed |

**Confirmed NOT working — this killed the original design:**

`GET api.openai.com/v1/models` with a ChatGPT OAuth token returns *"Missing
scopes: api.model.read"*. Codex's token carries
`openid profile email offline_access api.connectors.read api.connectors.invoke`
and nothing more. The first draft of this design proposed discovering models via
"each vendor's own `/v1/models`"; for ChatGPT that is simply unavailable, and
`wham/models` is the substitute.

## 3. The OAuth flow

Taken from `openai/codex` source plus a live `~/.codex/auth.json`, which agree.

```
issuer        https://auth.openai.com
authorize     {issuer}/oauth/authorize
token         {issuer}/oauth/token
revoke        {issuer}/oauth/revoke
client_id     app_EMoamEEZ73f0CkXaXp7hrann
redirect_uri  http://localhost:1455/auth/callback   (fallback port 1457)
scope         openid profile email offline_access api.connectors.read api.connectors.invoke
```

Authorize query parameters: `response_type=code`, `client_id`, `redirect_uri`,
`scope`, `code_challenge`, `code_challenge_method=S256`, `state`, `originator`,
`id_token_add_organizations=true`, `codex_cli_simplified_flow=true`.

PKCE: 64 random bytes, base64url without padding, S256 challenge.

Token exchange is form-encoded: `grant_type=authorization_code`, `code`,
`redirect_uri`, `client_id`, `code_verifier`. Refresh is
`grant_type=refresh_token`.

Identity comes from the `https://api.openai.com/auth` claim namespace on the
id_token: `chatgpt_account_id`, `chatgpt_plan_type`, `chatgpt_user_id`, `poid`
(the org). Measured lifetimes: **access_token 10 days, id_token 1 hour** — far
longer than Anthropic's, which makes the existing refresh threshold generous
rather than tight.

The existing `provider.Provider` interface needs no change. `Login`, `Refresh`,
`Profile`, `Quota`, `Endpoint`, `Authorize`, `ClassifyResponse` all have real
implementations. This is the payoff from the seam already being provider-shaped.

## 4. Inference

```
POST https://api.openai.com/v1/responses
Authorization: Bearer <access_token>
chatgpt-account-id: <account_id>
originator: codex_cli_rs
```

The proxy relays the client's own body. Codex is pointed at aiproxy with:

```toml
model_provider = "aiproxy"
[model_providers.aiproxy]
base_url = "http://127.0.0.1:3456/v1"
wire_api = "responses"
env_key = "AIPROXY_API_KEY"
```

## 5. Quota

`GET chatgpt.com/backend-api/wham/usage` returns, on a live Plus account:

```json
{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": { "used_percent": 29, "limit_window_seconds": 604800,
                        "reset_after_seconds": 89259, "reset_at": 1787282195 },
    "secondary_window": null
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0" },
  "rate_limit_reached_type": null
}
```

Mapping to `provider.QuotaBucket`:

- **Name** derived from `limit_window_seconds`: 18000 → `5h`, 604800 → `7d`.
  Deriving rather than hardcoding matters because it makes `windowHours` and
  `expiringAllowance` work unchanged — an OpenAI account ranks against an
  Anthropic one on the same scale with no special case.
- **Utilization** = `used_percent / 100`.
- **ResetsAt** = `reset_at * 1000` (the field is unix seconds).
- **Status** = `"rejected"` when `limit_reached`.

> `used_percent` really is 0..100 here and MUST be divided by 100. This is the
> exact inverse of the Anthropic bug fixed in "stop dividing header utilization
> by 100", where the header was already a fraction. Two providers, two
> conventions; the division belongs at each provider's own parse boundary and
> nowhere else. Both directions need a test asserting a known input maps to a
> known fraction, because both directions have now been got wrong once.

A `null` `secondary_window` is normal (this Plus account has only a weekly
window) and must produce no bucket rather than a zero-valued one — a zero
utilization bucket with no reset would otherwise make a spent account look idle.

**Backup path.** Every Responses reply also carries
`x-codex-primary-used-percent`, `-primary-window-minutes`, `-primary-reset-at`,
the `secondary` equivalents, `x-codex-credits-*` and
`x-codex-rate-limit-reached-type`. `ClassifyResponse` parses these, so quota
stays current from live traffic even if `wham/usage` fails. This mirrors how the
Anthropic classifier already reads its unified headers.

**`wham/usage` is a private, undocumented endpoint.** It is the only zero-spend
read available, so it is used, but a failure must degrade to the header path and
be recorded — never fail a request and never silently serve stale numbers as
fresh. The prober already records per-account probe errors; this reuses that.

## 6. Per-account model catalogue

A new method on the provider seam:

```go
Models(ctx context.Context, c Credential) ([]Model, error)

type Model struct {
    ID          string // "claude-opus-5", "gpt-5.6-sol"
    DisplayName string
    ContextWindow int
}
```

- **Anthropic:** `GET /v1/models`, reading `id`, `display_name`,
  `max_input_tokens`.
- **ChatGPT:** `GET wham/models?client_version=<v>`, reading `slug`,
  `display_name`, `context_window`.

`client_version` is required — omitting it returns a validation error. It is a
config value with a shipped default, because a server-side version gate is a
plausible way for this to break and an operator needs to be able to move it
without waiting for a release.

Catalogues are discovered per account, cached beside `Buckets` in
`account.Manager`, refreshed on the prober's cycle, and **fail soft to the last
known list**. A discovery failure must not empty a catalogue: that would make
every model on that account unroutable and take the account out of service for
a reason unrelated to its health.

An account with no catalogue yet — freshly added, never probed — is treated as
eligible for any model rather than for none. Failing closed here would mean a
new account cannot serve traffic until its first probe completes, which is the
same startup-blindness the prober fix removed.

## 7. Routing

`Select` gains one eligibility filter: when `SelectRequest.Model` is set and the
account has a known catalogue, the model must be in it.

Everything else is unchanged — priority, the expiring-allowance ranking, session
affinity, pause and rate-limit holds, warming. Routing by discovered access
rather than by a name table means plan differences are handled for free: an
account without `gpt-5.6-pro` is simply not a candidate for it, with nothing to
configure.

If no account can serve a model, the existing `ErrNoAccount` path answers, and
the error names the model so the cause is obvious.

## 8. Synthetic /v1/models

Served by the proxy itself, not relayed. It is the union of every enabled
account's catalogue, deduplicated by id, sorted for stability.

The response uses a superset shape carrying both dialects' fields, so neither
client needs the caller to be sniffed:

```json
{ "object": "list", "data": [
  { "id": "claude-opus-5", "object": "model", "type": "model",
    "display_name": "Claude Opus 5", "owned_by": "anthropic",
    "created": 1753315200, "context_window": 1000000 }
]}
```

`object`/`owned_by`/`created` satisfy an OpenAI-shaped parser; `type`/`id`/
`display_name` satisfy an Anthropic-shaped one. This is a shape neither vendor
documents, which is the accepted cost of one code path and no caller sniffing.
A strict client that rejects unknown fields would break; none of the clients in
use do.

## 9. Config and TUI

```json
"providers": { "openai": { "clientVersion": "0.147.0" } }
```

Login gains a provider choice: `l` on the Accounts screen asks Anthropic or
ChatGPT before starting the flow. Credentials import from `~/.codex/auth.json`
gets an import source alongside the Claude Code one — the file layout is
`{auth_mode, tokens:{id_token, access_token, refresh_token, account_id}}`.

Note this brings back an account UUID on the import path, which the teamclaude
removal left with no producer. The dedupe-on-uuid branch in `ImportCredentials`
was deliberately left in place for exactly this.

## 10. Risks and open questions

**Warming may not apply.** The warmer assumes a window that starts on first use.
Whether OpenAI's windows are first-use-anchored or fixed is UNVERIFIED. The live
reading shows a 7d window at 29% with a concrete `reset_at`, which does not
distinguish the two. This must be settled before warming is enabled for ChatGPT
accounts; until it is, warming skips them. Warming a fixed-window account wastes
a request every cycle and gains nothing.

**Usage accounting needs an OpenAI `ParseUsage`.** The Responses API reports
tokens differently from Messages. Without it, cost and token metrics silently
read zero for ChatGPT traffic — the same failure class as an unhandled outcome
enum reading as "ok". Zero is indistinguishable from free, so this is a
correctness requirement, not a nicety.

**Private endpoints.** `wham/usage` and `wham/models` are internal ChatGPT
endpoints. They can change without notice. Both have a defined degradation:
usage falls back to response headers, models falls back to the last known
catalogue.

**Privacy filter.** `WalkStrings` is JSON-generic so redaction still functions
on Responses bodies, but the `SkipKey` list was tuned against Anthropic shapes.
It needs review against a real Responses body before the filter is trusted on
ChatGPT traffic.

**Two accounts, two vendors, one ranking.** `expiringAllowance` compares
accounts across providers once bucket names are derived consistently. This is
intended — priority still separates them if an operator wants Anthropic
preferred — but it means a ChatGPT account can outrank an Anthropic one for a
model only Anthropic serves. The catalogue filter prevents that from mattering.

## 11. Testing

- OAuth: PKCE challenge derivation against a known vector; authorize URL
  parameter-by-parameter; token and refresh request bodies; callback port
  fallback 1455 → 1457.
- Quota: the live payload above as a fixture, asserting 29 → 0.29 and
  604800 → `7d`; a null secondary window producing no bucket; a
  `limit_reached` producing `rejected`.
- Header backup: `x-codex-*` parsed into the same buckets as the JSON path, so
  the two sources cannot drift.
- Catalogue: discovery failure preserves the previous list; an account with no
  catalogue stays eligible; a model absent everywhere yields a named error.
- Routing: a model present on only one account routes there regardless of
  ranking; two accounts with the same model rank by the existing rules.
- `/v1/models`: union and dedupe across providers; the emitted object carries
  both dialects' required fields.
