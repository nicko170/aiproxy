import { api, el, registerScreen } from "../app.js";

// Mirrors view.restartSettingsFields: these are persisted at once but only
// adopted by the running proxy after a restart. Labelled up front, and the
// server's Applied answer is shown after every save rather than assumed.
const RESTART = new Set(["blockedModels", "retryBudgetMs", "inlineAbsorbMaxMs", "headerTimeoutMs", "bodyIdleMs",
  "quotaProbeIntervalSeconds", "metricsRetentionDays", "updateCheckIntervalHours",
  "privacyEnabled", "privacyOnScanFailure", "privacyOnUnresolved", "privacyScanTimeoutMs", "privacyDenylist"]);

// The two documented rule examples, verbatim from the README.
const EXAMPLES = [
  ["Claude Code", "Primary working directory: /Users/you/code/GT"],
  ["Codex", "<cwd>/Users/you/code/GT"],
];

const FIELDS = [
  ["Routing", [
    ["switchThreshold", "number", "Rotate to the next account when a quota bucket reaches this fraction (0–1).", { min: 0.01, max: 1, step: 0.01 }],
    ["sessionAffinity", "bool", "Keep a session on the account that served it, while that account can serve."],
    ["modelOverride", "text", "Send every request in this model's family as this model, whatever the client asked for. Empty sends what was asked."],
    ["blockedModels", "list", "Models this proxy refuses: shell globs such as *fable*, comma-separated. Empty allows all."],
  ]],
  ["Timeouts", [
    ["retryBudgetMs", "int", "Time the proxy may add before the first byte: backoff, waiting on a paused account, refreshing a credential (ms)."],
    ["inlineAbsorbMaxMs", "int", "Longest upstream-suggested wait absorbed silently instead of rotating (ms)."],
    ["headerTimeoutMs", "int", "How long one attempt waits for upstream response headers — the model's own thinking time (ms)."],
    ["bodyIdleMs", "int", "How long a streaming body may stall before the relay gives up (ms)."],
  ]],
  ["Housekeeping", [
    ["quotaProbeIntervalSeconds", "int", "How often the background probe re-reads each account's quota (s). 0 turns the background probe off."],
    ["metricsRetentionDays", "int", "How long per-request accounting rows are kept (days)."],
    ["updateCheckEnabled", "bool", "Check GitHub for newer releases in the background."],
    ["updateCheckIntervalHours", "int", "How often to check for a newer release (hours)."],
  ]],
  ["Privacy", [
    ["privacyEnabled", "bool", "Redact secrets and PII from request bodies before they leave this machine."],
    ["privacyOnScanFailure", ["closed", "open"], "If a scan fails: closed refuses the request, open sends it unfiltered."],
    ["privacyOnUnresolved", ["passthrough", "error"], "If a response mentions a placeholder the proxy cannot restore."],
    ["privacyScanTimeoutMs", "int", "Bound on one request's scan (ms)."],
    ["privacyDenylist", "list", "Literal strings always redacted, comma-separated."],
  ]],
];

function input(name, kind, value, opts = {}) {
  if (kind === "bool") return el("input", { type: "checkbox", id: name, checked: value ? "" : undefined });
  if (Array.isArray(kind)) {
    const s = el("select", { id: name }, kind.map((o) => el("option", { value: o }, o)));
    s.value = value || kind[0];
    return s;
  }
  if (kind === "list") return el("input", { type: "text", id: name, value: (value || []).join(", ") });
  if (kind === "number" || kind === "int") return el("input", { type: "number", id: name, value: value ?? "", step: opts.step ?? 1, min: opts.min, max: opts.max });
  return el("input", { type: "text", id: name, value: value ?? "" });
}

function readInput(node, kind) {
  if (kind === "bool") return node.checked;
  if (kind === "list") return node.value.split(",").map((s) => s.trim()).filter(Boolean);
  if (kind === "int") return parseInt(node.value, 10);
  if (kind === "number") return parseFloat(node.value);
  return node.value;
}

