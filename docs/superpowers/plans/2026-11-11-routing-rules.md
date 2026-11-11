# Routing Rules (with the Web Dashboard) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prefer a group of accounts based on text in the request, configurable live from the TUI, the web dashboard and the control API, and ship it in one release with the finished web dashboard.

**Architecture:** The Manager owns the routing state: compiled rules, the default, and session→route memory. The proxy handler classifies each request into an `account.Route` before redaction. `Manager.Select` partitions candidates by the route's group, spills or refuses, and otherwise ranks exactly as today. Configuration flows through `view.Settings` and one new `view.Source` method, so the TUI and the dashboard edit the same thing.

**Tech Stack:** Go 1.26, stdlib plus the existing bubbletea TUI. The dashboard is vanilla ES modules, as its own plan specifies.

**Spec:** `docs/superpowers/specs/2026-11-11-routing-rules-design.md`. The dashboard work follows `docs/superpowers/plans/2026-08-25-web-dashboard.md` (on `feat/web-dashboard`) and its spec, `docs/superpowers/specs/2026-08-25-web-dashboard-design.md`.

## Global Constraints

- **No configuration, no change.** With no rules and no account groups, `Select` returns exactly what it returns today. The existing `internal/account/select_test.go` table must pass unmodified.
- JSON names are fixed by the spec: `accounts[].group`, `routing.rules[].{match,group,strict}`, `routing.defaultGroup`, `routing.defaultStrict`. On the settings wire they are `routingRules`, `defaultGroup` and `defaultStrict`. The account mutation route is `POST /_aiproxy/api/v1/accounts/{id}/group` with body `{"group": "..."}`.
- **Matching is literal, case-sensitive substring over the request body as received.** That means before privacy redaction, tested both as-is and JSON-string-escaped with HTML escaping off.
- **Rules are matched outside the Manager's mutex.** A multi-megabyte `bytes.Contains` under the lock would serialise every request.
- **Routing fields are live.** They go in `liveSettingsFields` and never `restartSettingsFields`.
- Every `view.Source` method keeps exactly one control route (`TestEveryViewSourceMethodHasAControlRoute`). This plan adds `SetAccountGroup` and its route, which relaxes the dashboard plan's "no new `view.Source` methods" constraint.
- All of the dashboard plan's Global Constraints apply to the dashboard tasks unchanged: no build step, no third-party JS, SVG through the DOM, and the API key never in JS beyond sign-in.
- `gofmt`, `go vet ./...`, `go test ./...` clean at every commit.
- All work happens on `feat/web-dashboard`. Nothing merges to `main` until Task 10.

## Review Focus

1. **A rule with non-ASCII text** (`/Users/nick/Código`) must match a body that carries it as raw UTF-8, which is how Claude Code (JS) and Codex (Rust) send it. Covered in Task 1.
2. **An account's group is renamed or emptied mid-session.** The session's remembered route still names the old group. Non-strict work must spill rather than fail, and strict work must get the "no usable account in group" error rather than an endless 429. Covered in Task 3.
3. **`"routingRules": null`** in a settings POST is "absent", so it keeps the rules. It does not clear them. Covered in Task 4.
4. **A rule pasted with surrounding whitespace or a trailing newline** (common from a terminal or a browser) is trimmed on save, so it does not silently never match. Covered in Task 4.
5. **Reordering rules with sessions in flight** applies the new order to those sessions at their next request, because the session map clears. Covered in Task 1.

---

### Task 0: Resume the dashboard branch

**Files:** none new.

- [ ] **Step 1: Restore the WIP checkpoint and commit it properly**

```bash
git checkout feat/web-dashboard
git log -1 --format=%s   # expect: wip: dashboard sign-in screen and client-side router
git reset --soft HEAD~1
go build ./... && go test ./internal/web/...
```

If the WIP files compile and their tests pass, commit them as `feat(web): sign-in screen and client-side router (partial Task 6)`. If they do not, commit nothing yet: Task 7 finishes dashboard Task 6 from this state.

- [ ] **Step 2: Merge main**

```bash
git merge main -m "merge: main into the dashboard branch (model override, routing design)"
go test ./...
```

