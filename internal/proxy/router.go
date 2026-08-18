package proxy

import (
	"net/http"
	"strings"

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
		// Spec §9: every method on view.Source has exactly one route here,
		// enforced by TestEveryViewSourceMethodHasAControlRoute. Each handler is
		// a thin adapter over o.View and does no aggregation of its own.
		cp.Get("/api/v1/status", statusHandler(o))
		cp.Get("/api/v1/accounts", accountsHandler(o))
		cp.Get("/api/v1/usage", usageHandler(o))
		cp.Get("/api/v1/totals", totalsHandler(o))
		cp.Get("/api/v1/latency", latencyHandler(o))
		cp.Get("/api/v1/quota/history", quotaHistoryHandler(o))
		cp.Get("/api/v1/events", eventsHandler(o))
		cp.Post("/api/v1/accounts/{id}/enabled", setAccountEnabledHandler(o))
		cp.Post("/api/v1/accounts/{id}/priority", setAccountPriorityHandler(o))
		cp.Delete("/api/v1/accounts/{id}", removeAccountHandler(o))
		cp.Post("/api/v1/settings", updateSettingsHandler(o))

		// Anything else under the reserved prefix is a 404, never a proxied
		// request: a future control route must not be answerable by the upstream.
		cp.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_found_error", "No such aiproxy endpoint")
		})
		// MethodNotAllowed must be set HERE as well, not only on the parent. chi
		// propagates the parent's handler into an already-mounted subrouter that
		// has none of its own, so with only the parent's set — which is the proxy
		// catch-all — a wrong method on a real control path (POST
		// /_aiproxy/api/v1/status) is forwarded upstream with an account
		// credential injected and answered by the provider.
		cp.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error",
				"Method not allowed on this aiproxy endpoint")
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