// routingCard edits routingRules, defaultGroup and defaultStrict. It owns a
// copy of the rules and reports them through collect().
function routingCard(s, groups) {
  const rules = (s.routingRules || []).map((r) => ({ ...r }));
  const groupList = el("datalist", { id: "routing-groups" }, groups.map((g) => el("option", { value: g })));
  const defaultGroup = el("input", { type: "text", id: "defaultGroup", list: "routing-groups", value: s.defaultGroup ?? "", placeholder: "ungrouped" });
  const defaultStrict = el("input", { type: "checkbox", id: "defaultStrict", checked: s.defaultStrict ? "" : undefined });
  const list = el("ol", { class: "rules" });

  const warn = (group) => groups.includes(group) || (group === "" && groups.includes("")) ? null
    : el("p", { class: "field-error" }, "no accounts in this group");

  function draw() {
    list.replaceChildren(...rules.map((r, i) => {
      const match = el("textarea", { rows: 2, "aria-label": `rule ${i + 1} text`, oninput: (e) => { r.match = e.target.value; } }, r.match);
      const group = el("input", { type: "text", list: "routing-groups", value: r.group, "aria-label": `rule ${i + 1} group`, placeholder: "ungrouped",
        oninput: (e) => { r.group = e.target.value; }, onchange: draw });
      const strict = el("input", { type: "checkbox", checked: r.strict ? "" : undefined, onchange: (e) => { r.strict = e.target.checked; } });
      const move = (d) => { const j = i + d; [rules[i], rules[j]] = [rules[j], rules[i]]; draw(); };
      return el("li", { class: "rule" },
        el("div", { class: "rule-main" }, match,
          el("div", { class: "inline" },
            el("label", { class: "inline" }, "prefer group", group),
            el("label", { class: "inline", title: "never spill outside this group, even when every account in it is spent" }, strict, "strict"),
            el("button", { type: "button", disabled: i === 0, onclick: () => move(-1), "aria-label": "move up" }, "▲"),
            el("button", { type: "button", disabled: i === rules.length - 1, onclick: () => move(1), "aria-label": "move down" }, "▼"),
            el("button", { type: "button", class: "danger", onclick: () => { rules.splice(i, 1); draw(); } }, "Remove")),
          warn(r.group.trim())));
    }));
    if (!rules.length) list.appendChild(el("li", { class: "muted" }, "No rules: every request prefers the default group."));
  }
  draw();

  const card = el("section", { class: "panel" },
    el("h3", {}, "Routing rules"),
    el("p", { class: "small muted" },
      "A rule is literal text. When a request contains it, the request prefers that rule's group of accounts. Rules are tried in order and the first match wins. ",
      "When every account in the group is spent, work spills onto the others unless the rule is strict. ",
      "A Claude Code session keeps the group its first matching request chose."),
    el("ul", { class: "small examples" }, EXAMPLES.map(([who, ex]) => el("li", {}, who + ": ", el("code", {}, ex)))),
    groupList, list,
    el("button", { type: "button", onclick: () => { rules.push({ match: "", group: "", strict: false }); draw(); } }, "Add rule"),
    el("div", { class: "row" }, el("label", { for: "defaultGroup" }, "default group"), defaultGroup,
      el("span", { class: "help" }, "Preferred by work no rule matches. Empty means ungrouped accounts.")),
    el("div", { class: "row" }, el("label", { for: "defaultStrict" }, "default strict"), defaultStrict,
      el("span", { class: "help" }, "Unmatched work never spills outside the default group.")));

  // Always sent, an empty list included: omitting routingRules means "keep
  // what is there", so deleting the last rule must say [] explicitly.
  card.collect = () => ({
    routingRules: rules.map((r) => ({ match: r.match, group: r.group.trim(), strict: !!r.strict })),
    defaultGroup: defaultGroup.value.trim(),
    defaultStrict: defaultStrict.checked,
  });
  return card;
}

registerScreen("settings", {
  title: "Settings",
  // Read once on entry, never polled: a form that reloads under the operator
  // mid-edit is hostile.
  async mount(root) {
    const [s, accounts] = await Promise.all([api("/settings"), api("/accounts")]);
    const groups = [...new Set(accounts.map((a) => a.group || ""))].sort();
    const nodes = new Map();
    const result = el("div", { class: "result", "aria-live": "polite" });

    const sections = FIELDS.map(([title, fields]) => el("section", { class: "panel" }, el("h3", {}, title),
      fields.map(([name, kind, help, opts]) => {
        const node = input(name, kind, s[name], opts);
        nodes.set(name, [node, kind]);
        return el("div", { class: "row" }, el("label", { for: name }, name), node,
          el("span", { class: "help" }, help, RESTART.has(name) ? el("span", { class: "pill warn" }, "takes effect on restart") : null));
      })));
    const routing = routingCard(s, groups);
    sections.splice(1, 0, routing);

    const save = el("button", { class: "primary", onclick: async () => {
      const next = { ...s };
      for (const [name, [node, kind]] of nodes) next[name] = readInput(node, kind);
      Object.assign(next, routing.collect());
      result.replaceChildren(el("p", { class: "muted" }, "Saving…"));
      try {
        const applied = await api("/settings", { method: "POST", body: JSON.stringify(next) });
        const live = applied.live || [], restart = applied.needsRestart || [];
        // replaceChildren would print a null as the text "null": filter.
        result.replaceChildren(...[
          el("p", {}, live.length || restart.length ? "Saved." : "Saved — nothing changed."),
          live.length ? el("p", { class: "small" }, "Applied now: " + live.join(", ")) : null,
          restart.length ? el("p", { class: "small warn-text" }, "Saved, takes effect on restart: " + restart.join(", ")) : null,
        ].filter(Boolean));
        Object.assign(s, await api("/settings"));
      } catch (e) {
        if (e.status === 401) throw e;
        // The form is left exactly as edited, so nothing is lost.
        result.replaceChildren(el("p", { class: "error-text" }, e.message));
      }
    } }, "Save");

    root.replaceChildren(el("h2", {}, "Settings"), ...sections, el("div", { class: "savebar" }, save, result));
  },
});