Expected: PASS. The conflicts likely to come up are in `cmd/aiproxy/main.go` (HandlerOptions) and `internal/proxy/handler.go` (`Authorized` vs `authorizedRequest`). Keep both sides' changes: the model override block and the session-auth call.

---

### Task 1: Config schema, account groups, and classification

**Files:**
- Modify: `internal/config/config.go`: `RoutingRule`, `Routing` fields, `Account.Group`, defaults
- Modify: `internal/account/account.go`: `Account.Group`, set in `fromConfig`
- Create: `internal/account/route.go`: `Route`, compiled rules, `SetRouting`, `Classify`
- Modify: `internal/account/manager.go`: Manager fields `rules`, `defaultRoute`, `sessionRoutes`
- Modify: `internal/account/mutate.go`: `SetGroup`
- Test: `internal/account/route_test.go`

**Interfaces:**
- Produces:
  - `config.RoutingRule{Match string "json:match"; Group string "json:group"; Strict bool "json:strict,omitempty"}`
  - `config.Routing` gains `Rules []RoutingRule "json:rules"`, `DefaultGroup string "json:defaultGroup"`, `DefaultStrict bool "json:defaultStrict"`. `Default()` sets `Rules: []RoutingRule{}`.
  - `config.Account.Group string "json:group,omitempty"`, and `account.Account.Group string`.
  - `type account.Route struct { Group string; Strict bool; Rule int }`. `Rule` is the matched rule's index, or `-1` for the default.
  - `func (m *Manager) SetRouting(rules []config.RoutingRule, defaultGroup string, defaultStrict bool)` compiles the rules and **clears `sessionRoutes`**.
  - `func (m *Manager) Classify(sessionID string, body []byte) Route`
  - `func (m *Manager) SetGroup(id, group string) error` (unknown id is an error, mirroring `SetPriority`)

- [ ] **Step 1: Write the failing tests** in `route_test.go`, using the existing `mgr`/`acct` helpers from `select_test.go`:

```go
func TestClassifyFirstMatchingRuleWins(t *testing.T)
// rules: [{Match:"code/GT", Group:"gt"}, {Match:"code", Group:"other"}], default "personal"
// Classify("", []byte(`{"system":"Primary working directory: /Users/n/code/GT/x"}`)) == Route{Group:"gt", Rule:0}
// Classify("", []byte(`{"system":"/Users/n/code/aiproxy"}`))                    == Route{Group:"other", Rule:1}
// Classify("", []byte(`{"system":"nothing"}`))                                  == Route{Group:"personal", Rule:-1}

func TestClassifyMatchesTheJSONEscapedForm(t *testing.T)
// rule Match: "say \"hi\"\n<cwd>/a</cwd>" ; body is json.Marshal of a map containing that string
// (with SetEscapeHTML(false), as clients send it) -> matches.

func TestClassifyMatchesNonASCIIText(t *testing.T)
// rule Match "/Users/nick/Código" ; body `{"system":"cwd /Users/nick/Código/app"}` as raw UTF-8 -> matches.

func TestClassifyRemembersAMatchForTheSession(t *testing.T)
// Classify("s1", gtBody) -> gt ; Classify("s1", []byte(`{"system":"haiku title call"}`)) -> gt (Rule 0)

func TestClassifyDoesNotRememberTheDefault(t *testing.T)
// Classify("s1", plainBody) -> default ; Classify("s1", gtBody) -> gt

func TestClassifyDoesNotMoveAMatchedSession(t *testing.T)
// rules gt (Match "GT") and other (Match "OTHER"); Classify("s1", "...GT...") -> gt;
// Classify("s1", "...OTHER...") -> still gt

func TestSetRoutingClearsSessionMemory(t *testing.T)
// Classify("s1", gtBody) -> gt ; SetRouting with the two rules REORDERED so "code" (other) is first;
// Classify("s1", haikuBody) -> default (forgotten) ; Classify("s1", gtBody) -> other

func TestClassifyRemembersStrictness(t *testing.T)
// rule {Match:"GT", Group:"gt", Strict:true}; Classify("s1", gtBody); Classify("s1", plain).Strict == true

func TestClassifyRemembersWithAffinityOff(t *testing.T)
// New(..., Options{SessionAffinity:false}); SetRouting gt rule; Classify("s1", gtBody); Classify("s1", plain).Group == "gt"

func TestClassifyWithNoRulesIsTheDefault(t *testing.T)
// fresh mgr (never SetRouting) -> Classify("s", anything) == Route{Group:"", Strict:false, Rule:-1}

func TestSetGroup(t *testing.T)
// SetGroup("a","gt") then Get("a").Group == "gt" ; SetGroup("nope","x") returns an error
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/account -run 'Classify|SetRouting|SetGroup'`
Expected: build failure (undefined `Route`, `SetRouting`, `Classify`, `SetGroup`).

