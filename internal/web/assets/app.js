// The dashboard shell: routing, fetch, live events, and the DOM helper every
// screen builds with.
//
// No framework and no build step (spec §3). The helper below is the whole
// abstraction: it exists so nothing in this app ever assigns innerHTML with a
// value that came from the API.

export { el } from "./dom.js";
import { el } from "./dom.js";

const BASE = "/_aiproxy/api/v1";

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// api fetches and unwraps the proxy's error envelope.
//
// The server's message is surfaced verbatim rather than replaced with
// something generic: it is frequently the only actionable thing available
// ("Model X is blocked by aiproxy", "no eligible account"), and a UI that
// swallows it makes every failure look identical.
export async function api(path, opts = {}) {
  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    headers: opts.body ? { "content-type": "application/json" } : undefined,
    ...opts,
  });
  if (res.status === 204) return null;
  let body = null;
  try { body = await res.json(); } catch { /* non-JSON error bodies exist */ }
  if (!res.ok) {
    const msg = body?.error?.message || `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  return body;
}

// startPolling runs fn on an interval, but ONLY while the tab is visible. A
// dashboard left open on a second monitor overnight should not spend the night
// querying SQLite on the other end of a tunnel.
export function startPolling(fn, intervalMs) {
  let timer = null;
  const tick = async () => { try { await fn(); } catch (e) { /* screens render their own errors */ } };
  const start = () => { if (timer === null) { tick(); timer = setInterval(tick, intervalMs); } };
  const stop = () => { if (timer !== null) { clearInterval(timer); timer = null; } };
  const onVisibility = () => (document.visibilityState === "visible" ? start() : stop());
  document.addEventListener("visibilitychange", onVisibility);
  onVisibility();
  return () => { stop(); document.removeEventListener("visibilitychange", onVisibility); };
}

// The SSE connection is owned HERE, not by the Activity screen, so it survives
// navigation: switching to Usage and back does not drop the feed or lose the
// events that arrived in between.
let source = null;
const eventHandlers = new Set();
const recent = [];
const RECENT_CAP = 2000;

export function subscribeEvents(handler) {
  eventHandlers.add(handler);
  ensureSource();
  return () => eventHandlers.delete(handler);
}

export function recentEvents() { return recent; }

function ensureSource() {
  if (source) return;
  let backoff = 1000;
  const connect = () => {
    source = new EventSource(BASE + "/events");
    source.onopen = () => { backoff = 1000; setConnState("live"); };
    source.onmessage = (m) => {
      let ev; try { ev = JSON.parse(m.data); } catch { return; }
      recent.push(ev);
      if (recent.length > RECENT_CAP) recent.splice(0, recent.length - RECENT_CAP);
      for (const h of eventHandlers) h(ev);
    };
    // A feed that has quietly stopped is worse than one that says it stopped:
    // the operator reads "no activity" as "nothing is happening".
    source.onerror = () => {
      setConnState("reconnecting");
      source.close(); source = null;
      setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 30000);
    };
  };
  connect();
}

function setConnState(state) {
  const dot = document.getElementById("conn");
  if (dot) { dot.className = "conn " + state; dot.title = state; }
}

const screens = new Map();
let active = null;

export function registerScreen(name, screen) { screens.set(name, screen); }
export function currentScreen() { return (location.hash.replace(/^#\/?/, "") || "overview").split("?")[0]; }
export function navigate(name) { location.hash = "#/" + name; }

async function render() {
  const name = currentScreen();
  const screen = screens.get(name) || screens.get("overview");
  if (active && active.unmount) active.unmount();
  const main = document.getElementById("app");
  main.replaceChildren();
  active = screen;
  for (const link of document.querySelectorAll("nav a")) {
    link.classList.toggle("active", link.dataset.screen === name);
  }
  try {
    await screen.mount(main);
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) { showSignIn(); return; }
    main.replaceChildren(el("div", { class: "error" },
      el("p", {}, "This screen failed to load."), el("code", {}, e.message),
      el("button", { onclick: () => render() }, "Retry")));
  }
}

window.addEventListener("hashchange", render);
export { render };

// showSignIn is wired via a dynamic import rather than a static one: signin.js
// imports api/el/render from this module, and a static import back into it
// from here would be a load-time cycle. A dynamic import is resolved at call
// time instead, once both modules have already finished evaluating.
async function showSignIn(message) {
  const { showSignIn: show } = await import("./screens/signin.js");
  show(message);
}

// Boot probes an authenticated endpoint. A 401 means sign in; anything else
// means the proxy is reachable and we can render.
export async function boot() {
  try {
    await api("/status");
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) { showSignIn(); return; }
  }
  await render();
}

document.getElementById("signout")?.addEventListener("click", async (e) => {
  e.preventDefault();
  try { await api("/session", { method: "DELETE" }); } catch { /* signing out best-effort */ }
  showSignIn();
});

boot();
