package web

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// CookieName is the browser's dashboard session cookie. It must equal
// proxy.sessionCookie — the two packages cannot share a constant without a
// dependency neither wants, so a test in internal/proxy asserts they agree.
const CookieName = "aiproxy_session"

// SignInHandler exchanges the proxy API key for a session cookie, and revokes
// it on DELETE.
//
// This endpoint exists because a browser cannot attach an x-api-key header to
// a top-level navigation. The key crosses the wire exactly once, in a POST
// body — never in a URL, where it would land in access logs and Referer
// headers — and the browser holds an opaque token afterwards.
func SignInHandler(sessions *Sessions, apiKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			if c, err := r.Cookie(CookieName); err == nil {
				sessions.Delete(c.Value)
			}
			http.SetCookie(w, expireCookie(r))
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Use POST to sign in and DELETE to sign out.")
			return
		}

		// An unconfigured key must never authorize. Without this branch the
		// constant-time compare below would succeed on two empty strings and
		// hand a session to anyone who posted an empty body.
		if apiKey == "" {
			writeJSONError(w, http.StatusForbidden, "authentication_error",
				"No proxy API key is configured, so the dashboard cannot be signed in to. Set listen.apiKey in config.json.")
			return
		}

		var body struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Could not read the sign-in request.")
			return
		}

		if subtle.ConstantTimeCompare([]byte(body.APIKey), []byte(apiKey)) != 1 {
			// Deliberately says nothing about what was submitted: echoing the
			// attempt writes it into the browser console and any log that
			// captures response bodies.
			writeJSONError(w, http.StatusUnauthorized, "authentication_error", "Incorrect API key.")
			return
		}

		tok, err := sessions.Mint()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "Could not create a session.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: CookieName, Value: tok, Path: "/_aiproxy",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
			Secure: r.TLS != nil, MaxAge: int(sessions.ttl / time.Second),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func expireCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: "", Path: "/_aiproxy",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: -1,
	}
}

// writeJSONError mirrors the envelope internal/proxy uses, so the dashboard
// parses one error shape from every endpoint it talks to.
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