- [ ] **Step 3: Implement**

- **Compiling a rule** stores its `Match` bytes, plus the escaped form when it differs. The escaped form comes from `json.Encoder` with `SetEscapeHTML(false)`, with the surrounding quotes and the trailing newline stripped.
- **`Classify` steps:**
  1. Under the lock, return the stored route if `sessionID != ""` and one exists. Otherwise copy the compiled rules slice header and the default route.
  2. Unlock and scan the rules in order with `bytes.Contains`.
  3. On a match with `sessionID != ""`, re-lock and store the route, but only if no route was stored in the meantime. If a concurrent request already stored one, return that.
- **`fromConfig`** copies `c.Group`.
- **Manager state:** `New` initialises `sessionRoutes` to an empty map and the default route to `Route{Rule: -1}`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/account ./internal/config`
Expected: PASS, including every pre-existing test.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/account
git commit -m "feat(account): routing rules — classify a request into a preferred group"
```

---

### Task 2: Group-aware selection

**Files:**
- Modify: `internal/account/select.go`: `SelectRequest`, `Select`
- Test: `internal/account/route_select_test.go`

**Interfaces:**
- Consumes: `Route` and `Account.Group` (Task 1).
- Produces: `SelectRequest` gains `Group string` and `Strict bool`. The zero values mean "no preference", which is today's behaviour.

- [ ] **Step 1: Write the failing tests.** Accounts use `acct(id, prio)` with `Group` set.

```go
func TestSelectPrefersTheRequestedGroupOverPriority(t *testing.T)
// a(p0, group "personal"), g(p5, group "gt"); Select{Group:"gt"} -> g ; Select{Group:"personal"} -> a

func TestSelectSpillsWhenTheGroupIsSpent(t *testing.T)
// MarkRateLimited("g", time.Hour); Select{Group:"gt"} -> a

func TestSelectStrictRefusesToSpill(t *testing.T)
// MarkRateLimited("g", time.Hour); Select{Group:"gt", Strict:true} -> errors.Is(err, ErrNoAccount)

func TestSelectSpillsWhenTheGroupHasNoAccounts(t *testing.T)
// Select{Group:"ghost"} -> best of all (a)

func TestSelectAffinityYieldsToTheRecoveredGroup(t *testing.T)
// RecordSession("s","a"); Select{SessionID:"s", Group:"gt"} -> g (a is outside gt and g is eligible)

func TestSelectAffinityHoldsWhileTheGroupIsSpent(t *testing.T)
// RecordSession("s","a"); MarkRateLimited("g"); Select{SessionID:"s", Group:"gt"} -> a

func TestSelectStrictIgnoresAffinityOutsideTheGroup(t *testing.T)
// two gt accounts g1(p0), g2(p1); RecordSession("s","a"); Select{SessionID:"s", Group:"gt", Strict:true} -> g1

func TestSelectPinBeatsANonStrictGroup(t *testing.T)
// Pin("a"); Select{Group:"gt"} -> a

func TestSelectStrictIgnoresAPinOutsideTheGroup(t *testing.T)
// Pin("a"); Select{Group:"gt", Strict:true} -> g ; and the pin is NOT cleared (Select{} afterwards -> a)

func TestSelectPausedGroupAccountStillBeatsSpilling(t *testing.T)
// g paused (PausedUntil in the future, still eligible); Select{Group:"gt"} -> g
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/account -run 'Select.*(Group|Spill|Strict|Pin|Affinity|Paused)'`
Expected: FAIL (unknown fields `Group`/`Strict` on `SelectRequest`).

- [ ] **Step 3: Implement in `Select`**, following spec §4.3:

