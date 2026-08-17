package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"

	"github.com/nicko170/aiproxy/internal/account"
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
}

// proxyHandler buffers the request and hands it to the attempt loop.
func proxyHandler(o HandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
					o.OnResult(req, Result{Status: http.StatusBadRequest, TTFBMS: 0})
				}
				return
			}
		}

		res := o.Attempter.Do(r.Context(), w, req)
		if o.OnResult != nil {
			o.OnResult(req, res)
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
