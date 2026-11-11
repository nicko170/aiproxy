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

// colorFor hashes a key onto the categorical palette (style.css --series-1..8,
// the dataviz skill's validated reference palette, stepped separately for
// dark mode). The same key always gets the same slot, across screens and
// reloads, so the Usage legend agrees with the Overview cards.
export const SERIES_SLOTS = 8;
export function colorFor(key) {
  return `var(--series-${slotFor(key) + 1})`;
}

export function slotFor(key) {
  let h = 2166136261;
  for (const ch of String(key)) {
    h ^= ch.codePointAt(0);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0) % SERIES_SLOTS;
}