- **Pin:** honour `pinnedChoiceLocked` unless `req.Strict` and the pinned account's `Group != req.Group`. In that case skip it without clearing the pin. Read the pinned account without calling `pinnedChoiceLocked`'s clearing path, or add a guard before it.
- **Affinity:** honour the affinity account only if it is in `req.Group`, or (`!req.Strict` and no eligible account in `req.Group` exists).
- **Candidates:** build the eligible candidates as today, then keep those with `Group == req.Group`.
  - If any are kept, rank only those.
  - Otherwise, if `req.Strict`, return `ErrNoAccount`.
  - Otherwise rank all eligible candidates.
- **Ranking:** the existing sort, untouched.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/account`
Expected: PASS, with `select_test.go` unmodified.

- [ ] **Step 5: Commit**

```bash
git add internal/account
git commit -m "feat(account): prefer the routed group, spill or refuse when it is spent"
```

---

### Task 3: Proxy wiring, strict refusals, and visibility

**Files:**
- Modify: `internal/proxy/attempt.go`: `Request.Route`, `Result.Group`/`Result.Spilled`, pass the route to `Select`, `writeNoAccount`, `retryAfterHint`
- Modify: `internal/proxy/handler.go`: classify in `proxyHandler`
- Modify: `internal/view/types.go`: `Event.Group`, `Event.Spilled`
- Modify: `cmd/aiproxy/main.go`: `mgr.SetRouting(...)` after `account.New`, the log line, and `view.Event`
- Test: `internal/proxy/route_test.go`

**Interfaces:**
- Consumes: `Manager.Classify`, `Route`, `SelectRequest.Group/Strict` (Tasks 1–2).
- Produces:
  - `proxy.Request.Route account.Route`
  - `proxy.Result.Group string`, `proxy.Result.Spilled bool`
  - `view.Event.Group string "json:group,omitempty"`, `view.Event.Spilled bool "json:spilled,omitempty"`

- [ ] **Step 1: Write the failing tests** using `newRouterHarness`. Extend it so a test can add a second account with a `Group`, or build a two-account variant beside it, with each account pointing at its own `FakeUpstream` so the serving account is observable.

```go
func TestRouterRoutesMatchingWorkToItsGroup(t *testing.T)
// accounts: personal (p0, group "personal"), gt (p5, group "gt");
// SetRouting([{Match:"code/GT", Group:"gt"}], "personal", false)
// POST body containing "Primary working directory: /Users/n/code/GT" -> served by gt; lastResult.Group=="gt", Spilled==false
// POST body without it -> served by personal

func TestRouterMarksSpilledWork(t *testing.T)
// gt account rate-limited (h.mgr.MarkRateLimited) -> GT body served by personal, Result.Spilled == true

func TestRouterMatchesBeforeRedaction(t *testing.T)
// privacy filter on with a denylist entry "code/GT" (so redaction rewrites it);
// rule Match "code/GT" -> still served by gt

func TestRouterStrictRefusalUsesTheGroupsOwnReset(t *testing.T)
// strict rule; gt has a 5h bucket resetting in 90s and is over threshold; personal resets in 10s
// -> 429, Retry-After is "90" (not "10"), Outcome NoAccountReady

func TestRouterStrictRefusalForAnEmptyGroupIsNotRetryable(t *testing.T)
// strict rule pointing at group "ghost" -> 502, body mentions "ghost" and "strict", upstream not called

func TestRouterStrictWorkWhoseGroupWasEmptiedMidSession(t *testing.T)
// session "s" routed to gt (strict) ; then h.mgr.SetGroup("gt-acct", "renamed")
// -> next request in "s" gets the 502 naming "gt", not a 429

