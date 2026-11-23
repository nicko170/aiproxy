import { api, el } from "../app.js";

// startLogin opens the add-account flow. The OAuth callback listener binds to
// the aiproxy HOST, so on a server it can never reach the operator's browser:
// this flow always uses the paste-the-code path, which exists for exactly
// that. onDone runs after a successful login so the caller can re-fetch.
export function startLogin(onDone) {
  let timer = null, sessionId = null;
  // gen invalidates in-flight begin/poll work when the operator restarts the
  // flow (the long-lived toggle) or backs out. Without it, a stale response
  // could render a session the dialog no longer owns.
  let gen = 0;
  const stop = () => { if (timer) { clearTimeout(timer); timer = null; } };
  const cancelSession = () => {
    if (!sessionId) return;
    const id = sessionId;
    sessionId = null;
    api(`/accounts/login/${encodeURIComponent(id)}`, { method: "DELETE" }).catch(() => {});
  };
  const body = el("div", { class: "login-body" });
  const d = el("dialog", { class: "login" }, el("h3", {}, "Add an account"), body,
    el("div", { class: "controls" }, el("button", { onclick: () => d.close() }, "Close")));
  // Closing must stop polling and free the pending login's listener: a poll
  // left running against an abandoned session leaks a timer per attempt.
  d.addEventListener("close", () => { gen++; stop(); cancelSession(); d.remove(); });
  document.body.appendChild(d);
  d.showModal();

  const fail = (msg) => {
    gen++;
    stop();
    cancelSession();
    body.replaceChildren(el("p", { class: "error-text" }, msg),
      el("button", { onclick: pickProvider }, "Try again"));
  };

  function pickProvider() {
    gen++;
    stop();
    cancelSession();
    body.replaceChildren(el("p", {}, "Which subscription?"),
      el("div", { class: "controls" },
        el("button", { onclick: () => begin("anthropic", false) }, "Claude (Anthropic)"),
        el("button", { onclick: () => begin("openai", false) }, "ChatGPT (OpenAI)")));
  }

  async function begin(provider, longLived) {
    const my = ++gen;
    stop();
    cancelSession();
    body.replaceChildren(el("p", { class: "muted" }, "Starting…"));
    let r;
    try {
      r = await api("/accounts/login", { method: "POST", body: JSON.stringify({ provider, longLived }) });
    } catch (e) { if (my === gen) fail(e.message); return; }
    if (my !== gen) {
      // Superseded while the request was in flight: release the flow we
      // started rather than leaking its listener.
      api(`/accounts/login/${encodeURIComponent(r.sessionId)}`, { method: "DELETE" }).catch(() => {});
      return;
    }
    sessionId = r.sessionId;
    const code = el("input", { type: "text", placeholder: "paste the code here", autocomplete: "off", "aria-label": "authorization code", class: "wide" });
    const err = el("p", { class: "error-text" });
    const submit = async (e) => {
      e.preventDefault();
      err.textContent = "";
      try {
        // code#state and a bare code are both accepted server-side: pass it
        // through unchanged rather than splitting it here.
        await api(`/accounts/login/${encodeURIComponent(sessionId)}/code`, { method: "POST", body: JSON.stringify({ code: code.value.trim() }) });
        body.replaceChildren(el("p", { class: "muted" }, "Exchanging the code…"));
      } catch (ex) { err.textContent = ex.message; }
    };
    // Only Claude has a long-lived grant. Toggling it restarts the flow,
    // because the scope is part of the authorize URL already displayed.
    let toggle = null;
    if (provider === "anthropic") {
      toggle = el("input", { type: "checkbox", checked: longLived });
      toggle.addEventListener("change", () => begin(provider, toggle.checked));
    }
    body.replaceChildren(
      el("p", {}, "1. Open this link, sign in, and approve:"),
      el("p", {}, el("a", { href: r.url, target: "_blank", rel: "noopener" }, "Open the sign-in page")),
      // On a remote deploy the operator may need to move the URL by hand.
      el("input", { type: "text", readonly: true, value: r.url, class: "wide mono", "aria-label": "sign-in URL", onfocus: (e) => e.target.select() }),
      toggle ? el("label", { class: "small muted" }, toggle, " Long-lived (1 year) — re-authorise when it expires") : null,
      el("p", { class: "small muted" }, "If this browser is on the aiproxy machine, signing in finishes on its own."),
      el("form", { onsubmit: submit },
        el("p", {}, "2. Otherwise paste the code the page shows you — or, if your browser ended on a page that would not load, the full address from its address bar:"),
        code, err,
        el("div", { class: "controls" }, el("button", { type: "submit" }, "Finish"))));
    code.focus();
    // Poll from the start, not only after a paste: when the browser is on the
    // aiproxy host the provider's redirect reaches the local callback and
    // finishes the login with no code to paste at all.
    poll(my);
  }

  async function poll(my) {
    if (my !== gen) return;
    let st;
    try {
      st = await api(`/accounts/login/${encodeURIComponent(sessionId)}`);
    } catch (e) { if (my === gen) fail(e.message); return; }
    if (my !== gen) return;
    if (st.status === "done") {
      // The session is terminal; there is nothing left to cancel on close.
      sessionId = null;
      stop();
      const who = st.profile?.email || st.profile?.displayName || "the account";
      body.replaceChildren(el("p", {}, `Added ${who}.`));
      onDone?.();
      return;
    }
    // LoginResult.Err says which failure it was — state mismatch, timeout,
    // rejected exchange, failed save — and that is the difference between
    // "try again" and "your clock is wrong". Show it verbatim.
    if (st.status === "error") { sessionId = null; fail(st.error || "Login failed."); return; }
    timer = setTimeout(() => poll(my), 1000);
  }

  pickProvider();
}
