package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/view"
)

// ReservedPrefix is the control-plane namespace. Nothing under it is ever
// proxied, so a control route can never be shadowed by an upstream path and a
// future endpoint can never be silently forwarded to the provider.
const ReservedPrefix = "/_aiproxy"

// sessionHeader is how a client tags requests belonging to one conversation.
const sessionHeader = "x-claude-code-session-id"

// maxBodyBytes bounds a buffered request body. The body must be buffered so an
// attempt can be replayed on another account; the cap keeps a hostile or
// runaway client from exhausting memory.
const maxBodyBytes = 64 << 20 // 64 MiB

// IsLoopback reports whether an address is on the local machine.
func IsLoopback(addr string) bool {
	if addr == "" {
		return false
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Authorized gates access to the proxy. Loopback is exempt: the key exists to
// stop other machines on the network, not the operator's own tools. The compare
// is constant time so a wrong key leaks nothing about the right one.
func Authorized(remoteAddr, presentedKey, configuredKey string) bool {
	if configuredKey == "" {
		return true
	}
	if IsLoopback(remoteAddr) {
		return true
	}
	a, b := []byte(presentedKey), []byte(configuredKey)
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

// ParseModel reads the top-level "model" field. Only the top level is decoded,
// so a megabyte of nested message content is never walked, and a "model" string
// appearing inside message text cannot be mistaken for the request's model.
func ParseModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}
	raw, ok := fields["model"]
	if !ok {
		return ""
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return ""
	}
	return model
}

// ModelMatches reports whether a blocklist pattern matches a model name.
// Patterns use shell globbing, e.g. "*fable*".
func ModelMatches(pattern, model string) bool {
	ok, err := path.Match(pattern, model)
	return err == nil && ok
}

// HandlerOptions configures the proxy handler.
type HandlerOptions struct {
	Attempter     *Attempter
	Manager       *account.Manager
	APIKey        string
	BlockedModels []string
	Log           *slog.Logger
	// OnResult receives every completed attempt. Stage 2 wires metrics here.
	OnResult func(Request, Result)
	// Upstream is the base URL for passthrough paths (no account selection).
	Upstream string
	// PassthroughPrefixes are relayed with the client's own credential.
	PassthroughPrefixes []string
	// Dropped reports how many accounting samples have been discarded because
	// the ingester's buffer was full, so degradation is visible rather than
	// silent (spec §7.3). Nil is treated as always zero.
	Dropped func() int64
	// View backs the control API (spec §9): every handler under
	// ReservedPrefix's api/v1 tree is a thin adapter over View and contains no
	// aggregation logic of its own. A nil View is only safe when no test or
	// caller exercises a control route.
	View view.Source
}

// proxyHandler buffers the request and hands it to the attempt loop.
//
// OnResult is wired onto the Attempter itself, once, here — NOT called after
// Do returns below. Do panics with http.ErrAbortHandler on a truncated relay,
// which unwinds straight past a call site here; the only place that survives
// that unwind is Do's own deferred block; see the defer in Do and the comment
// on Attempter.OnResult.
func proxyHandler(o HandlerOptions) http.HandlerFunc {
	if o.Attempter != nil {
		o.Attempter.OnResult = o.OnResult
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// The reserved namespace is refused HERE, at the point of harm, rather
		// than only in the router. chi matches on r.URL.RawPath when it is set, so
		// any spelling that survives escaping unchanged — a percent-encoded
		// character in the prefix (/%5Faiproxy/...), a ..%2f traversal that
		// normalizes into it — misses the mounted subrouter, falls through to the
		// catch-all, and is forwarded to the provider with an ACCOUNT CREDENTIAL
		// attached. This is the second such door found; guarding the one place
		// that can do the damage means there is no third.
		if strings.HasPrefix(path.Clean(r.URL.Path), ReservedPrefix) {
			writeError(w, http.StatusNotFound, "not_found_error", "No such aiproxy endpoint")
			return
		}

		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "Could not read request body")
			return
		}

		req := Request{
			Method:    r.Method,
			Path:      r.URL.RequestURI(),
			Header:    r.Header.Clone(),
			Body:      body,
			Model:     ParseModel(body),
			SessionID: r.Header.Get(sessionHeader),
		}

		if req.Model != "" {
			for _, pattern := range o.BlockedModels {
				if !ModelMatches(pattern, req.Model) {
					continue
				}
				o.Log.Info("refused blocked model", "model", req.Model, "pattern", pattern)
				writeError(w, http.StatusBadRequest, "invalid_request_error",
					"Model \""+req.Model+"\" is blocked by aiproxy (matched \""+pattern+"\").")
				if o.OnResult != nil {
					// StartedAt must be stamped, TTFBMS must be -1, and the outcome
					// must say "blocked". Left at their zero values this row landed
					// at started_at=0: invisible to every window query, aggregated
					// into bucket 0, deleted by the first prune, and contributing a
					// spurious 0 ms to the TTFB percentile that spec §2.1 exists to
					// make observable — while reporting a refusal as "ok".
					// -1 is the package's established "no first byte was produced"
					// sentinel, which the percentile queries already exclude.
					o.OnResult(req, Result{
						Status:    http.StatusBadRequest,
						Outcome:   provider.OutcomeBlocked,
						StartedAt: time.Now().UnixMilli(),
						TTFBMS:    -1,
					})
				}
				return
			}
		}

		// The result is reported to o.OnResult from inside Do itself (see above),
		// including on the panic path, so nothing further happens with the return
		// value here.
		o.Attempter.Do(r.Context(), w, req)
	}
}

