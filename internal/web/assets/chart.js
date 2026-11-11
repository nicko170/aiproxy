import { el } from "./dom.js";

// Chart maths: pure functions from data to numbers. No DOM, no SVG — that is
// what lets node --test cover this module (see charts_test.go). The marks that
// draw with these live further down, built through dom.js's el(), which only
// touches the document inside function bodies.

// linearScale maps a numeric domain onto a pixel range. A zero-width domain —
// one account with flat traffic, on every render — maps to the middle of the
// range instead of dividing by zero.
export function linearScale({ domain: [lo, hi], range: [a, b] }) {
  const span = hi - lo;
  const s = (v) => (span === 0 ? (a + b) / 2 : a + ((v - lo) / span) * (b - a));
  s.invert = (px) => (b === a ? lo : lo + ((px - a) / (b - a)) * span);
  s.domain = [lo, hi];
  s.range = [a, b];
  return s;
}

// timeScale is a linearScale over unix-ms timestamps.
export function timeScale(opts) {
  return linearScale(opts);
}

// niceTicks picks a 1/2/5×10ⁿ step near the requested count and walks from a
// floored start to a ceilinged end. An all-zero series still gets an axis.
export function niceTicks(lo, hi, target = 5) {
  if (!Number.isFinite(lo) || !Number.isFinite(hi)) return [0, 1];
  if (hi < lo) [lo, hi] = [hi, lo];
  if (hi === lo) {
    if (hi === 0) { lo = 0; hi = 1; } else { lo = Math.min(0, lo); hi = Math.max(0, hi); }
  }
  const raw = (hi - lo) / Math.max(1, target);
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? 10 * mag;
  const start = Math.floor(lo / step) * step;
  const end = Math.ceil(hi / step) * step;
  const out = [];
  for (let v = start; v <= end + step / 2; v += step) out.push(+v.toFixed(12));
  return out;
}

const MIN = 60e3, HOUR = 3600e3, DAY = 86400e3;

// timeTicks chooses its unit from the span: minutes under an hour, hours under
// two days, days beyond. Times are LOCAL — "the spike at 14:00" means the
// operator's own clock — and labels use the browser's locale.
export function timeTicks(t0, t1, target = 6) {
  const span = t1 - t0;
  if (!(span > 0)) return [{ at: t0, label: fmtClock(t0) }, { at: t1, label: fmtClock(t1) }];
  const raw = span / Math.max(1, target);
  let steps, fmt;
  if (span <= HOUR) {
    steps = [1, 2, 5, 10, 15, 30, 60].map((m) => m * MIN);
    fmt = fmtClock;
  } else if (span <= 2 * DAY) {
    steps = [1, 2, 3, 4, 6, 12, 24].map((h) => h * HOUR);
    fmt = fmtClock;
  } else {
    steps = [1, 2, 3, 7, 14, 30].map((d) => d * DAY);
    fmt = fmtDay;
  }
  const step = steps.find((s) => s >= raw) ?? steps[steps.length - 1];
  // Align to local wall-clock boundaries so labels read 14:00, not 14:07.
  const offset = new Date(t0).getTimezoneOffset() * MIN;
  let at = Math.ceil((t0 - offset) / step) * step + offset;
  const out = [];
  for (; at <= t1; at += step) out.push({ at, label: fmt(at) });
  return out;
}

const clockFmt = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
const dayFmt = new Intl.DateTimeFormat(undefined, { day: "2-digit", month: "short" });
function fmtClock(t) { return clockFmt.format(new Date(t)); }
function fmtDay(t) { return dayFmt.format(new Date(t)); }

function compact(n, div, suffix) {
  const v = n / div;
  return (v >= 100 ? v.toFixed(0) : v.toFixed(1).replace(/\.0$/, "")) + suffix;
}

