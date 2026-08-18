# aiproxy — local privacy filter

**Date:** 2026-08-18
**Status:** approved, pre-implementation
**Depends on:** the buffered request body in `internal/proxy.proxyHandler`, the
streaming relay in `internal/proxy.Relay`, and the download-verify-rename path
in `internal/updater` (reused for model assets).

## 1. Purpose

aiproxy sits between a coding agent and Anthropic and sees everything the agent
sends: file contents, diffs, terminal output, tool results. Some of that should
never leave the machine.

This adds a filter that detects sensitive spans locally, replaces them with
stable placeholders before the request goes upstream, and restores the original
values in the response on the way back to the agent. The agent behaves as
though nothing happened; Anthropic never receives the value.

### In scope

Three classes of sensitive data, in the operator's stated priority:

- **Credentials and secrets** — API keys, tokens, passwords, private keys,
  connection strings, `.env` contents.
- **Personal data in prose** — names, emails, phone numbers, postal addresses,
  dates of birth, account numbers, as they appear in fixtures, database dumps,
  support tickets, and CSVs.
- **Internal identifiers** — hostnames, internal URLs, bucket names, project
  codenames, supplied by the operator as a denylist.

Delivered as two tiers behind one interface:

- **Tier 1** — deterministic detection: a rule table, a narrow entropy
  detector, and the operator denylist. Pure Go, no new dependency, microseconds.
