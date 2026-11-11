import { api, el, registerScreen, startPolling } from "../app.js";
import { lineChart, legend, formatPercent } from "../chart.js";
import { renderAccountCard } from "./overview.js";

// confirmDialog is an in-page dialog rather than window.confirm, so it can
// name the consequence. Resolves true on confirm.
function confirmDialog(title, consequence, action) {
  return new Promise((resolve) => {
    const d = el("dialog", {},
      el("h3", {}, title), el("p", {}, consequence),
      el("div", { class: "controls" },
        el("button", { onclick: () => { d.close(); resolve(false); } }, "Cancel"),
        el("button", { class: "danger", onclick: () => { d.close(); resolve(true); } }, action)));
    d.addEventListener("close", () => { d.remove(); resolve(false); });
    document.body.appendChild(d);
    d.showModal();
  });
}

const DAY = 86400e3;

registerScreen("accounts", {
  title: "Accounts",
  async mount(root) {
    let selected = null;
    let accounts = [], status = {};
    const errors = new Map(); // account id -> message, shown beside its controls
    const toolbarErr = el("span", { class: "field-error" });
    const list = el("div", { class: "cards" });
    const detail = el("div", { class: "detail" });
    const groupsList = el("datalist", { id: "account-groups" });

    const importSel = el("select", { "aria-label": "import source" },
      el("option", { value: "claude-code" }, "Claude Code"), el("option", { value: "codex" }, "Codex"));
    const toolbar = el("div", { class: "toolbar" },
      el("button", { onclick: async () => (await import("./login.js")).startLogin(() => load()) }, "Add account…"),
      el("span", { class: "inline" }, importSel, el("button", {
        onclick: () => mutate(null, async () => {
          const r = await api("/accounts/import", { method: "POST", body: JSON.stringify({ source: importSel.value }) });
          toolbarErr.textContent = r.added ? "" : "nothing new to import";
        }),
      }, "Import")),
      el("button", { onclick: () => mutate(null, () => api("/probe", { method: "POST" })) }, "Probe quota now"),
      toolbarErr);
    root.replaceChildren(el("h2", {}, "Accounts"), toolbar, groupsList, list, detail);

    // Every mutation re-fetches rather than updating local state: the server's
    // answer is the truth, and a control that appears to work but did not is
    // worse than a short wait.
    async function mutate(id, fn) {
      if (id) errors.delete(id); else toolbarErr.textContent = "";
      try { await fn(); } catch (e) {
        if (id) errors.set(id, e.message); else toolbarErr.textContent = e.message;
      }
      await load();
    }

    function controls(a) {
      const groupInput = el("input", { value: a.group || "", list: "account-groups", "aria-label": "routing group for " + a.label, placeholder: "ungrouped" });
      const saveGroup = () => mutate(a.id, () => api(`/accounts/${encodeURIComponent(a.id)}/group`, {
        method: "POST", body: JSON.stringify({ group: groupInput.value.trim() }) }));
      groupInput.addEventListener("keydown", (e) => { if (e.key === "Enter") saveGroup(); });
      const prio = (p) => mutate(a.id, () => api(`/accounts/${encodeURIComponent(a.id)}/priority`, {
        method: "POST", body: JSON.stringify({ priority: p }) }));
      return el("div", { class: "controls-block" },
        el("div", { class: "inline" }, el("span", { class: "muted small" }, "group"), groupInput, el("button", { onclick: saveGroup }, "Set")),
        el("div", { class: "controls" },
          el("button", { onclick: () => prio(Math.max(0, a.priority - 1)), disabled: a.priority <= 0, title: "raise priority" }, "▲ priority"),
          el("button", { onclick: () => prio(a.priority + 1), title: "lower priority" }, "▼"),
          el("button", {
            onclick: async () => {
              if (!a.disabled && !(await confirmDialog(`Disable ${a.label}?`,
                "It stops serving requests, and every session pinned to it moves to another account.", "Disable"))) return;
              mutate(a.id, () => api(`/accounts/${encodeURIComponent(a.id)}/enabled`, { method: "POST", body: JSON.stringify({ enabled: a.disabled }) }));
            },
          }, a.disabled ? "Enable" : "Disable"),
          el("button", {
            class: "danger",
            onclick: async () => {
              if (!(await confirmDialog(`Remove ${a.label}?`,
                "Its credential is deleted from config.json and cannot be recovered without logging in again. Its usage history stays.", "Remove"))) return;
              mutate(a.id, () => api(`/accounts/${encodeURIComponent(a.id)}`, { method: "DELETE" }));
              if (selected === a.id) { selected = null; detail.replaceChildren(); }
            },
          }, "Remove"),
          el("button", { onclick: () => { selected = selected === a.id ? null : a.id; render(); loadDetail(); } }, selected === a.id ? "Hide history" : "History")),
        errors.has(a.id) ? el("p", { class: "field-error" }, errors.get(a.id)) : null);
    }

    function render() {
      const groups = [...new Set(accounts.map((a) => a.group).filter(Boolean))].sort();
      groupsList.replaceChildren(...groups.map((g) => el("option", { value: g })));
      if (!accounts.length) {
        list.replaceChildren(el("div", { class: "empty-state" }, el("p", {}, "No accounts yet. Add one, or import from Claude Code or Codex.")));
        return;
      }
      // A focused input would be destroyed by a re-render mid-typing; skip
      // the poll's redraw while the operator is editing.
      if (list.contains(document.activeElement) && document.activeElement.tagName === "INPUT") return;
      list.replaceChildren(...accounts.map((a) => {
        const card = renderAccountCard(a, status, controls(a));
        if (a.id === selected) card.classList.add("selected");
        return card;
      }));
    }

    async function loadDetail() {
      if (!selected) { detail.replaceChildren(); return; }
      const a = accounts.find((x) => x.id === selected);
      const to = Date.now(), from = to - 7 * DAY;
      detail.replaceChildren(el("p", { class: "muted" }, "Loading quota history…"));
      try {
        const pts = await api(`/quota/history?account=${encodeURIComponent(selected)}&from=${from}&to=${to}`);
        const byBucket = new Map();
        const markers = [];
        let lastReset = new Map();
        for (const p of pts) {
          if (!byBucket.has(p.bucket)) byBucket.set(p.bucket, []);
          byBucket.get(p.bucket).push({ x: p.at, y: p.utilization });
          if (p.resetsAt && lastReset.has(p.bucket) && lastReset.get(p.bucket) !== p.resetsAt) {
            markers.push({ at: p.at, label: `${p.bucket} reset` });
          }
          lastReset.set(p.bucket, p.resetsAt);
        }
        const series = [...byBucket].map(([key, points]) => ({ key, points }));
        const width = Math.max(320, Math.min(detail.clientWidth || 900, 1100));
        detail.replaceChildren(el("section", { class: "panel" },
          el("h3", {}, `${a?.label || selected} — quota over the last 7 days`),
          lineChart({ series, width, height: 220, yMax: 1, format: formatPercent, xDomain: [from, to], markers, label: "quota utilisation" }),
          series.length > 1 ? legend(series.map((s) => s.key), { shape: "line" }) : null));
      } catch (e) {
        if (e.status === 401) throw e;
        detail.replaceChildren(el("div", { class: "error" }, el("p", {}, "Could not load quota history."), el("code", {}, e.message)));
      }
    }

    async function load() {
      try {
        [accounts, status] = await Promise.all([api("/accounts"), api("/status")]);
      } catch (e) {
        if (e.status === 401) throw e;
        list.replaceChildren(el("div", { class: "error" }, el("p", {}, "Could not load accounts."), el("code", {}, e.message),
          el("button", { onclick: load }, "Retry")));
        return;
      }
      render();
    }

    this.stop = startPolling(load, 5000);
  },
  unmount() { this.stop?.(); },
});
