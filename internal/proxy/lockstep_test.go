package proxy

import (
	"net/http"
	"reflect"
	"testing"

	chi "github.com/go-chi/chi/v5"
	"github.com/nicko170/aiproxy/internal/testutil"
	"github.com/nicko170/aiproxy/internal/view"
)

// routeFor names the one route a view.Source method is served by. Spec §9:
// "Whenever a method is added to view.Source, a route is added here — the two
// are kept in lockstep deliberately, and a test asserts every interface
// method has a corresponding route." Adding a Source method without adding an
// entry here (or to unroutedAllowlist below, with a reason) fails this test;
// so does an entry here whose route was never actually registered on the
// router, since that part is checked against chi's real route tree rather
// than assumed.
var routeFor = map[string]struct{ method, pattern string }{
	"ServerStatus":        {http.MethodGet, ReservedPrefix + "/api/v1/status"},
	"Accounts":            {http.MethodGet, ReservedPrefix + "/api/v1/accounts"},
	"UsageSeries":         {http.MethodGet, ReservedPrefix + "/api/v1/usage"},
	"Totals":              {http.MethodGet, ReservedPrefix + "/api/v1/totals"},
	"LatencyPercentiles":  {http.MethodGet, ReservedPrefix + "/api/v1/latency"},
	"AccountQuotaHistory": {http.MethodGet, ReservedPrefix + "/api/v1/quota/history"},
	"Subscribe":           {http.MethodGet, ReservedPrefix + "/api/v1/events"},
	"SetAccountEnabled":   {http.MethodPost, ReservedPrefix + "/api/v1/accounts/{id}/enabled"},
	"SetPriority":         {http.MethodPost, ReservedPrefix + "/api/v1/accounts/{id}/priority"},
	"RemoveAccount":       {http.MethodDelete, ReservedPrefix + "/api/v1/accounts/{id}"},
	"UpdateSettings":      {http.MethodPost, ReservedPrefix + "/api/v1/settings"},
}

// unroutedAllowlist names Source methods deliberately reachable through no
// route at all, with the reason recorded so the omission reads as a decision.
// Empty today: Login, ImportCredentials, and ProbeNow are deferred by being
// left off the Source interface itself (see source.go's doc comment), not by
// being routeless members of it, so there is nothing to list here yet. A
// future stage adding one of those methods to Source must add either a route
// above or an entry here.
var unroutedAllowlist = map[string]string{}

func TestEveryViewSourceMethodHasAControlRoute(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	registered := map[string]bool{} // "METHOD pattern"

	// Walk the harness's own live router — the exact handler serving the test
	// server above — rather than building a second one, so what is inspected
	// is what is actually running.
	mux, ok := h.srv.Config.Handler.(chi.Routes)
	if !ok {
		t.Fatal("router does not implement chi.Routes; cannot walk it")
	}
	if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	srcType := reflect.TypeOf((*view.Source)(nil)).Elem()
	for i := 0; i < srcType.NumMethod(); i++ {
		name := srcType.Method(i).Name
		if reason, skip := unroutedAllowlist[name]; skip {
			t.Logf("%s deliberately unrouted: %s", name, reason)
			continue
		}
		rt, ok := routeFor[name]
		if !ok {
			t.Errorf("view.Source.%s has no route mapping in this test and is not in "+
				"unroutedAllowlist; add a route to internal/proxy/router.go and an entry "+
				"here, or explain the omission in the allowlist", name)
			continue
		}
		if !registered[rt.method+" "+rt.pattern] {
			t.Errorf("view.Source.%s expects %s %s to be registered, but it is not",
				name, rt.method, rt.pattern)
		}
	}
}