export function formatTokens(n) {
  const a = Math.abs(n);
  if (a >= 1e9) return compact(n, 1e9, "B");
  if (a >= 1e6) return compact(n, 1e6, "M");
  if (a >= 1e3) return compact(n, 1e3, "K");
  return String(Math.round(n));
}

// formatCostMicros works on integer micros so cost never picks up float
// drift. Under a cent it shows four decimals rather than reading "$0.00".
export function formatCostMicros(micros) {
  const m = Math.round(micros);
  if (m !== 0 && Math.abs(m) < 10_000) {
    return "$" + (m / 1e6).toFixed(4);
  }
  const cents = Math.floor(m / 10_000);
  const dollars = Math.floor(cents / 100);
  return "$" + dollars + "." + String(cents % 100).padStart(2, "0");
}

export function formatMs(ms) {
  if (ms < 1000) return Math.round(ms) + "ms";
  return (ms / 1000).toFixed(2).replace(/0$/, "").replace(/\.$/, "") + "s";
}

export function formatPercent(f) {
  return Math.round(f * 100) + "%";
}

// stack turns long-form points into one series per key, cumulative in key
// order. A key missing from a bucket contributes zero rather than a hole that
// would shift every series above it.
export function stack(points, keys) {
  const xs = [...new Set(points.map((p) => p.x))].sort((a, b) => a - b);
  const byX = new Map(xs.map((x) => [x, new Map()]));
  for (const p of points) {
    const m = byX.get(p.x);
    m.set(p.key, (m.get(p.key) || 0) + p.value);
  }
  const base = new Map(xs.map((x) => [x, 0]));
  return keys.map((key) => ({
    key,
    values: xs.map((x) => {
      const y0 = base.get(x);
      const y1 = y0 + (byX.get(x).get(key) || 0);
      base.set(x, y1);
      return { x, y0, y1 };
    }),
  }));
}

// colorFor gives each key one slot of the categorical palette (style.css
// --series-1..8, the dataviz skill's validated reference palette, stepped
// separately for dark mode). A key starts at its hash slot; if another key
// already holds that slot this page, it takes the next free one, so two
// series on screen never share a colour while slots remain. Assignments live
// for the page, so the Usage legend agrees with the Overview cards, and a
// key's slot is the same across reloads unless it collided. A ninth key
// reuses its hash slot: past eight, fold series into "other".
export const SERIES_SLOTS = 8;
const assigned = new Map();
const taken = new Set();

export function colorFor(key) {
  return `var(--series-${slotFor(key) + 1})`;
}

export function slotFor(key) {
  const k = String(key);
  if (assigned.has(k)) return assigned.get(k);
  const home = hashSlot(k);
  let slot = home;
  for (let i = 0; i < SERIES_SLOTS; i++) {
    const s = (home + i) % SERIES_SLOTS;
    if (!taken.has(s)) { slot = s; break; }
  }
  assigned.set(k, slot);
  taken.add(slot);
  return slot;
}

function hashSlot(k) {
  let h = 2166136261;
  for (const ch of k) {
    h ^= ch.codePointAt(0);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0) % SERIES_SLOTS;
}

// ---------------------------------------------------------------------------
// Marks. Everything below builds SVG through el(), so values from the API
// become text nodes and attributes, never markup. Mark specs follow the
// dataviz skill: 2px lines, a ~10% area wash under each line, hairline solid
// gridlines in --line, text in --fg/--muted (never the series colour),
// bars ≤24px with a 4px rounded data end.

const M = { l: 52, r: 14, t: 10, b: 26 };

function frame(width, height, label) {
  const svg = el("svg", {
    viewBox: `0 0 ${width} ${height}`, width: "100%", class: "chart",
    role: "img", "aria-label": label || "chart", preserveAspectRatio: "xMidYMid meet",
  });
  if (label) svg.appendChild(el("title", {}, label));
  return svg;
}

