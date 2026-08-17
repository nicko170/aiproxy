package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter assembles the listener's routes: the reserved control-plane prefix
// first, then a catch-all that proxies everything else upstream.
func NewRouter(o HandlerOptions) http.Handler {
	r := chi.NewRouter()
	// Recoverer keeps one panicking request from taking the process down with
	// every other in-flight session. It re-panics http.ErrAbortHandler rather
	// than converting it to a 500, which is what lets the relay sever a
	// truncated stream instead of finishing it cleanly.
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)

	r.Route(ReservedPrefix, func(cp chi.Router) {
		cp.Get("/api/v1/status", statusHandler(o))
		// Anything else under the reserved prefix is a 404, never a proxied
		// request: a future control route must not be answerable by the upstream.
		cp.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found_error", "No such aiproxy endpoint")
		})
	})

	if o.Upstream != "" {
		pt := passthroughHandler(o)
		for _, prefix := range o.PassthroughPrefixes {
			// Register both the bare path and its subtree: a prefix like
			// "/api/oauth/file_upload" is itself a valid endpoint, while
			// "/v1/code/" only ever appears with more path after it.
			r.Handle(strings.TrimSuffix(prefix, "/"), pt)
			r.Handle(strings.TrimSuffix(prefix, "/")+"/*", pt)
		}
	}

	r.NotFound(proxyHandler(o))
	r.MethodNotAllowed(proxyHandler(o))
	return r
}

type statusAccount struct {
	ID               string             `json:"id"`
	Label            string             `json:"label"`
	Provider         string             `json:"provider"`
	Priority         int                `json:"priority"`
	Disabled         bool               `json:"disabled"`
	Status           string             `json:"status"`
	LastError        string             `json:"lastError,omitempty"`
	InFlight         int                `json:"inFlight"`
	RateLimitedUntil int64              `json:"rateLimitedUntil,omitempty"`
	PausedUntil      int64              `json:"pausedUntil,omitempty"`
	Buckets          map[string]float64 `json:"buckets"`
}

// statusHandler is the minimal readout for stage 1. Stage 3 replaces it with the
// full control API backed by view.Source.
func statusHandler(o HandlerOptions) http.HandlerFunc {
	started := time.Now()
	return func(w http.ResponseWriter, r *http.Request) {
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}

		accounts := []statusAccount{}
		for _, a := range o.Manager.Snapshot() {
			buckets := map[string]float64{}
			for name, b := range a.Buckets {
				buckets[name] = b.Utilization
			}
			accounts = append(accounts, statusAccount{
				ID: a.ID, Label: a.Label, Provider: a.Provider,
				Priority: a.Priority, Disabled: a.Disabled,
				Status: a.Status.String(), LastError: a.LastError,
				InFlight: a.InFlight, RateLimitedUntil: a.RateLimitedUntil,
				PausedUntil: a.PausedUntil, Buckets: buckets,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"uptimeSeconds": int(time.Since(started).Seconds()),
			"accounts":      accounts,
		})
	}
}
