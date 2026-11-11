import { api, el, registerScreen, subscribeEvents, recentEvents, onConnState } from "../app.js";
import { formatMs, formatTokens } from "../chart.js";

const ROW_CAP = 2000; // the shell's RECENT_CAP: a week-old tab must not grow without bound

const clock = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

function severity(ev) {
  if (ev.status >= 500 || ev.outcome === "error") return "bad";
  if (ev.status === 429 || (ev.status >= 400 && ev.status < 500)) return "warn";
  return "ok";
}

registerScreen("activity", {
  title: "Activity",
  async mount(root) {
    const labels = new Map();
    api("/accounts").then((as) => { for (const a of as) labels.set(a.id, a.label); redraw(); }).catch(() => {});

    let paused = false, pausedAt = 0;
    const events = recentEvents().slice(-ROW_CAP);
    const filters = { account: "", model: "", outcome: "" };

    const selects = {};
    const mkSelect = (key, title) => {
      const s = el("select", { "aria-label": "filter by " + title, onchange: () => { filters[key] = s.value; redraw(); } });
      selects[key] = s;
      return el("label", { class: "filter" }, title, s);
    };
    const pauseBtn = el("button", { onclick: () => { paused = !paused; pausedAt = events.length; redraw(); } }, "Pause");
    const pauseNote = el("span", { class: "muted small" });
    const banner = el("div", { class: "banner", hidden: true }, "Live feed disconnected — reconnecting…");
    const tbody = el("tbody");
    const table = el("table", { class: "feed" },
      el("thead", {}, el("tr", {}, ["time", "model", "account", "status", "outcome", "duration", "ttfb", "in", "out"].map((h) => el("th", {}, h)))),
      tbody);

    root.replaceChildren(el("h2", {}, "Activity"), banner,
      el("div", { class: "toolbar" }, pauseBtn, pauseNote, mkSelect("account", "account"), mkSelect("model", "model"), mkSelect("outcome", "outcome")),
      table);

    const label = (id) => labels.get(id) || id;
    const fill = (key, valueOf, show = (v) => v) => {
      const s = selects[key], cur = s.value;
      const seen = [...new Set(events.map(valueOf).filter(Boolean))].sort();
      s.replaceChildren(el("option", { value: "" }, "all"), ...seen.map((v) => el("option", { value: v }, show(v))));
      s.value = seen.includes(cur) ? cur : "";
      filters[key] = s.value;
    };

    function row(ev) {
      const model = ev.requestedModel ? `${ev.requestedModel} → ${ev.model}` : ev.model;
      const acct = el("td", {}, label(ev.account));
      if (ev.spilled) {
        acct.appendChild(el("span", { class: "spill", title: `served outside group "${ev.group}"` }, " ↪"));
      }
      return el("tr", { class: "sev-" + severity(ev) },
        el("td", { class: "num muted" }, clock.format(new Date(ev.time))),
        el("td", {}, model), acct,
        el("td", { class: "num" }, String(ev.status)),
        el("td", {}, ev.outcome), // the text is always shown: never colour alone
        el("td", { class: "num" }, formatMs(ev.durationMs)),
        el("td", { class: "num" }, ev.ttfbMs < 0 ? "—" : formatMs(ev.ttfbMs)),
        el("td", { class: "num" }, formatTokens(ev.inputTokens || 0)),
        el("td", { class: "num" }, formatTokens(ev.outputTokens || 0)));
    }

    function redraw() {
      fill("account", (e) => e.account, label);
      fill("model", (e) => e.model);
      fill("outcome", (e) => e.outcome);
      pauseBtn.textContent = paused ? "Resume" : "Pause";
      const shown = paused ? events.slice(0, pausedAt) : events;
      pauseNote.textContent = paused && events.length > pausedAt ? `${events.length - pausedAt} new while paused` : "";
      const rows = shown.filter((e) =>
        (!filters.account || e.account === filters.account) &&
        (!filters.model || e.model === filters.model) &&
        (!filters.outcome || e.outcome === filters.outcome)).slice(-200).reverse();
      tbody.replaceChildren(...(rows.length ? rows.map(row)
        : [el("tr", {}, el("td", { colspan: 9, class: "muted empty-row" }, events.length ? "Nothing matches these filters." : "Waiting for the first request…"))]));
    }

    this.unsub = subscribeEvents((ev) => {
      events.push(ev);
      if (events.length > ROW_CAP) { events.splice(0, events.length - ROW_CAP); pausedAt = Math.max(0, pausedAt - 1); }
      redraw();
    });
    // "No rows" and "disconnected" look identical without this banner, and
    // only one of them means the proxy is idle.
    this.unconn = onConnState((s) => { banner.hidden = s !== "reconnecting"; });
    redraw();
  },
  unmount() { this.unsub?.(); this.unconn?.(); },
});