func TestRouterNonStrictWorkWhoseGroupWasEmptiedMidSessionSpills(t *testing.T)
// same, non-strict -> served by personal, Spilled == true
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/proxy -run 'Router.*(Group|Spill|Redaction|Strict)'`
Expected: build failure on `Route`/`Group`/`Spilled`.

- [ ] **Step 3: Implement**

- **`proxyHandler`:** after the model-override block and before the blocklist, set `req.Route = o.Manager.Classify(req.SessionID, req.Body)` when `o.Manager != nil`. It must come before the privacy block.
- **`Attempter.Do`:** pass `Group: req.Route.Group, Strict: req.Route.Strict` into `SelectRequest`. Where `res.AccountID` is stamped, also set `res.Group = req.Route.Group` and `res.Spilled = acct.Group != req.Route.Group`.
- **Initialise `res.Group`** to `req.Route.Group` before the loop, so refusals carry it.
- **`writeNoAccount(w, res, refused, route account.Route)`:** when `route.Strict`, count only accounts with `Group == route.Group`. If none of them is enabled and non-errored, answer 502 with `res.Outcome = provider.OutcomeNoAccountReady` and this message:
  `No usable account in group "<g>": a strict routing rule keeps this work there. Add or re-enable an account in that group, or turn strict off.`
  Otherwise apply the existing refused-vs-exhausted logic over the group's accounts only.
- **`retryAfterHint(inScope func(account.Account) bool)`** filters the accounts it considers. Non-strict callers pass a function that always returns true.
- **`cmd/aiproxy/main.go`:**
  - Call `mgr.SetRouting(cfg.Routing.Rules, cfg.Routing.DefaultGroup, cfg.Routing.DefaultStrict)` after `account.New`.
  - Add `"group", res.Group, "spilled", res.Spilled` to the request log line.
  - Set `Group` and `Spilled` on the published `view.Event`.
  - Mirror the event change in `newRouterHarness`'s `OnResult`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/proxy ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy internal/view/types.go cmd/aiproxy
git commit -m "feat(proxy): route requests by rule, and say when work spilled or was refused"
```

---

### Task 4: Settings, account group, and the control API

**Files:**
- Modify: `internal/view/types.go`: `RoutingRule`, the three `Settings` fields, `Account.Group`
- Modify: `internal/view/settings.go`: validation
- Modify: `internal/view/local.go`: `settingsFromConfig`, `UpdateSettings`, `diffSettings`, `liveSettingsFields`, `Accounts`, `SetAccountGroup`
- Modify: `internal/view/source.go`: the `SetAccountGroup` method
- Modify: `internal/proxy/control.go` and `internal/proxy/router.go`: `setAccountGroupHandler` and its route
- Test: `internal/view/local_test.go`, `internal/proxy/control_api_test.go`

**Interfaces:**
- Consumes: `Manager.SetRouting`, `Manager.SetGroup` (Task 1).
- Produces:
  - `view.RoutingRule{Match string "json:match"; Group string "json:group"; Strict bool "json:strict"}`
  - `view.Settings` gains `RoutingRules []RoutingRule "json:routingRules"`, `DefaultGroup *string "json:defaultGroup,omitempty"`, `DefaultStrict *bool "json:defaultStrict,omitempty"`. Nil means absent, which keeps the current value. `Settings()` always returns them non-nil, with `RoutingRules` as `[]RoutingRule{}` when empty.
  - `view.Account.Group string "json:group"`
  - `Source.SetAccountGroup(ctx context.Context, accountID, group string) error`
  - `POST /_aiproxy/api/v1/accounts/{id}/group` with `{"group": string}`, answering `{"ok":true}` the way the priority route does.

- [ ] **Step 1: Write the failing tests**

```go
// internal/view/local_test.go
func TestUpdateSettingsRoutingIsLiveAndAppliesToTheManager(t *testing.T)
// set RoutingRules [{Match:"GT",Group:"gt"}], DefaultGroup "personal"
// -> Applied.Live contains "routingRules" and "defaultGroup"; cfg persisted;
//    h.mgr.Classify("", []byte("GT")).Group == "gt"

func TestUpdateSettingsOmittedRoutingFieldsAreKept(t *testing.T)
// seed rules + defaultGroup + defaultStrict=true; UpdateSettings with RoutingRules nil, DefaultGroup nil, DefaultStrict nil
// -> config unchanged, Applied mentions none of the three

func TestUpdateSettingsNullRoutingRulesIsAbsent(t *testing.T)
// decode `{"routingRules":null, ...other required fields...}` into Settings, UpdateSettings -> rules kept

func TestUpdateSettingsExplicitEmptyRoutingRulesClears(t *testing.T)
// RoutingRules: []RoutingRule{} -> config has zero rules

func TestUpdateSettingsTrimsPastedRules(t *testing.T)
// Match "  code/GT\n", Group " gt " -> persisted "code/GT", "gt"

func TestUpdateSettingsRejectsABlankMatch(t *testing.T)
// Match "   " -> error, nothing persisted

func TestUpdateSettingsUnchangedRoutingDoesNotClearSessions(t *testing.T)
// Classify("s", gtBody) records gt; UpdateSettings changing only switchThreshold
// -> Classify("s", plainBody) still gt

func TestSetAccountGroupPersistsAndApplies(t *testing.T)
// SetAccountGroup("a", " gt ") -> cfg account a has Group "gt"; Accounts() shows Group "gt"
// unknown id -> errors.Is(err, ErrUnknownAccount)

// internal/proxy/control_api_test.go
func TestControlSetAccountGroup(t *testing.T)
// POST /_aiproxy/api/v1/accounts/acct-0/group {"group":"gt"} -> 200 {"ok":true}; unknown id -> 404
```