function yAxis(svg, y, ticks, width, format) {
  const g = el("g", { class: "axis y" });
  for (const t of ticks) {
    const py = y(t);
    g.appendChild(el("line", { x1: M.l, x2: width - M.r, y1: py, y2: py, class: "grid" }));
    g.appendChild(el("text", { x: M.l - 6, y: py, "text-anchor": "end", "dominant-baseline": "middle", class: "tick" }, format(t)));
  }
  svg.appendChild(g);
}

function xAxis(svg, x, ticks, height) {
  const g = el("g", { class: "axis x" });
  for (const t of ticks) {
    g.appendChild(el("text", { x: x(t.at), y: height - 6, "text-anchor": "middle", class: "tick" }, t.label));
  }
  svg.appendChild(g);
}

// crosshair wires the one tooltip implementation: a vertical hairline that
// snaps to the nearest bucket and lists every series at it. The pointer and
// the arrow keys (when the chart has focus) drive the same index, so the
// data is never mouse-only.
function crosshair(svg, { xs, x, height, width, rows }) {
  if (xs.length === 0) return;
  svg.setAttribute("tabindex", "0");
  const line = el("line", { class: "crosshair", y1: M.t, y2: height - M.b, visibility: "hidden" });
  const tip = el("g", { class: "tooltip", visibility: "hidden" });
  svg.append(line, tip);
  let idx = -1;

  const show = (i) => {
    idx = Math.max(0, Math.min(xs.length - 1, i));
    const px = x(xs[idx]);
    line.setAttribute("x1", px); line.setAttribute("x2", px);
    line.setAttribute("visibility", "visible");
    const lines = rows(idx); // [{color?, value, name}]
    tip.replaceChildren();
    const lh = 15, padX = 8, w = 170, h = 8 + lh * (lines.length + 1);
    const tx = px + 10 + w > width - M.r ? px - 10 - w : px + 10;
    tip.appendChild(el("rect", { x: tx, y: M.t, width: w, height: h, rx: 4, class: "tip-bg" }));
    tip.appendChild(el("text", { x: tx + padX, y: M.t + lh, class: "tip-head" }, tipTime(xs[idx])));
    lines.forEach((r, j) => {
      const ty = M.t + lh * (j + 2);
      if (r.color) tip.appendChild(el("line", { x1: tx + padX, x2: tx + padX + 10, y1: ty - 4, y2: ty - 4, stroke: r.color, class: "tip-key" }));
      tip.appendChild(el("text", { x: tx + padX + 16, y: ty, class: "tip-row" },
        el("tspan", { class: "tip-value" }, r.value), el("tspan", { dx: 6, class: "tip-name" }, r.name)));
    });
    tip.setAttribute("visibility", "visible");
  };
  const hide = () => { line.setAttribute("visibility", "hidden"); tip.setAttribute("visibility", "hidden"); };
  const nearest = (px) => {
    const t = x.invert(px);
    let best = 0;
    for (let i = 1; i < xs.length; i++) if (Math.abs(xs[i] - t) < Math.abs(xs[best] - t)) best = i;
    return best;
  };

  svg.addEventListener("pointermove", (e) => {
    const r = svg.getBoundingClientRect();
    if (!r.width) return;
    show(nearest(((e.clientX - r.left) / r.width) * width));
  });
  svg.addEventListener("pointerleave", hide);
  svg.addEventListener("blur", hide);
  svg.addEventListener("focus", () => show(idx < 0 ? xs.length - 1 : idx));
  svg.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft") { show(idx - 1); e.preventDefault(); }
    if (e.key === "ArrowRight") { show(idx + 1); e.preventDefault(); }
    if (e.key === "Escape") hide();
  });
}

const tipFmt = typeof Intl !== "undefined"
  ? new Intl.DateTimeFormat(undefined, { month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false })
  : null;
function tipTime(t) { return tipFmt ? tipFmt.format(new Date(t)) : String(t); }