- **Tier 2** — [`openai/privacy-filter`](https://huggingface.co/openai/privacy-filter)
  (Apache 2.0, 1.5B total / 50M active, 128K context, F1 97.43% on
  PII-Masking-300k) run in-process through a vendored purego binding to
  ONNX Runtime, for the prose PII that rules cannot see.

### Out of scope

- **Protecting proprietary source code.** Redaction cannot help here: the agent
  needs the code to do its work. Restricting *which* code reaches the proxy is a
  scoping problem, not a filtering one.
- **Modifying the client's system prompt** to explain placeholders. It is the
  client's prompt, and injecting into it changes the prompt-cache prefix. The
  placeholder format is self-describing instead (§5).
- **Restoring across process restarts.** The restore table is per-request by
  construction (§5.3); nothing needs to survive.
- **Non-Anthropic providers.** The detection and restoration layers are provider
  agnostic, but only the Anthropic request and SSE shapes are implemented.
- **Egress blocking.** This filter transforms requests; it does not decide
  whether a request may be made at all.

## 2. Non-negotiable properties

Each has a test (§13).

1. **Redact-then-restore is the identity.** For any input, the bytes the agent
   receives are the bytes upstream would have produced had nothing been
   redacted. A botched restoration writes a corrupted file to the operator's
   disk, which is worse than the leak the filter exists to prevent.
2. **Restoration is boundary-agnostic.** A placeholder split across any number
   of SSE events, at any byte offset, restores correctly.
3. **A stream with no placeholder sentinel is passed through byte-for-byte,
   with no added buffering.** `Relay` exists to flush every chunk as it
   arrives; the filter must not turn a token stream into a batch.
4. **Nothing outside a replaced span is altered.** The request that goes
   upstream differs from the client's request only in the replaced spans — no
   re-serialization, no key reordering, no whitespace changes.
5. **No plaintext sensitive value is written to disk, or retained beyond the
   request that carried it.** The scan cache holds byte offsets, never values
   (§7).
6. **A collision never restores the wrong value.** Two distinct values that
   hash to the same placeholder are detected and disambiguated, or the request
   fails (§5.2).
7. **The filter is never *silently* absent.** With the filter enabled and
   unable to run, the configured failure mode applies and the condition is
   visible in `Status` and the UI. `open` mode may send an unfiltered request —
   that is its stated purpose — but never without recording that it did.

## 3. Architecture

```
                    ┌─────────────────────────────────────┐
client request ────▶│ proxyHandler: body buffered         │
                    │   redact once, before the retry loop│
                    └──────────────┬──────────────────────┘
                                   │ redacted body + restore table
                                   ▼
                         Attempter (retries, rotation)
                                   │
                                   ▼  upstream response
                    ┌─────────────────────────────────────┐
client response ◀───│ Relay: streaming restore transform  │
                    └─────────────────────────────────────┘

internal/privacy            pipeline, placeholders, JSON walker, cache
internal/privacy/rules      Tier 1 detectors (pure Go)
internal/privacy/ner        Tier 2 detector (ONNX via vendored purego)
internal/privacy/onnxrt     vendored ONNX Runtime binding
```

`internal/privacy` imports the standard library and `internal/config`. It does
not import `internal/proxy`; `internal/proxy` calls into it. That keeps the
whole detect-redact-restore path testable without a proxy, a network, or a
model.

### 3.1 Hook points

**Redaction happens once, in `proxyHandler`, immediately after the body is
buffered and before `proxy.Request` is constructed.** Not inside the attempt
loop: `Attempter` replays `req.Body` on every retry and `prov.RewriteBody`
already rewrites it per attempt for model mapping. Redacting per attempt would
produce different placeholders on a retry, which breaks the prompt cache and
means the restore table no longer matches what was sent.

Redaction runs *after* `ParseModel` and the blocked-model check, so routing and
policy decisions are made on the client's own values. Nothing in §4.1's scanned
set could change them — `model` is never scanned — but fixing the order removes
the question.

The restore table has to reach `Relay`, which is called deep inside
`attempt.go`. Two additions carry it: a `Restore *privacy.Table` field on
`proxy.Request`, and the same on `RelayOptions`, passed through where
`Attempter` already builds its `RelayOptions`. A nil table means no filtering
and the relay does no rewriting at all — which is what keeps the filter's cost
exactly zero when it is disabled.

**`passthroughHandler` is excluded outright.** Those paths
(`proxy.DefaultPassthroughPrefixes` — OAuth, `/v1/code/`) relay the client's own
credential. Redacting a credential the upstream must verify breaks
authentication, and those bodies are not model context.

## 4. The request side

The body is JSON. Neither obvious approach is correct on its own:

- Matching raw bytes fails because a value's on-the-wire bytes are
  JSON-escaped; the detector must see the decoded value.
- Decoding, mutating, and re-marshalling fails property 4: Go's
  `encoding/json` sorts map keys and rewrites whitespace, so the request stops
  being the client's request.

So: a **JSON string-literal walker** built on `json.Decoder.Token()` plus
`InputOffset()` (standard library only) yields, for every string value in the
document, its byte span in the original body and its decoded value. Detectors
run on decoded values. Replacements are spliced back into the original bytes
with correct escaping, **applied last-span-first** so that rewriting one span
never invalidates the offsets of those before it. Every byte outside a replaced
span survives untouched.

### 4.1 Which strings are scanned

**Every string value except a denylist of structural keys.** An allowlist of
JSON paths would silently stop covering a field the day Anthropic adds one; a
denylist fails in the safe direction — it scans more than necessary, never
less.

Never scanned:

| Key | Why |
|---|---|
| `model` | Redacting the model name breaks routing and selection. |
| `type`, `role`, `id`, `name`, `stop_reason`, `stop_sequence` | Protocol enums and identifiers; a placeholder here is a malformed request. |
| `anthropic_version`, `anthropic_beta` | Protocol negotiation. Rewriting `anthropic-beta` costs the 1M-token context (see `TestBetaHeaderAndModelReachUpstreamUnaltered`). |
| `cache_control` and its children | Cache directives, not content. |
| `data` under an `image`/`document` source | Base64 payloads: megabytes of maximum-entropy text that would trip the entropy detector and dominate scan time. |

Strings shorter than 8 bytes are skipped **by the rule and entropy detectors
only**: no credential format fits, and most strings in a request are short
protocol values. The operator denylist (§6.5) is exempt — an internal codename
or short hostname can easily be under 8 bytes, and silently not matching a
literal the operator explicitly asked for would be the worst kind of failure
here.

## 5. Placeholders

```
[[AIPROXY_SECRET_a1b2c3d4]]
  │        │      └── first 8 hex of HMAC-SHA256(installKey, decodedValue)
  │        └───────── the finding's label, so the model can still reason
  └────────────────── sentinel: [[AIPROXY_
```

ASCII only — a non-ASCII sentinel invites tokenizer and encoding trouble for no
benefit. Labels: `SECRET`, `EMAIL`, `PHONE`, `ADDRESS`, `PERSON`, `URL`,
`DATE`, `ACCOUNT`, `ID`. Recognised by
`\[\[AIPROXY_[A-Z]+_[0-9a-f]{8,12}\]\]`. The longest form is 33 bytes
(`[[AIPROXY_` 10 + label ≤8 + `_` 1 + 12 hex + `]]` 2); the recogniser and the
streaming restorer share one constant, `maxPlaceholderBytes = 40`, which carries
headroom for a longer label so adding one is not a silent stream bug.

The label is included deliberately. `[[AIPROXY_EMAIL_...]]` lets the model
write "update the email address" sensibly; an opaque blob leaves it guessing.

### 5.1 Why a keyed hash of the value

- **Stable.** The same value always yields the same placeholder, so a redacted
  prefix is byte-identical across turns and Anthropic's prompt cache still
  hits. A positional counter (`SECRET_1`, `SECRET_2`) renumbers when content
  shifts, silently multiplying cost.
- **Keyed.** An unkeyed hash of a known-format value — an AWS access key ID has
  ~20 bits of real entropy after its prefix — is brute-forceable from the
  placeholder alone. The key defeats that.
- **Not reversible.** The placeholder discloses only that this value equals a
  value seen before. That residual is accepted in exchange for cache stability,
  and is recorded here as a known disclosure rather than left implicit.

The key is 32 random bytes in `~/.config/aiproxy/privacy.key` (0600), created
on first use. Deliberately not in `config.json`: that file is rendered in the
Settings screen and rewritten on every mutation, and a key belongs in neither.

### 5.2 Collisions

Within one request, the restore table maps placeholder → plaintext. Before
inserting, the pipeline checks whether that placeholder already maps to a
*different* plaintext. If so it widens that finding's suffix to 12 hex and
retries. If the 12-hex form also collides — which requires a deliberate
adversarial construction — the request fails under the configured failure mode.
Restoring the wrong secret is never an option.

### 5.3 Why the restore table is per-request

The model can only reference a placeholder it was shown, and everything it was
shown was in the request we just redacted — including any prefix served from
Anthropic's cache, since `cache_control` avoids re-billing content we still
sent. So the table built during redaction is complete by construction. It lives
in memory for the life of the request and is dropped when the response
completes. No persistence, no encrypted store, no new place for secrets to sit.

One consequence is self-inflicted and permanent: if a restoration ever fails
and a placeholder reaches the client, the client's conversation history keeps
that literal. Later turns then carry the placeholder rather than the plaintext,
and it stays visibly wrong rather than resolving to something else. That is the
intended behaviour of the `passthrough` failure mode (§12).

## 6. Detection

```go
// Finding is one sensitive span within a single decoded string.
type Finding struct {
    Start, End int     // byte offsets into the decoded string
    Label      string  // SECRET, EMAIL, ... — selects the placeholder label
    Rule       string  // which rule or model label fired, for the UI and logs
    Confidence float64 // 1.0 for deterministic rules
}

type Detector interface {
    Name() string
    Scan(ctx context.Context, text string) ([]Finding, error)
}
```

Detectors are independent and composable. Tier 2 is one more implementation of
this interface and the pipeline cannot tell the difference.

### 6.1 Overlap resolution

Detectors may report overlapping spans. The pipeline sorts findings by start
ascending, then length descending, then detector registration order, takes the
first at each position, and drops anything overlapping it. Fully deterministic —
the third key exists so two detectors reporting the identical span always
resolve the same way — and it prefers the longer span, the connection string
over the password inside it.

### 6.2 Tier 1: the rule table

A data table of `{name, label, pattern, minEntropy, validator}`, so adding a
rule is adding a row. Covering at minimum:

AWS access key IDs (`(A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}`) and
contextual secret access keys; GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`,
`ghr_`, `github_pat_`); `sk-ant-` and `sk-`/`sk-proj-`; Slack
(`xox[baprs]-`); Stripe (`sk_live_`, `rk_live_`); Google API keys
(`AIza[0-9A-Za-z_-]{35}`); PEM private-key blocks; JWTs
(`eyJ…\.…\.…`); credentials embedded in URLs
(`scheme://user:pass@`); Postgres/MySQL/Mongo/Redis connection strings
carrying a password; `.env` assignments
(`^[A-Z][A-Z0-9_]*_(KEY|TOKEN|SECRET|PASSWORD)=`); and generic assignments
(`(?i)(api[_-]?key|secret|token|password|credential)\s*[:=]\s*["']?(…)`).

### 6.3 Entropy, narrowly

A bare high-entropy string in source code is usually a hash, a UUID, or a
fixture — not a secret. So entropy is a *qualifier*, not a detector: it applies
only inside the capture group of a contextual rule, or to standalone base64/hex
runs of at least 24 characters with Shannon entropy at or above 4.0 bits/char
(base64) or 3.0 (hex).

### 6.4 Allowlist

Checked before any finding is accepted, because a false positive on a commit SHA
replaces it with a placeholder and derails the agent: UUIDs, 40- and 64-char hex
(git SHAs, content hashes), semver strings, RFC 2606 example domains, RFC 5737
example addresses, and obvious non-secrets (`YOUR_API_KEY`, `changeme`,
`xxxxx`, `<redacted>`).

### 6.5 The operator denylist

Literal strings and `/regex/` forms from config, labelled `ID`. This is what
covers internal identifiers — hostnames, buckets, codenames — which no general
model can know about.

## 7. The scan cache

Detection is the expensive part; splicing is microseconds. Claude Code resends
the entire conversation on every turn, so without caching a hundred-turn session
would push tens of megabytes through the Tier 2 model repeatedly, which is not
merely slow but unusable.

**Key:** `SHA256(rulesetVersion ‖ modelVersion ‖ activeRuleToggles ‖ enabledLabels ‖ decodedString)`.

`activeRuleToggles` is load-bearing rather than defensive: `rules.builtinSecrets`
and `rules.entropy` are live-tunable (§10), so without them in the key, turning
entropy off would leave its findings cached and still being applied.
**Value:** the `[]Finding` for that string — byte offsets and labels, nothing
else.

Three properties follow:

- **No extra plaintext retention.** On a hit, the pipeline skips the detectors
  and splices using the plaintext already present in the current request body.
  A cache of secrets held in RAM for an hour is a materially worse thing to own
  than a cache of byte offsets, given core dumps and swap. This is property 5.
- **Per-string, not per-turn.** The JSON walker already yields each string
  separately, so every prior turn's blocks hit and only genuinely new content is
  scanned. The tempting alternative — "only scan the last few messages" —
  reaches the same result by *assuming* earlier content was already scanned by
  us, which is false on a fresh start, after an eviction, or when a client
  replays an old conversation into a new process. A filter that skips work on
  that assumption leaks. Hashing the whole `messages` array is the opposite
  failure: one key that changes every turn and therefore never hits.
- **Bounded LRU, not a short TTL.** Content → findings is a pure function and
  never goes stale, so a TTL buys no correctness — only memory bounding, which
  an LRU does better. A 60-minute expiry would evict entries that are *still
  being resent every turn*, producing a rescan storm mid-conversation exactly
  when the context is largest. The version salt in the key makes a rule change
  or model upgrade invalidate everything automatically, so nothing depends on
  expiry for freshness.

Default bound: 50 000 entries. Findings are almost always empty, so this is a
few megabytes. `Status` reports the hit rate (§11) because a collapsed hit rate
is the first symptom of a cache-key bug.

## 8. The response side

This is the highest-risk component in the design.

Token boundaries do not respect placeholders, so a placeholder *will* arrive
split across SSE events:

```
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"your key [[AIPRO"}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"XY_SECRET_a1b2c3d4]] is stale"}}
```

Two events, two JSON strings. No scan of a single event's bytes can see it.

### 8.1 The rewriter

One rewriter per content block, keyed by the event's `index`, holding a pending
tail. It emits every byte that cannot begin a sentinel and holds back at most
`maxPlaceholderBytes - 1` = 39 bytes (§5). When a complete placeholder
is recognised it is replaced from the restore table; when a candidate is ruled
out the held bytes are released immediately.

Because the sentinel is `[[AIPROXY_`, holdback engages only when a chunk ends
mid-sentinel — in practice, a trailing `[`. Ordinary prose and code stream
through with nothing withheld, which is property 3.

`content_block_stop` flushes any remaining tail and discards the rewriter. If
the stream errors mid-block, `Relay` already severs it deliberately rather than
finishing cleanly; up to `maxPlaceholderBytes - 1` held bytes are lost with the truncated
stream, which is consistent and not worth special handling.

### 8.2 Which events are rewritten

| Event / delta | Field | Notes |
|---|---|---|
| `content_block_delta` / `text_delta` | `delta.text` | The common path. |
| `content_block_delta` / `input_json_delta` | `delta.partial_json` | **How the agent writes files.** See §8.3. |
| `content_block_delta` / `thinking_delta` | `delta.thinking` | Extended thinking can quote context. |
| `content_block_start` | complete `text` / tool `input` | Non-empty blocks may arrive whole. |
| everything else | — | `message_start`, `message_delta`, `message_stop`, `ping`, `error` pass through untouched. |

A non-streaming response is easier: the whole body goes through the same JSON
walker as the request (§4), with replacement in place.

Usage parsing in `Relay` is unaffected: the rewriter touches only the delta
fields above, and never the `message_delta` events usage rides on. One
consequence to record: `Result.Bytes` becomes the post-restoration count, so
recorded bytes are what the client received rather than what upstream sent.

### 8.3 `input_json_delta`, and two levels of escaping

`partial_json` fragments concatenate into the tool-input JSON document — the
`content` argument of a file write, for instance. A placeholder inside that
document sits inside a JSON string literal, and the plaintext replacing it may
contain `"`, `\`, or newlines. So the plaintext must be escaped for the inner
document, and that result escaped again for the `partial_json` string in the
SSE event.

No JSON state machine is needed to know the escaping context: a placeholder can
only appear where redaction put one, which is inside a string value. On a match
we are inside a string literal by construction, so the substitution always
escapes as JSON string content. (A model emitting a placeholder outside a string
literal would already be producing invalid JSON; that case is not handled.)

This is where a mistake corrupts a file instead of leaking one, so it carries
the heaviest tests (§13).

## 9. Tier 2: the model

`internal/privacy/ner` implements `Detector`.

### 9.1 Runtime

The vendored binding in `internal/privacy/onnxrt`, derived from
[onnxruntime-purego](https://github.com/shota3506/onnxruntime-purego), calls
ONNX Runtime through `purego`, so `CGO_ENABLED=0` still holds and the release
pipeline keeps cross-compiling four targets from one runner. Vendoring is the
point: the upstream project warns its API may change without notice, and a
pinned, patchable copy removes that from the critical path.

Two consequences follow from linking a native library into the proxy, and both
are handled rather than argued away. A segfault inside ONNX Runtime is not
recoverable in Go, so the runtime version is pinned to a known-good build, input
sizes are hard-bounded before any tensor is constructed, and the session is
created once and reused. And the model loads **lazily** on first use, so a
proxy with the filter disabled never dlopens anything.

### 9.2 The tokenizer gate

**This is the largest unknown in the design and it is sequenced first.**

The model reports spans over its own tokenization, and the pipeline needs
character offsets into the original string. A tokenizer that disagrees with the
reference implementation by one character produces spans that redact the wrong
bytes — silently, and in a component whose whole purpose is trustworthiness.

So the first implementation task is a Go tokenizer for the model's
`tokenizer.json` that reproduces reference offsets **exactly** on a fixture set
generated from the reference implementation, covering multi-byte UTF-8,
combining characters, and long inputs. Nothing downstream of the tokenizer is
built until that passes. If it cannot be made to pass, Tier 2 stops there and
Tier 1 ships alone; that is an acceptable outcome and a far better one than
approximate offsets.

### 9.3 Inference

- **Chunking.** Inputs are split to a bounded window with overlap so a span
  straddling a boundary is still seen, and findings are de-duplicated by
  `(start, end, label)` after offsets are mapped back.
- **Decoding.** BIOES tags with constrained Viterbi, using the transition
  parameters in `viterbi_calibration.json`.
- **Label filtering.** Only labels enabled in config produce findings.
- **Bounds.** `maxScanBytes` (default 4 KiB) caps what any one string
  contributes; anything longer is scanned up to the cap and the truncation is
  reported, never silently dropped. This is a LATENCY bound, not a size limit:
  measured on darwin/arm64 CPU the model costs ~0.5 ms/token at ~4 bytes/token,
  so 4 KB scans in ~520 ms, 16 KB in ~2.84 s, and the 256 KiB this spec
  originally specified extrapolated to ~45 s inside a single `Scan` — with the
  runner serialised by a mutex, a head-of-line block on every other request.
  The recall tradeoff is accepted deliberately: prose PII in the tail of a large
  file is missed by the model tier, but the deterministic rules have no cap and
  still scan the whole string, so credentials and denylist entries anywhere in a
  large file are caught regardless. The model tier is for prose PII, and prose
  messages are short; large blobs are the rules' domain.

### 9.4 Assets

Neither the runtime library (~20–40 MB) nor the weights (`model_q4f16.onnx`,
~800 MB) ship in the release tarball; the binary stays ~13 MB.

On first enable, aiproxy downloads both into
`~/.config/aiproxy/models/privacy-filter/` and verifies each against a SHA-256
digest compiled into the binary. Both URLs are pinned to an immutable revision —
the Hugging Face resolve URL for a specific commit, and a specific ONNX Runtime
release asset — so "the digest did not match" always means the download broke,
never that upstream moved the file — the same download, verify, then rename path
`internal/updater` already implements, reused rather than reinvented. Nothing
unverified is ever loaded, and a partial download can never be mistaken for a
complete one. `aiproxy privacy install` performs the same fetch explicitly, for
operators who would rather not have it happen on demand.

## 10. Configuration

```json
"privacy": {
  "enabled": false,
  "onScanFailure": "closed",
  "onUnresolvedPlaceholder": "passthrough",
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

The filter is **off by default**: it rewrites request bodies, and that is not a
behaviour to acquire silently on upgrade. Enabling it turns on the deterministic
rules, because those are the reason to enable it. Every NER label is
individually opt-in and the default set is empty — `private_url` and
`private_date` in particular would be destructive in source code, where import
URLs, endpoints, doc links, changelog dates, and licence years are everywhere.

`enabled`, `onScanFailure`, `onUnresolvedPlaceholder`, `rules`, `denylist`, and
`allowlistExtra` are live-tunable. `ner.enabled`, `ner.labels`, and
`cacheEntries` are restart-gated: the session, the label set baked into cache
keys, and the LRU's capacity are all fixed at construction. Both classes are
reported through `view.Applied` as usual.

## 11. Seam and UI

`view.Status` gains `Privacy PrivacyStatus`, following the precedent set by
`Probe` and `Update` — a background-ish fact about the running instance,
rendered by the poll the TUI already makes:

```go
type PrivacyStatus struct {
    Enabled       bool             `json:"enabled"`
    ModelState    string           `json:"modelState"` // off, absent, downloading, loading, ready, error
    DownloadedPct int              `json:"downloadedPct,omitempty"`
    Redactions    map[string]int64 `json:"redactions"` // label -> count this session
    CacheHitRate  float64          `json:"cacheHitRate"`
    LastError     string           `json:"lastError,omitempty"`
    Unresolved    int64            `json:"unresolved"` // placeholders passed through
}
```

No new `view.Source` method, and therefore no new route: the model downloads
lazily on enable, and `aiproxy privacy install` is a CLI path that needs no
seam. The lockstep test stays green without a `routeFor` entry.

In the TUI: a header segment when the filter is active and has redacted
something — `⊘ 12 redacted`, falling back to `[!] 12 redacted` under `modeNone`
and shedding whole words rather than clipping, exactly as the update segment
does; a per-request
redaction count in the Activity feed, because seeing it work is what makes it
trustworthy; the new settings rows; and `Unresolved > 0` surfaced as a warning,
since that is the one condition that means the agent received something wrong.

## 12. Failure modes

| Condition | `closed` (default) | `open` |
|---|---|---|
| Filter enabled, model absent or failed to load | 503 `api_error`, nothing sent upstream | send unfiltered, warn |
| Scan error or timeout | 500 `api_error`, nothing sent upstream | send unfiltered, warn |
| Placeholder collision unresolvable at 12 hex | 500 `api_error` | 500 `api_error` — never open |

| Condition | `passthrough` (default) | `error` |
|---|---|---|
| Response contains a placeholder absent from the restore table | emit the placeholder verbatim, increment `Unresolved`, log at warn | sever the stream |

Fail-closed on the request side is the default because a privacy filter that
degrades silently is worse than no filter: the operator believes they are
protected precisely when they are not. Passthrough is the default on the
response side because the alternative is guessing at a value and writing it into
the operator's files.

## 13. Testing

The load-bearing tests, written before the code they cover:

1. **Round trip is the identity.** Property test over generated inputs:
   `restore(redact(x)) == x`, including values containing `"`, `\`, newlines,
   and multi-byte UTF-8.
2. **Chunk-boundary fuzz.** Take an SSE stream containing placeholders and split
   it at *every* byte offset; assert the restored output is identical every
   time. This is the test that makes the streaming rewriter trustworthy, and it
   is the reason property 2 can be claimed.
3. **No sentinel, no interference.** A stream with no `[[AIPROXY_` is emitted
   byte-for-byte, and the rewriter withholds nothing (asserted on the writer's
   call sequence, not just the final bytes) — property 3.
4. **`input_json_delta` round trip.** A file-write tool call whose content
   contains a secret with JSON metacharacters, split across deltas; assert the
   client receives the original content byte-exactly.
5. **Structural keys are untouched.** `model` is never redacted; base64 image
   `data` is never scanned (asserted by a detector call counter).
6. **Cache correctness.** A second identical request performs zero detector
   invocations; changing the ruleset version or label set invalidates.
7. **Collisions.** Two distinct plaintexts forced to the same 8-hex suffix are
   widened and both restore correctly.
8. **Fail-closed means closed.** With `onScanFailure: closed` and a detector
   that errors, the fake upstream receives **zero** requests.
9. **Passthrough paths are never filtered.** A request to an OAuth prefix
   reaches upstream byte-identical.
10. **Tokenizer offsets.** Reference-generated fixtures, exact match (§9.2).

Tier 2's own tests run against a fake `Detector` for the pipeline, and against
the real model only when the assets are present, skipped otherwise so `go test
./...` never depends on an 800 MB download.

## 14. Delivery order

1. JSON string walker with byte spans; the structural-key denylist.
2. Placeholders: install key, keyed hash, collision widening, restore table.
3. Tier 1 rules, entropy qualifier, allowlist, operator denylist.
4. The request-side pipeline, wired into `proxyHandler`, with fail-closed.
5. The streaming restorer in `Relay`, including `input_json_delta`.
6. The scan cache.
7. Config, `view.Status.Privacy`, Settings rows, TUI surfacing.
8. **Tokenizer gate** (§9.2) — everything after this depends on it passing.
9. Vendored `onnxrt` binding; asset download and verification.
10. The NER detector: chunking, Viterbi, label filtering.
11. `aiproxy privacy install`; README.

Steps 1–7 deliver a working filter for secrets and internal identifiers. Step 8
is a gate, not a task: if it fails, the work stops there with Tier 1 shipped.

## 15. Deferred, deliberately

- **Telling the model about placeholders.** A system-prompt note would improve
  its handling of them but changes the cache prefix and edits the client's
  prompt. Revisit if the model proves to mangle placeholders in practice.
- **Restoring in the request direction.** If the operator's own history contains
  placeholders from a failed restore, we do not resolve them on the way out.
  §5.3 explains why that state is visible rather than silent.
- **Other providers.** The detection and placeholder layers are provider
  agnostic; only Anthropic's shapes are implemented.
- **Streaming detection.** Requests are fully buffered before scanning, which is
  what `proxyHandler` already does. Scanning a streamed request body would
  require a different design and there is no client that needs it.
- **A second opinion on precision.** No mechanism yet lets an operator review
  what was redacted and mark a false positive; the Activity feed shows counts,
  not spans.
