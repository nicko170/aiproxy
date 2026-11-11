import { api, el, registerScreen, startPolling } from "../app.js";
import { stackedArea, barChart, legend, niceTicks, costAxisFormat, formatTokens, formatCostMicros, formatMs } from "../chart.js";

const RANGES = { "1h": 3600e3, "24h": 86400e3, "7d": 7 * 86400e3, "30d": 30 * 86400e3 };
const GROUPS = ["account", "model", "outcome"];

// The view lives in the hash (#/usage?range=24h&group=model) so it is
// linkable and survives a reload.
function readView() {
  const q = new URLSearchParams(location.hash.split("?")[1] || "");
  const range = RANGES[q.get("range")] ? q.get("range") : "24h";
  const group = GROUPS.includes(q.get("group")) ? q.get("group") : "account";
  return { range, group };
}
function writeView(v) { location.hash = `#/usage?range=${v.range}&group=${v.group}`; }

function segmented(options, current, onPick, label) {
  return el("div", { class: "segmented", role: "group", "aria-label": label },
    options.map((o) => el("button", { class: o === current ? "on" : "", "aria-pressed": o === current, onclick: () => onPick(o) }, o)));
}

function stat(label, value, note) {
  return el("div", { class: "stat" }, el("span", { class: "stat-label" }, label), el("span", { class: "stat-value" }, value),
    note ? el("span", { class: "stat-note" }, note) : null);
}

// stackTotals is each bucket's summed value, for sizing an axis.
function stackTotals(points) {
  const by = new Map();
  for (const p of points) by.set(p.x, (by.get(p.x) || 0) + p.value);
  return [...by.values()];
}

registerScreen("usage", {
  title: "Usage",
  async mount(root) {
    const view = readView();
    const labels = new Map();
    const controls = el("div", { class: "toolbar" },
      segmented(Object.keys(RANGES), view.range, (r) => writeView({ ...view, range: r }), "range"),
      segmented(GROUPS, view.group, (g) => writeView({ ...view, group: g }), "group by"));
    const body = el("div", { class: "usage" }, el("p", { class: "muted" }, "Loading…"));
    root.replaceChildren(el("h2", {}, "Usage"), controls, body);

    const load = async () => {
      const to = Date.now(), from = to - RANGES[view.range];
      // minute and hour are the only rollups the store keeps; minutes over
      // 30 days would be a series no chart can draw.
      const granularity = view.range === "1h" ? "minute" : "hour";
      const w = `from=${from}&to=${to}`;
      let series, totals, latency;
      try {
        [series, totals, latency] = await Promise.all([
          api(`/usage?${w}&granularity=${granularity}&groupBy=${view.group}`),
          api(`/totals?${w}`), api(`/latency?${w}`),
        ]);
        if (view.group === "account" && !labels.size) {
          for (const a of await api("/accounts")) labels.set(a.id, a.label);
        }
      } catch (e) {
        if (e.status === 401) throw e;
        body.replaceChildren(el("div", { class: "error" }, el("p", {}, "Could not load usage."), el("code", {}, e.message),
          el("button", { onclick: load }, "Retry")));
        return;
      }

      const name = (k) => (view.group === "account" ? labels.get(k) || k : k);
      const pts = series.points || [];
      const keys = [...new Set(pts.map((p) => p.key))].sort();
      const width = Math.max(320, Math.min(body.clientWidth || 900, 1100));
      const long = (field) => pts.map((p) => ({ x: p.bucketStart, key: name(p.key), value: p[field] }));
      const names = keys.map(name);

      // Cost is a floor, not a total, while any request had no known price
      // (spec §7.4) — the same rule the TUI follows.
      const cost = totals.unpricedRequests
        ? `≥ ${formatCostMicros(totals.costMicros)}`
        : formatCostMicros(totals.costMicros);
      const costNote = totals.unpricedRequests ? `${totals.unpricedRequests} request${totals.unpricedRequests === 1 ? "" : "s"} unpriced` : null;

      const out = [
        el("div", { class: "strip" },
          stat("requests", formatTokens(totals.requests)),
          stat("input tokens", formatTokens(totals.inputTokens)),
          stat("output tokens", formatTokens(totals.outputTokens)),
          stat("cache read", formatTokens(totals.cacheReadTokens)),
          stat("cache write", formatTokens(totals.cacheWriteTokens)),
          stat("cost", cost, costNote),
          // Latency is one aggregate over the window, not a series: stat
          // tiles, not lines.
          stat("TTFB p50", latency.ttfbP50Ms > 0 ? formatMs(latency.ttfbP50Ms) : "—"),
          stat("TTFB p95", latency.ttfbP95Ms > 0 ? formatMs(latency.ttfbP95Ms) : "—"),
          stat("duration p95", latency.durationP95Ms > 0 ? formatMs(latency.durationP95Ms) : "—")),
      ];
      const chart = (title, node) => el("section", { class: "panel" }, el("h3", {}, title), node, names.length > 1 ? legend(names) : null);
      const xDomain = [from, to];
      const costPts = long("costMicros");
      const costTop = Math.max(0, ...stackTotals(costPts));
      out.push(chart(`Input tokens by ${view.group}`, stackedArea({ points: long("inputTokens"), keys: names, width, height: 220, xDomain, label: "input tokens" })));
      out.push(chart(`Output tokens by ${view.group}`, stackedArea({ points: long("outputTokens"), keys: names, width, height: 220, xDomain, label: "output tokens" })));
      out.push(chart(`Cost by ${view.group}`, stackedArea({ points: costPts, keys: names, width, height: 200, xDomain,
        format: formatCostMicros, axisFormat: costAxisFormat(niceTicks(0, costTop, 4)), label: "cost" })));
      if (view.group === "outcome") {
        const sums = new Map();
        for (const p of pts) sums.set(p.key, (sums.get(p.key) || 0) + p.requests);
        out.push(el("section", { class: "panel" }, el("h3", {}, "Outcome mix (requests)"),
          barChart({ bars: [...sums].sort((a, b) => b[1] - a[1]).map(([k, v]) => ({ label: k, value: v })), width, height: 200, format: formatTokens })));
      }
      body.replaceChildren(...out);
    };
    this.stop = startPolling(load, 30000);
  },
  unmount() { this.stop?.(); },
});