// emptyChart is a chart that has nothing to show — visibly distinct from a
// failed fetch, which the screen renders itself. Axes stay, so "zero" never
// reads as "broken".
export function emptyChart(message, { width = 640, height = 200 } = {}) {
  const svg = frame(width, height, message);
  svg.classList.add("empty");
  const y = linearScale({ domain: [0, 1], range: [height - M.b, M.t] });
  yAxis(svg, y, [0, 1], width, () => "");
  svg.appendChild(el("text", { x: width / 2, y: height / 2, "text-anchor": "middle", class: "empty-text" }, message));
  return svg;
}

// stackedArea draws long-form points ({x, key, value}) stacked in key order:
// each band is a ~10% wash under a 2px line at its top edge.
export function stackedArea({ points, keys, width = 640, height = 220, format = formatTokens, label }) {
  if (!points.length || !keys.length) return emptyChart("No traffic in this range", { width, height });
  const series = stack(points, keys);
  const xs = series[0].values.map((v) => v.x);
  const top = Math.max(...series[series.length - 1].values.map((v) => v.y1));
  const yt = niceTicks(0, top, 4);
  const x = timeScale({ domain: [xs[0], xs[xs.length - 1]], range: [M.l, width - M.r] });
  const y = linearScale({ domain: [0, yt[yt.length - 1]], range: [height - M.b, M.t] });
  const svg = frame(width, height, label || "stacked area: " + keys.join(", "));
  yAxis(svg, y, yt, width, format);
  xAxis(svg, x, timeTicks(xs[0], xs[xs.length - 1], 6), height);

  for (const s of series) {
    const c = colorFor(s.key);
    const upper = s.values.map((v) => `${x(v.x)},${y(v.y1)}`);
    const lower = s.values.map((v) => `${x(v.x)},${y(v.y0)}`).reverse();
    const g = el("g", { class: "series" }, el("title", {}, s.key));
    if (xs.length === 1) {
      g.appendChild(el("circle", { cx: x(xs[0]), cy: y(s.values[0].y1), r: 4, fill: c, class: "dot" }));
    } else {
      g.appendChild(el("polygon", { points: upper.concat(lower).join(" "), fill: c, class: "wash" }));
      g.appendChild(el("polyline", { points: upper.join(" "), stroke: c, class: "stroke" }));
    }
    svg.appendChild(g);
  }
  crosshair(svg, {
    xs, x, height, width,
    rows: (i) => series.slice().reverse().map((s) => ({
      color: colorFor(s.key), name: s.key, value: format(s.values[i].y1 - s.values[i].y0),
    })),
  });
  return svg;
}

// lineChart draws independent series ([{key, points: [{x, y}]}]) on one axis.
export function lineChart({ series, width = 640, height = 200, format = (v) => String(v), label, yMax }) {
  const all = series.flatMap((s) => s.points);
  if (!all.length) return emptyChart("No data in this range", { width, height });
  const xs = [...new Set(all.map((p) => p.x))].sort((a, b) => a - b);
  const yt = niceTicks(0, yMax ?? Math.max(...all.map((p) => p.y)), 4);
  const x = timeScale({ domain: [xs[0], xs[xs.length - 1]], range: [M.l, width - M.r] });
  const y = linearScale({ domain: [0, yt[yt.length - 1]], range: [height - M.b, M.t] });
  const svg = frame(width, height, label || "line chart: " + series.map((s) => s.key).join(", "));
  yAxis(svg, y, yt, width, format);
  xAxis(svg, x, timeTicks(xs[0], xs[xs.length - 1], 6), height);
  for (const s of series) {
    const c = colorFor(s.key);
    const g = el("g", { class: "series" }, el("title", {}, s.key));
    const pts = s.points.slice().sort((a, b) => a.x - b.x);
    if (pts.length === 1) {
      g.appendChild(el("circle", { cx: x(pts[0].x), cy: y(pts[0].y), r: 4, fill: c, class: "dot" }));
    } else {
      g.appendChild(el("polyline", { points: pts.map((p) => `${x(p.x)},${y(p.y)}`).join(" "), stroke: c, class: "stroke" }));
    }
    svg.appendChild(g);
  }
  const at = series.map((s) => new Map(s.points.map((p) => [p.x, p.y])));
  crosshair(svg, {
    xs, x, height, width,
    rows: (i) => series.map((s, j) => ({
      color: colorFor(s.key), name: s.key,
      value: at[j].has(xs[i]) ? format(at[j].get(xs[i])) : "—",
    })),
  });
  return svg;
}

