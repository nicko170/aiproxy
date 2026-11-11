import test from "node:test";
import assert from "node:assert/strict";
import {
  linearScale, timeScale, niceTicks, timeTicks,
  formatTokens, formatCostMicros, formatMs, formatPercent,
  stack, colorFor,
} from "./chart.js";

test("linearScale maps domain onto range", () => {
  const s = linearScale({ domain: [0, 100], range: [0, 200] });
  assert.equal(s(0), 0);
  assert.equal(s(50), 100);
  assert.equal(s(100), 200);
});

// A flat series — every value identical — is the case that divides by zero.
// It happens constantly in practice: one account, one model, no traffic.
test("linearScale survives a zero-width domain", () => {
  const s = linearScale({ domain: [5, 5], range: [0, 100] });
  const v = s(5);
  assert.ok(Number.isFinite(v), `expected a finite value, got ${v}`);
});

test("linearScale inverts", () => {
  const s = linearScale({ domain: [0, 10], range: [0, 500] });
  assert.equal(Math.round(s.invert(250)), 5);
});

test("niceTicks produces round numbers covering the range", () => {
  const ticks = niceTicks(0, 97, 5);
  assert.ok(ticks.length >= 3 && ticks.length <= 8, `got ${ticks.length} ticks`);
  assert.equal(ticks[0], 0);
  assert.ok(ticks[ticks.length - 1] >= 97);
  for (const t of ticks) assert.ok(Number.isFinite(t));
});

test("niceTicks handles an all-zero series", () => {
  const ticks = niceTicks(0, 0, 5);
  assert.ok(ticks.length >= 2, "an all-zero series still needs an axis");
  assert.ok(ticks.every(Number.isFinite));
});

// The four ranges the Usage screen offers. Each must produce a readable number
// of labels — not one, not forty.
test("timeTicks is sane across every offered range", () => {
  const end = Date.UTC(2026, 7, 25, 12, 0, 0);
  for (const [name, span] of [
    ["1h", 3600e3], ["24h", 86400e3], ["7d", 7 * 86400e3], ["30d", 30 * 86400e3],
  ]) {
    const ticks = timeTicks(end - span, end, 6);
    assert.ok(ticks.length >= 2 && ticks.length <= 12, `${name}: ${ticks.length} ticks`);
    for (const t of ticks) {
      assert.ok(t.at >= end - span && t.at <= end, `${name}: tick outside range`);
      assert.ok(t.label.length > 0 && t.label.length <= 12, `${name}: bad label ${t.label}`);
    }
  }
});

test("token formatting is compact and lossless enough to read", () => {
  assert.equal(formatTokens(0), "0");
  assert.equal(formatTokens(999), "999");
  assert.equal(formatTokens(1500), "1.5K");
  assert.equal(formatTokens(1_500_000), "1.5M");
  assert.equal(formatTokens(2_300_000_000), "2.3B");
});

test("cost is formatted from micros without float drift", () => {
  assert.equal(formatCostMicros(0), "$0.00");
  assert.equal(formatCostMicros(1_000_000), "$1.00");
  assert.equal(formatCostMicros(223_333_729), "$223.33");
  assert.equal(formatCostMicros(1_500), "$0.0015");
});

test("durations read as ms then seconds", () => {
  assert.equal(formatMs(0), "0ms");
  assert.equal(formatMs(240), "240ms");
  assert.equal(formatMs(3972), "3.97s");
});

test("percent formats a fraction", () => {
  assert.equal(formatPercent(0), "0%");
  assert.equal(formatPercent(0.58), "58%");
  assert.equal(formatPercent(1), "100%");
});

test("stack accumulates series in key order", () => {
  const points = [
    { x: 1, key: "a", value: 2 }, { x: 1, key: "b", value: 3 },
    { x: 2, key: "a", value: 1 }, { x: 2, key: "b", value: 1 },
  ];
  const series = stack(points, ["a", "b"]);
  assert.equal(series.length, 2);
  assert.deepEqual(series[0].values.map(v => [v.y0, v.y1]), [[0, 2], [0, 1]]);
  assert.deepEqual(series[1].values.map(v => [v.y0, v.y1]), [[2, 5], [1, 2]]);
});

// A key missing from one bucket must contribute zero, not a hole that shifts
// every series above it.
test("stack fills gaps with zero", () => {
  const series = stack([{ x: 1, key: "a", value: 2 }, { x: 2, key: "b", value: 4 }], ["a", "b"]);
  assert.deepEqual(series[0].values.map(v => v.y1), [2, 0]);
  assert.deepEqual(series[1].values.map(v => [v.y0, v.y1]), [[2, 2], [0, 4]]);
});

// An account must keep its colour across screens and reloads, or the legend on
// Usage disagrees with the cards on Overview.
test("colorFor is stable and deterministic", () => {
  assert.equal(colorFor("acct-1"), colorFor("acct-1"));
  assert.notEqual(colorFor("acct-1"), colorFor("acct-2"));
  assert.match(colorFor("acct-1"), /^(#|hsl|var|oklch)/);
});

// Hashing alone put two of three models on the same orange in the preview:
// series on one screen must not share a colour while slots remain.
test("colorFor gives distinct keys distinct slots until the palette runs out", () => {
  // Slots are page-wide: acct-1 and acct-2 were coloured by the test above,
  // so they plus six new keys are the first eight this page has seen.
  const keys = ["acct-1", "acct-2", ...Array.from({ length: 6 }, (_, i) => `distinct-key-${i}`)];
  const colors = new Set(keys.map(colorFor));
  assert.equal(colors.size, 8);
});
