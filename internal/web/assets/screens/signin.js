import { api, el, boot } from "../app.js";

// Sign-in is not a registered screen: it replaces the whole shell, because
// there is nothing to navigate to until it succeeds.
export function showSignIn(message) {
  document.body.classList.add("signin");
  const input = el("input", { type: "password", id: "key", autocomplete: "current-password",
                              placeholder: "proxy API key" });
  const err = el("p", { class: "error-text" }, message || "");

  const submit = async (e) => {
    e.preventDefault();
    err.replaceChildren();
    try {
      await api("/session", { method: "POST", body: JSON.stringify({ apiKey: input.value }) });
      document.body.classList.remove("signin");
      await boot();
    } catch (ex) {
      err.replaceChildren(document.createTextNode(ex.message));
    }
  };

  document.getElementById("app").replaceChildren(
    el("form", { class: "signin-card", onsubmit: submit },
      el("h1", {}, "aiproxy"),
      el("label", { for: "key" }, "Proxy API key"),
      input, err,
      el("button", { type: "submit" }, "Sign in")));
  input.focus();
}
