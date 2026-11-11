# Routing rules: send work to the accounts that should pay for it

**Status:** implemented (v0.5.0)
**Date:** 2026-11-11
**Lands with:** the web dashboard ([2026-08-25-web-dashboard-design.md](2026-08-25-web-dashboard-design.md)),
in one release. The dashboard's Settings and Accounts screens are where most
operators will configure this.

## 1. Intent

An operator with more than one subscription often has a reason to care which
one a piece of work lands on, beyond "whichever ranks best". The motivating
case: one operator has a company account (GreenThread) and several personal
accounts. Work done in the company's repositories should be billed to the
company account; everything else should be billed to the personal ones.

Today aiproxy cannot express that. Selection is pin → session affinity →
priority → expiring allowance, and none of those know what the work *is*.

aiproxy is open source, so this must be a general, properly configurable
feature, not a hard-coded path check. Every operator decides for themselves
what text identifies "this kind of work" and which accounts it should prefer.

## 2. Goals, non-goals, anti-goals

**Goals**

- Route a request to a preferred **group** of accounts based on **text found in
  the request**.
- A preference, not a wall, by default: when every account in the preferred
  group is spent, work **spills** onto the other accounts, in both directions
  (company work onto personal, personal work onto company).
- An opt-in **strict** mode per rule for operators who need a wall (compliance,
  a client who must never be billed for someone else's work).
- Fully configurable, live (no restart), from the **TUI**, the **web
  dashboard**, and the control API. Hand-editing `config.json` must never be
  required.
- Visible: an operator can see, per request, which group was preferred and
  whether it spilled.

**Non-goals**

- Regex or structured matching. A rule is literal text. The operator chooses
  how precise it is (see §4.1). Regex can come later if anyone asks.
- Per-group usage/cost reporting in the metrics store. Usage grouped by
  account already answers "who paid", and an account's group is known. No
  schema change.
- Group-aware warming. The warmer keeps its current behaviour.
- Routing passthrough paths (`/v1/code/`, OAuth files, …). They carry the
  client's own credential and never select an account.

**Anti-goals** (would make this a failure even if it technically works)

- **Changing behaviour for anyone who has not configured it.** With no rules
  and no account groups, selection must be byte-for-byte what it is today.
- **A session flip-flopping between groups.** Bouncing a conversation between
  accounts throws away the upstream prompt cache on every bounce.
- **Silent spill.** If company work lands on a personal account, the operator
  must be able to see that it happened.
- **Matching on redacted text.** If the privacy filter rewrites the path a rule
  looks for, the rule must still match. Matching runs on the client's original
  body.

## 3. Configuration

```json
{
  "accounts": [
    { "id": "…", "label": "nick@greenthread", "group": "greenthread", "…": "…" },
    { "id": "…", "label": "nick@personal-1",  "group": "personal",    "…": "…" }
  ],
  "routing": {
    "rules": [
      {
        "match": "Primary working directory: /Users/nickp/code/GT",
        "group": "greenthread",
        "strict": false
      }
    ],
    "defaultGroup": "personal",
    "defaultStrict": false
  }
}
```

- **`accounts[].group`** (string, default `""`). A free-form name. Accounts
  with no group are in the group `""`.
- **`routing.rules`** (ordered list, default `[]`). Each rule has:
  - `match`: non-empty literal text.
  - `group`: the group to prefer when it matches. May be `""` (ungrouped
    accounts).
  - `strict`: when true, work matched by this rule never leaves the group.
- **`routing.defaultGroup`** (default `""`): preferred group for work that no
  rule matches.
- **`routing.defaultStrict`** (default `false`): strict mode for unmatched work.

**Backward compatibility falls out of the defaults.** With no rules, every
request prefers `defaultGroup` `""`. Every account is in group `""`, so
the preferred group is all accounts, and selection is unchanged. That is the
anti-goal in §2 met by construction, and it gets a test.

A rule naming a group with no accounts in it is legal: it prefers nothing, so
non-strict work spills immediately, and strict work is refused. The UIs flag
such a rule ("no accounts in this group") rather than refusing to save it,
because the operator may be about to add the account.

## 4. Behaviour

### 4.1 Matching

- A rule matches when its `match` text appears **anywhere in the request body**.
  It is a plain, case-sensitive substring test, with no parsing and no regex.
- **Rules are tried in order. The first match wins.** No match means
  `defaultGroup`.
- **Precision is the operator's choice, and the docs say so.**
  `Primary working directory: /Users/nickp/code/GT` matches only Claude Code
  sessions started in that tree. `code/GT` alone also matches a personal
  session that merely reads a GT file. The README shows the precise form for
  Claude Code and for Codex (`<cwd>/Users/…`).
- **The body is JSON, so match the escaped form too.** A rule's text is tested
  both as-is and in its JSON-string-escaped form (`json.Marshal` minus the
  surrounding quotes). Without the escaped form, a rule containing `"`, `\` or
  a newline would never match. Build the escaped form with HTML escaping off
  (`json.Encoder.SetEscapeHTML(false)`). Otherwise a Codex rule like
  `<cwd>/Users/…` becomes `\u003ccwd\u003e…` and never matches. Keep `/`
  unescaped: clients do not escape it.
- **Where in the pipeline:** in `proxyHandler`, after the body is read and
  before privacy redaction. Model override and the blocklist are independent of
  it; place it next to them.
- **Cost:** `bytes.Contains` over the body, once per rule (twice when the
  escaped form differs). Bodies are typically 100 KB to a few MB and rules are
  few, so this is cheap. It is skipped entirely when the session already has a
  group (§4.2) or there are no rules.

### 4.2 Sessions: decide once, then stick

The working directory is not in every request. Claude Code's small background
calls (titles, summaries) may omit it. So the group belongs to the
**session**, not to each request:

- Requests carrying `x-claude-code-session-id` look up the session's group
  first. If it has one, that group is used and no rules are evaluated.
- Otherwise the rules are evaluated. **If a rule matches, that group is recorded
  for the session and sticks for its lifetime.** If none matches, the request
  uses `defaultGroup`, but **nothing is recorded**, so a later request in the
  same session that does carry the directory can still claim it.
- A later request that would match a *different* rule does not move the
  session (*my call*). First match wins for stability, which is the
  flip-flop anti-goal.
- The session→group map lives beside the existing session-affinity map
  (`Manager.affinity`) and has the same lifetime. It is held in memory, not
  persisted, and never pruned, which is the same as affinity today (one short
  entry per session). If affinity ever gains pruning, this map follows it.
- **Changing the rules** (any `UpdateSettings` that changes `rules` or
  `defaultGroup`) clears the session→group map, so the new rules apply to
  sessions already in flight. *My call:* otherwise editing a wrong rule would
  appear to do nothing until every client restarted.
- The session map stores what the rule decided (group **and** strict), not
  just the group, so a sticky session keeps its strictness.
- Session→group memory is independent of the `sessionAffinity` setting
  (*my call*). Affinity is about keeping a warm account; this is about what
  the work is. Turning affinity off must not make background calls forget
  their project.
- Requests with no session header (Codex, curl) are matched per request.
  Codex re-sends its full input every turn, `<environment_context>` included, so
  this works for it without session memory.

### 4.3 Selection

`SelectRequest` gains `Group string` and `Strict bool`. `Manager.Select`
becomes:

1. **Pin** (`f` on the overview). Beats everything except a strict rule
   (*my call*). Strict work ignores a pin to an account outside its group and
   routes normally within the group. Strict is a wall, the pin is a
   convenience, and a wall with a convenience-shaped hole is not a wall.
   Non-strict work follows the pin as today.
2. **Session affinity.** The session's affinity account is honoured only if it
   is in the preferred group, **or** no account in the preferred group is
   currently eligible *and* the work is not strict. Strict work never follows
   affinity out of its group. This is what brings spilled work home: when GreenThread
   resets, the session moves back onto it at the next request, at the cost of
   one cold prompt cache. *My call:* the operator's intent is "GT work is
   billed to GT", which outweighs a cache miss.
3. **Candidates.** Eligible accounts (eligibility unchanged) are split into the
   preferred group and the rest. If the preferred group has any candidates,
   rank only those. Otherwise, if `Strict`, return `ErrNoAccount`, which the attempt
   loop already turns into its no-account response (`writeNoAccount`). Otherwise rank the
   rest.
4. **Ranking** within whichever set was chosen is unchanged: unpaused before
   paused, then priority, then expiring allowance, then id.

"Spent" therefore means exactly what it means today: over `switchThreshold`,
rate-limited, disabled, errored, or unable to serve the model. A GreenThread
account that cannot serve the requested model spills just as an exhausted one
does. A preferred-group account that is only *paused* (a hinted throttle) is
still a candidate, so work waits for it rather than spilling. That is the
existing "queue on the warm account" behaviour, kept on purpose.

**When strict refuses**, the client must be told why, and must not be sent into
an endless retry loop. `writeNoAccount` today reasons over every account. For
strict work it reasons over the group's accounts only:

- If the group has accounts that will become eligible (spent quota,
  rate-limited), return the existing 429, with `Retry-After` taken from the
  soonest reset **within the group**.
- If the group has no accounts, or every account in it is disabled or errored,
  return a non-retryable error (the existing 502 path's status) whose message
  names the group and says a strict routing rule refused the request.

`Select` therefore needs to report *why* it failed for strict work, not only
`ErrNoAccount`. A dedicated error value, or a small result struct, is the
builder's choice. These refusals are recorded with the existing no-account
outcome. Their Activity rows show the group like any other.

The attempt loop's `Exclude` set works as today. If a preferred-group attempt
fails and is excluded, the next `Select` either finds another account in the
group or spills (or, if strict, stops).

### 4.4 Visibility

- `proxy.Result` gains `Group` (the preferred group) and `Spilled` (served by
  an account outside it). `view.Event` carries both.
- **Activity (TUI and web):** a spilled request is marked on its account cell,
  for example `nick@personal-1 ↪`. A filter by group is welcome but optional.
- The request log line gains `group` and `spilled`.
- **Accounts (TUI and web):** a group column.

## 5. Configuring it

Everything is live: rules, defaults and account groups take effect on the next
request.

**Control API**

- `view.Settings` gains `RoutingRules []RoutingRule`, `DefaultGroup string`, and
  `DefaultStrict bool`, all in `liveSettingsFields`. They go through the
  existing `GET`/`POST /api/v1/settings`.
- **An omitted field keeps its current value** (*my call*). A `POST` body
  that leaves out `routingRules`, `defaultGroup` or `defaultStrict` (an older
  script, or a client built before these fields existed) must not wipe them.
  Silently losing a strict rule would silently remove a wall. On the wire
  these three are optional (pointer or nil-slice semantics). Explicit `[]`
  clears the rules; absence does not. `diffSettings` treats absent as
  unchanged.
- **Validation:** every rule has non-empty `match`. Group names are trimmed, and
  so is `match`'s outer whitespace. `match` must not be *only* whitespace.
- A new `view.Source` method `SetAccountGroup(ctx, accountID, group)` with a
  route `POST /api/v1/accounts/{id}/group`, mirroring `SetPriority`.
  `TestEveryViewSourceMethodHasAControlRoute` will insist on the route.
- `view.Account` gains `Group`.

**TUI**

- **Accounts screen:** a group column, and a key to edit the selected
  account's group with the same text-input modal used for renaming or
  priority.
- **Settings screen:** `defaultGroup` and `defaultStrict` as ordinary rows. A
  `routingRules` row shows a summary (for example `1 rule`). Enter opens a
  rules editor modal that lists the rules in order and supports add, edit,
  delete, and move up/down. Each rule is edited as three fields: match, group,
  and strict. The modal names the groups that currently exist and flags a
  rule whose group has no accounts.

**Web dashboard**

- **Settings screen:** a "Routing" card with the same rules editor and the two
  default fields.
- **Accounts screen:** group shown and editable per account.
- The dashboard spec's §2 claim of "no new query endpoints" stays true. This
  adds one mutation route, listed above.

## 6. Delivery

The dashboard branch (`feat/web-dashboard`) is parked partway through Task 6 of
its plan, with a WIP checkpoint commit at its tip, and is two commits behind
`main`. This feature lands with it, in one release:

1. Resume the WIP checkpoint on `feat/web-dashboard` (`git reset --soft
   HEAD~1`, as its commit message says), commit it properly, then merge
   `main` in. The dashboard plan's Settings task gains `modelOverride`
   (shipped in v0.4.0) and the routing fields. Its "no new `view.Source`
   methods" constraint is relaxed for `SetAccountGroup`.
2. Build routing rules' backend (config, Manager, handler, view, control
   API) and TUI editors on that branch. They do not depend on the dashboard,
   so they can come first and be tested on their own.
3. Finish the dashboard plan. Its Settings (Task 14) and Accounts (Task 12)
   screens include the routing UI from §5.
4. One release (v0.5.0) carrying both.

Before the README example is written, **confirm the exact environment-line
text** Claude Code and Codex send, against a real captured request. The
feature does not depend on it, since rules are operator text, but the
documented example must be right.

## 7. Testing

- **No configuration means no change:** with no rules and no groups, `Select`
  returns exactly what it returns today across the existing selection test
  table.
- **Matching:** first-match order, the escaped-form match (a rule containing
  `"`), no match falls to default, and matching sees the pre-redaction body.
- **Sessions:** a match sticks; an unmatched request does not record; a later
  different match does not move the session; changing the rules clears the map.
- **Selection:** prefers the group; spills when the group is spent; strict
  refuses; affinity to a spilled account yields once the group recovers;
  pin wins for non-strict work and is ignored by strict work; strict work
  never follows affinity out of its group.
- **Strict refusals:** a spent group gets a 429 with a `Retry-After` from
  the group's own resets. An empty or dead group gets the non-retryable
  error naming the group.
- **Omitted fields:** a settings `POST` without the routing fields leaves them
  untouched; explicit `[]` clears the rules.
- **Settings:** the round-trip, validation, and that the new fields are reported
  under `Live`.
- **End to end through the router:** a body containing the rule text is served
  by the group's account; one without it is served by the default group's.
- **TUI:** golden frames for the new rows and the rules modal.

## 8. Calls made without the operator

Marked *my call* above, collected here:

- First match sticks for a session; a later different match does not move it.
- Changing the rules clears session→group memory.
- Affinity yields to the preferred group once it recovers (one cache miss).
- Per-rule `strict` and `defaultStrict` exist. The operator asked for spill;
  these are for other operators of an open-source tool.
- A rule pointing at an empty group is saved and flagged, not refused.
- Strict beats the pin and affinity; non-strict work follows both as today.
- Strict refusals are group-aware: 429 with the group's own `Retry-After`
  when it will recover, a non-retryable error naming the group when it won't.
- Omitting the routing fields from a settings `POST` keeps them.
- Session→group memory works whether or not `sessionAffinity` is on.
- Literal substring only; no regex.

## 9. Left to the builder

Exact key bindings and modal layout in the TUI; the visual treatment of the
spill marker; field names in Go (the JSON names above are fixed); where in
`proxyHandler` the match sits relative to the override and the blocklist; the
web card's layout, within the dashboard spec's conventions.
