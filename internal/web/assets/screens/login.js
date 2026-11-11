import { api, el } from "../app.js";

// startLogin opens the add-account flow. The OAuth callback listener binds to
// the aiproxy HOST, so on a server it can never reach the operator's browser:
// this flow always uses the paste-the-code path, which exists for exactly
// that. onDone runs after a successful login so the caller can re-fetch.
export function startLogin(onDone) {
  let timer = null, sessionId = null;
  const stop = () => { if (timer) { clearTimeout(timer); timer = null; } };
  const body = el("div", { class: "login-body" });
  const d = el("dialog", { class: "login" }, el("h3", {}, "Add an account"), body,
    el("div", { class: "controls" }, el("button", { onclick: () => d.close() }, "Close")));
  // Closing must stop polling: a poll left running against an abandoned
  // session leaks a timer per attempt.
  d.addEventListener("close", () => { stop(); d.remove(); });
  document.body.appendChild(d);
  d.showModal();

  const fail = (msg) => body.replaceChildren(el("p", { class: "error-text" }, msg),
    el("button", { onclick: pickProvider }, "Try again"));

  function pickProvider() {
    stop();
    body.replaceChildren(el("p", {}, "Which subscription?"),
      el("div", { class: "controls" },
        el("button", { onclick: () => begin("anthropic") }, "Claude (Anthropic)"),
        el("button", { onclick: () => begin("openai") }, "ChatGPT (OpenAI)")));
  }

  async function begin(provider) {
    body.replaceChildren(el("p", { class: "muted" }, "Starting…"));
    let r;
    try {
      r = await api("/accounts/login", { method: "POST", body: JSON.stringify({ provider }) });
    } catch (e) { fail(e.message); return; }
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
    body.replaceChildren(
      el("p", {}, "1. Open this link, sign in, and approve:"),
      el("p", {}, el("a", { href: r.url, target: "_blank", rel: "noopener" }, "Open the sign-in page")),
      // On a remote deploy the operator may need to move the URL by hand.
      el("input", { type: "text", readonly: true, value: r.url, class: "wide mono", "aria-label": "sign-in URL", onfocus: (e) => e.target.select() }),
      el("p", { class: "small muted" }, "If this browser is on the aiproxy machine, signing in finishes on its own."),
      el("form", { onsubmit: submit },
        el("p", {}, "2. Otherwise paste the code the page shows you — or, if your browser ended on a page that would not load, the full address from its address bar:"),
        code, err,
        el("div", { class: "controls" }, el("button", { type: "submit" }, "Finish"))));
    code.focus();
    // Poll from the start, not only after a paste: when the browser is on the
    // aiproxy host the provider's redirect reaches the local callback and
    // finishes the login with no code to paste at all.
    poll();
  }

  async function poll() {
    let st;
    try {
      st = await api(`/accounts/login/${encodeURIComponent(sessionId)}`);
    } catch (e) { fail(e.message); return; }
    if (st.status === "done") {
      const who = st.profile?.email || st.profile?.displayName || "the account";
      body.replaceChildren(el("p", {}, `Added ${who}.`));
      onDone?.();
      return;
    }
    // LoginResult.Err says which failure it was — state mismatch, timeout,
    // rejected exchange, failed save — and that is the difference between
    // "try again" and "your clock is wrong". Show it verbatim.
    if (st.status === "error") { fail(st.error || "Login failed."); return; }
    timer = setTimeout(poll, 1000);
  }

  pickProvider();
}
