import { api, el, registerScreen, startPolling } from "../app.js";
import { gauge, colorFor, formatMs, formatPercent } from "../chart.js";

// humanDuration renders seconds as "4h12m" / "3d2h" / "45s".
export function humanDuration(sec) {
  sec = Math.max(0, Math.floor(sec));
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d}d${h}h`;
  if (h) return `${h}h${m}m`;
  if (m) return `${m}m`;
  return `${sec}s`;
}

// countdown renders a future unix-ms instant as "in 4m". The limits it is used
// for are omitempty on the wire: absent or zero means "not limited", and the
// caller must not render anything — "limited until 1970" is the bug.
export function countdown(untilMs) {
  return "in " + humanDuration((untilMs - Date.now()) / 1000);
}

const BUCKET_NAMES = { five_hour: "5h", seven_day: "7d" };
function bucketLabel(name) {
  return BUCKET_NAMES[name] || name.replace(/^seven_day_/, "7d ").replace(/_/g, " ");
}

// renderAccountCard is the one account card, shared with the Accounts screen
// (which adds controls through `extra`). status supplies the pin state.
export function renderAccountCard(a, status, extra = null) {
  const now = Date.now();
  const limits = [];
  if (a.rateLimitedUntil && a.rateLimitedUntil > now) limits.push(el("span", { class: "pill warn" }, "rate-limited " + countdown(a.rateLimitedUntil)));
  if (a.pausedUntil && a.pausedUntil > now) limits.push(el("span", { class: "pill warn" }, "paused " + countdown(a.pausedUntil)));
  if (a.disabled) limits.push(el("span", { class: "pill" }, "disabled"));
  if (a.status === "errored") limits.push(el("span", { class: "pill bad", title: a.lastError || "" }, "errored"));
  const pinned = status?.pinnedAccountId === a.id;
  if (pinned) limits.push(el("span", { class: "pill accent" }, "forced"));

  const buckets = Object.entries(a.buckets || {}).sort(([x], [y]) => x.localeCompare(y));
  return el("article", { class: "card account" + (a.disabled ? " disabled" : ""), "data-id": a.id },
    el("header", {},
      el("span", { class: "swatch", style: `background:${colorFor(a.id)}` }),
      el("h3", {}, a.label),
      el("span", { class: "muted" }, a.provider)),
    el("dl", { class: "facts" },
      el("dt", {}, "priority"), el("dd", {}, "p" + a.priority),
      el("dt", {}, "group"), el("dd", {}, a.group || "—"),
      el("dt", {}, "in flight"), el("dd", {}, String(a.inFlight ?? 0)),
      el("dt", {}, "status"), el("dd", {}, a.status)),
    limits.length ? el("div", { class: "pills" }, limits) : null,
    buckets.length
      ? el("div", { class: "gauges" }, buckets.map(([name, f]) => gauge({ fraction: f, label: bucketLabel(name) })))
      : el("p", { class: "muted small" }, "no quota read yet"),
    extra);
}

function header(status) {
  const items = [
    ["listening", status.listenAddr],
    ["up", humanDuration(status.uptimeSeconds)],
    ["in flight", String(status.inFlight)],
    ["p95 TTFB", status.ttfbP95Ms > 0 ? formatMs(status.ttfbP95Ms) : "—"],
  ];
  // Drops are shown only when non-zero: a silent sample drop is exactly what
  // the operator needs to see, and a permanent "0 dropped" is noise.
  if (status.metricsDropped) items.push(["metrics dropped", String(status.metricsDropped)]);
  if (status.eventsDropped) items.push(["events dropped", String(status.eventsDropped)]);
  const strip = el("div", { class: "strip" }, items.map(([k, v]) => el("div", { class: "stat" }, el("span", { class: "stat-label" }, k), el("span", { class: "stat-value" }, v))));
  if (status.update?.available) {
    strip.appendChild(el("a", { class: "pill accent", href: status.update.releaseUrl || "#", target: "_blank", rel: "noopener" },
      `update available: ${status.update.latestVersion}`));
  }
  return strip;
}

function skeleton() {
  return el("div", { class: "cards" }, [0, 1].map(() => el("article", { class: "card skeleton" }, el("div", { class: "bar" }), el("div", { class: "bar short" }))));
}

registerScreen("overview", {
  title: "Overview",
  async mount(root) {
    root.replaceChildren(el("h2", {}, "Overview"), skeleton());
    let busy = null;

    const load = async () => {
      let status, accounts;
      try {
        [status, accounts] = await Promise.all([api("/status"), api("/accounts")]);
      } catch (e) {
        if (e.status === 401) throw e;
        root.replaceChildren(el("h2", {}, "Overview"), el("div", { class: "error" },
          el("p", {}, "Could not load the overview."), el("code", {}, e.message),
          el("button", { onclick: load }, "Retry")));
        return;
      }
      const body = [el("h2", {}, "Overview"), header(status)];
      if (!accounts.length) {
        body.push(el("div", { class: "empty-state" },
          el("p", {}, "No accounts yet."),
          el("a", { href: "#/accounts", class: "button" }, "Add one on the Accounts screen")));
      } else {
        const pinControl = (a) => {
          const pinned = status.pinnedAccountId === a.id;
          return el("div", { class: "controls" }, el("button", {
            disabled: busy === a.id,
            title: pinned ? "return to normal routing" : "send all traffic through this account until it runs out",
            onclick: async () => {
              busy = a.id;
              try {
                await api("/accounts/pin", { method: "POST", body: JSON.stringify({ accountId: pinned ? "" : a.id }) });
              } catch (e) { alert(e.message); }
              busy = null;
              // Reflect the server's answer, not an optimistic guess.
              await load();
            },
          }, pinned ? "Unforce" : "Force all traffic here"));
        };
        body.push(el("div", { class: "cards" }, accounts.map((a) => renderAccountCard(a, status, pinControl(a)))));
      }
      root.replaceChildren(...body);
    };

    this.stop = startPolling(load, 5000);
  },
  unmount() { this.stop?.(); },
});