// DefaultPassthroughPrefixes are upstream paths bound to the CLIENT's own paired
// identity rather than to a rotated account. Injecting one of our credentials
// here gets refused upstream and the client quietly loses the feature, so these
// relay transparently: the client's headers survive, no account is selected, and
// the body is streamed rather than buffered (some are long-poll channels that
// withhold response headers for minutes).
var DefaultPassthroughPrefixes = []string{
	"/v1/code/",
	"/api/oauth/files/",
	"/api/oauth/file_upload",
	"/v1/oauth/token",
}

func passthroughHandler(o HandlerOptions) http.HandlerFunc {
	upstream := strings.TrimSuffix(o.Upstream, "/")
	client := &http.Client{
		// Client.Timeout is left unset, which really does mean "no timeout" for
		// the overall request/response. But a zero TransportOptions is NOT the
		// same thing: NewTransport applies its own 120s ResponseHeaderTimeout
		// default, which would sever exactly the long-poll channels this comment
		// used to claim were unbounded (§4.6 — the client keeps the request open
		// indefinitely and upstream may withhold response headers for minutes).
		// DisableResponseHeaderTimeout turns that default off instead of merely
		// raising it, so this path is genuinely unbounded end to end.
		Transport: NewTransport(TransportOptions{DisableResponseHeaderTimeout: true}),
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		out, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), r.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "proxy_error", "Could not build upstream request")
			return
		}
		for k, vs := range r.Header {
			lk := strings.ToLower(k)
			// Authorization is deliberately NOT stripped: carrying the client's
			// own credential is the entire purpose of this path.
			//
			// x-api-key IS stripped, exactly as on the account path. It
			// authenticates the client to US, and a user who follows the obvious
			// setup — ANTHROPIC_API_KEY set to their aiproxy key — would otherwise
			// ship that key to api.anthropic.com on every passthrough request.
			if hopByHop[lk] || lk == "accept-encoding" || lk == "x-api-key" {
				continue
			}
			for _, v := range vs {
				out.Header.Add(k, v)
			}
		}
		out.ContentLength = r.ContentLength

		res, err := client.Do(out)
		if err != nil {
			o.Log.Warn("passthrough failed", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusBadGateway, "proxy_error", "Upstream unreachable")
			return
		}
		defer res.Body.Close()

		for k, vs := range res.Header {
			if connectionSpecific[strings.ToLower(k)] {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		n, relayErr := Relay(r.Context(), w, res.Body, RelayOptions{BodyIdle: 0}) // 0 takes the default
		if relayErr != nil && !errors.Is(relayErr, context.Canceled) {
			o.Log.Warn("passthrough relay ended early", "path", r.URL.Path, "err", relayErr, "bytes", n)
			// Same treatment as the account path: returning normally would let
			// net/http finish the chunked body cleanly, and a client cannot tell
			// a cleanly-finished truncated stream from a complete one.
			panic(http.ErrAbortHandler)
		}
	}
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