The existing `TestEveryViewSourceMethodHasAControlRoute` covers the route's existence.

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/view ./internal/proxy -run 'Routing|SetAccountGroup|ControlSetAccountGroup|EveryViewSource'`
Expected: build failure.

- [ ] **Step 3: Implement**

- **`UpdateSettings`:**
  1. Trim each rule's `Match` and `Group`, and trim `*DefaultGroup` when present. This runs before `Validate`.
  2. Inside the config-update closure, fill any absent field (`RoutingRules == nil`, or a nil pointer) from `before`. Then `diffSettings` sees it as unchanged.
  3. Persist all three to `c.Routing`.
  4. After persisting, call `l.mgr.SetRouting` **only if** one of the three changed, so an unrelated save does not wipe session memory.
- **`diffSettings`:** compare `routingRules` element-wise, and `defaultGroup` and `defaultStrict` by value.
- **`liveSettingsFields`:** append `"routingRules"`, `"defaultGroup"` and `"defaultStrict"`.
- **`Validate`:** reject a rule whose trimmed `Match` is empty, with the message `routingRules[<i>].match must not be blank`.
- **`SetAccountGroup`:** mirrors `SetPriority` (lock, check existence, `setAccountField`, then `mgr.SetGroup`) and trims the group.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/view ./internal/proxy ./internal/tui`
Expected: PASS. If a TUI test builds `view.Settings` literals and breaks, nil pointers are valid ("absent"), so fix the literal, not the semantics.

- [ ] **Step 5: Commit**

```bash
git add internal/view internal/proxy
git commit -m "feat(view): routing rules and account groups through the settings seam and control API"
```

---

### Task 5: TUI — group column, group editing, default rows, spill marker

**Files:**
- Modify: `internal/tui/accounts.go`: group column, `g` key, text-input modal state
- Modify: `internal/tui/settings.go`: `defaultGroup`, `defaultStrict` rows, and a `routingRules` summary row (Task 6 opens its editor)
- Modify: `internal/tui/activity.go`: spill marker on the account cell
- Modify: `internal/tui/app.go`: help-screen entry for `g` on Accounts
- Test: `internal/tui/behavior_test.go`, regenerated goldens under `internal/tui/testdata/`

**Interfaces:**
- Consumes: `view.Account.Group`, `Source.SetAccountGroup`, the `view.Settings` routing fields (Task 4), `view.Event.Spilled` (Task 3).
- Produces: `settingField` gains `opens func(m *Model) tea.Cmd`. When non-nil, enter calls it instead of opening the inline text input. Task 6 sets it on the `routingRules` row.

- [ ] **Step 1: Write the failing tests**

```go
func TestAccountsGKeyEditsTheGroup(t *testing.T)
// fake Source; select account a; press "g", type "gt", press enter
// -> fake recorded SetAccountGroup("a","gt")

func TestActivityMarksSpilledRequests(t *testing.T)
// view.Event{Account:"a", Spilled:true} -> rendered account cell ends with " ↪"
// a long label is truncated before the marker so the marker always survives

func TestSettingsDefaultRows(t *testing.T)
// defaultGroup row get/set round-trips through a *string; defaultStrict toggles a *bool;
// routingRules row reads "none" for [] and "2 rules" for two
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/tui -run 'GKey|Spilled|DefaultRows'`
Expected: FAIL.

- [ ] **Step 3: Implement**