// barChart draws categories ([{label, value, key?}]) as vertical bars: ≤24px,
// 4px rounded at the data end, square on the baseline, each bar its own
// hover and focus target carrying its value.
export function barChart({ bars, width = 640, height = 200, format = (v) => String(v), label }) {
  if (!bars.length) return emptyChart("Nothing to show", { width, height });
  const yt = niceTicks(0, Math.max(...bars.map((b) => b.value)), 4);
  const y = linearScale({ domain: [0, yt[yt.length - 1]], range: [height - M.b, M.t] });
  const svg = frame(width, height, label || "bar chart");
  yAxis(svg, y, yt, width, format);
  const band = (width - M.l - M.r) / bars.length;
  const bw = Math.min(24, band * 0.6);
  bars.forEach((b, i) => {
    const cx = M.l + band * (i + 0.5);
    const y0 = y(0), y1 = y(b.value), h = Math.max(0, y0 - y1);
    const r = Math.min(4, h / 2, bw / 2);
    const x0 = cx - bw / 2;
    // A path, so only the data end is rounded and the baseline stays square.
    const d = h === 0 ? "" :
      `M${x0},${y0} V${y1 + r} Q${x0},${y1} ${x0 + r},${y1} H${x0 + bw - r} Q${x0 + bw},${y1} ${x0 + bw},${y1 + r} V${y0} Z`;
    const g = el("g", { class: "bar", tabindex: 0 },
      el("title", {}, `${b.label}: ${format(b.value)}`),
      el("rect", { x: cx - band / 2, y: M.t, width: band, height: y0 - M.t, class: "hit" }),
      d ? el("path", { d, fill: colorFor(b.key ?? b.label) }) : null,
      el("text", { x: cx, y: height - 6, "text-anchor": "middle", class: "tick" }, b.label),
      el("text", { x: cx, y: y1 - 4, "text-anchor": "middle", class: "bar-value" }, format(b.value)));
    svg.appendChild(g);
  });
  return svg;
}

// gauge is a meter: one bar, its fill carrying severity (ok → warning →
// danger) with the label and percentage in text tokens beside it.
export function gauge({ fraction, label, sublabel }) {
  const f = Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0));
  const sev = f >= 0.9 ? "danger" : f >= 0.7 ? "warning" : "ok";
  const w = 200, h = 34;
  const svg = frame(w, h, `${label}: ${formatPercent(f)}`);
  svg.classList.add("gauge");
  svg.append(
    el("text", { x: 0, y: 11, class: "gauge-label" }, label),
    el("text", { x: w, y: 11, "text-anchor": "end", class: "gauge-value" }, formatPercent(f)),
    el("rect", { x: 0, y: 17, width: w, height: 8, rx: 4, class: "gauge-track" }),
    f > 0 ? el("rect", { x: 0, y: 17, width: Math.max(8, w * f), height: 8, rx: 4, class: "gauge-fill " + sev }) : null);
  if (sublabel) {
    svg.appendChild(el("text", { x: 0, y: h + 6, class: "gauge-sub" }, sublabel));
    svg.setAttribute("viewBox", `0 0 ${w} ${h + 14}`);
  }
  return svg;
}

// legend is a swatch AND the key's text for every series: identity is never
// colour alone.
export function legend(keys, { shape = "rect" } = {}) {
  return el("ul", { class: "legend" }, keys.map((k) =>
    el("li", {}, el("span", { class: "swatch " + shape, style: `background:${colorFor(k)}` }), String(k))));
}