- **Accounts:** add a `group` column, showing `—` for `""`. `g` opens the same `textinput` modal pattern the settings screen uses, prefilled with the current group. Enter calls `m.action("grouped "+label, "set group", ...)` and esc cancels. Add `g group` to the footer.
- **Settings:** add the rows `defaultGroup` (text; description "group preferred by work no routing rule matches; empty means ungrouped accounts"), `defaultStrict` (boolean; description "unmatched work never spills outside the default group") and `routingRules` (summary; description "text in a request → preferred account group; enter to edit"). Put them directly after `modelOverride`.
- **Activity:** the account cell is `truncate(label, width-2) + " ↪"` when `e.Spilled`.

- [ ] **Step 4: Regenerate and review the goldens**

```bash
go test ./internal/tui -run TestGoldenFrames -update
git diff --stat internal/tui/testdata
go test ./internal/tui
```

Expected: only the settings and accounts goldens change, and the diff shows only the new rows and column. PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): account groups, routing defaults, and a spill marker in Activity"
```

---

### Task 6: TUI — the routing rules editor

**Files:**
- Create: `internal/tui/rules.go`: `rulesEditor` state, keys, and view
- Modify: `internal/tui/settings.go`: the `routingRules` row's `opens` hook, and key routing while the editor is open
- Test: `internal/tui/rules_test.go`, plus a new golden `internal/tui/testdata/rules_80.golden`

**Interfaces:**
- Consumes: `settingField.opens` (Task 5), `pushSettings`, `view.RoutingRule`, `m.accounts` (for the group hints).
- Produces: none used elsewhere.

- [ ] **Step 1: Write the failing tests**

Each mutation pushes the whole `Settings` immediately (live). Assert on the `RoutingRules` the fake `Source` receives.

```go
func TestRulesEditorAddsARule(t *testing.T)
// open editor; "a"; type match "Primary working directory: /Users/n/code/GT"; enter; type group "gt"; enter
// -> pushed RoutingRules == [{Match:..., Group:"gt", Strict:false}]

func TestRulesEditorEditsDeletesTogglesAndReorders(t *testing.T)
// seed two rules; "e" changes the match of rule 0; "s" toggles strict on rule 0;
// "J" moves rule 0 down; "d" then "y" deletes the selected rule -> each push reflects the step

func TestRulesEditorDeletingTheLastRuleSendsAnEmptyList(t *testing.T)
// one rule; "d","y" -> pushed RoutingRules is non-nil and has length 0 (explicit clear, not "absent")

func TestRulesEditorFlagsAGroupWithNoAccounts(t *testing.T)
// accounts in groups "gt","personal"; a rule with group "ghost" renders "no accounts in this group"

func TestRulesEditorEscCloses(t *testing.T)
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/tui -run RulesEditor`
Expected: build failure (no `rulesEditor`).

- [ ] **Step 3: Implement `rules.go`**

- **Keys:** `j`/`k` select, `a` add, `e`/enter edit, `s` toggle strict, `J`/`K` move down/up, `d` delete (confirmed by `y`, any other key cancels), esc closes.
- **Add and edit** are two prompts in sequence: match, then group, each prefilled when editing.
- **The view** lists the rules in order, as `n. "<match>" → <group> [strict]`, truncated to the width. Below the list it shows a line naming the groups that currently have accounts. A rule whose group has no accounts gets a warning-coloured `no accounts in this group`.
- **The footer** shows the keys.

- [ ] **Step 4: Add the golden and run**

Add a `rules_80` frame to the golden table in `frames_test.go` with two rules, one of them pointing at a group with no accounts. Then:

```bash
go test ./internal/tui -run TestGoldenFrames -update && go test ./internal/tui
```

Expected: PASS. Review the new golden by eye.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): edit routing rules from the settings screen"
```

---

### Task 7: Finish the dashboard, Tasks 6–13

Execute `docs/superpowers/plans/2026-08-25-web-dashboard.md` Tasks 6–13 as written, **each as its own task with its own review**. Make these amendments as each task is reached:

- **Dashboard Task 6:** resume from the state Task 0 left. Do not redo committed work.
- **Dashboard Task 10 (Activity):** render `event.spilled` as a ` ↪` suffix on the account cell, with a `title` attribute of `served outside group "<event.group>"`. Add a test-visible hook equivalent to the plan's existing assertions for that task.
- **Dashboard Task 12 (Accounts):** show `group` on each account card (`—` when empty), and an inline editor that calls `POST /accounts/{id}/group` with `{"group": "..."}`. Re-fetch after success, as the other mutations do. Show errors beside the control (Task 12 Step 4's rule).

Commit messages are as each dashboard task specifies.

---

### Task 8: Dashboard Settings, with model override and routing

Execute dashboard plan **Task 14** with these additions to Step 1's field list:

- **`modelOverride`:** text. Help: "send every request in this model's family as this model; empty sends what was asked". Live.
- **A "Routing" card**, containing:
  - `defaultGroup` (text, with a `<datalist>` of the groups currently on accounts, read from `GET /accounts`)
  - `defaultStrict` (checkbox)
  - a rules list editor: each row has `match` (a textarea, since rules can be long), `group` (text with the same datalist), `strict` (checkbox), move up/down buttons and remove, plus "Add rule". A rule whose group matches no account shows "no accounts in this group".
  - help text explaining that a rule is literal text found anywhere in the request, with the two documented examples from Task 10 shown verbatim.
- **Saving** sends `routingRules` as the full current list. When it is empty, send `[]`, never omit it: omission means "keep". Send `defaultGroup` and `defaultStrict` always.
- **The `Applied` rendering** (Task 14 Step 2) needs no change: the routing fields come back under `live`.

Commit: `feat(web): settings screen, with model override and routing rules`.

---

### Task 9: Dashboard end-to-end (dashboard Task 15)

Execute dashboard plan **Task 15** as written, and extend its end-to-end test with this case: sign in, POST settings with one rule and a `defaultGroup`, POST a group onto an account, then GET `/settings` and `/accounts` and assert both came back.

---

### Task 10: Docs and release

**Files:**
- Modify: `README.md`: a "Routing rules" section, and the `routing` and `accounts` keys in the config example
- Modify: `docs/superpowers/specs/2026-11-11-routing-rules-design.md`: status set to implemented

- [ ] **Step 1: Confirm Claude Code's exact environment line against a real request**

```bash
( nc -l 127.0.0.1 9876 > /tmp/cc-request.txt & ) ; sleep 1
ANTHROPIC_BASE_URL=http://127.0.0.1:9876 ANTHROPIC_API_KEY=x timeout 15 claude -p hi >/dev/null 2>&1
grep -o -E '(Primary working directory|Working directory): [^\\"]+' /tmp/cc-request.txt | head -3
```

Expected: one line naming the directory the command ran in. Use that exact prefix in the README. Codex's form is already confirmed from captured fixtures: `<cwd>/path</cwd>`.

- [ ] **Step 2: Write the README section**

The section covers:
- what groups and rules are
- first match wins
- precision is your choice, with the confirmed Claude Code example and `<cwd>/Users/you/code/GT` for Codex
- the session stickiness
- spilling and `strict`
- the `↪` marker
- where to edit it: TUI Settings → `routingRules`, `g` on Accounts, the dashboard's Settings and Accounts screens, and the control API route

Add the keys to the config example block, and add a bullet beside the existing `routing.*` bullets.

- [ ] **Step 3: Full verification**

```bash
gofmt -l . ; go vet ./... ; go test ./... ; CGO_ENABLED=0 go build ./cmd/aiproxy
```

Expected: no gofmt output, vet clean, all PASS, build succeeds.

- [ ] **Step 4: Run it for real**

Start the built binary with a scratch `XDG_CONFIG_HOME`, sign into the dashboard, add a rule and set a group in both the TUI and the browser, and confirm each sees the other's change.

- [ ] **Step 5: Merge and release.** Do this only when the operator says to ship.

```bash
git checkout main && git merge --no-ff feat/web-dashboard -m "merge: web dashboard and routing rules"
git tag -a v0.5.0 -m "v0.5.0 …"   # notes: dashboard, routing rules, model override in the dashboard
git push origin main && git push origin v0.5.0
gh run watch "$(gh run list --workflow release.yml --limit 1 --json databaseId -q '.[0].databaseId')" --exit-status
```

Expected: the release run succeeds and `gh release view v0.5.0` lists four tarballs and `checksums.txt`.
