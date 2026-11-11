package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
	"github.com/nicko170/aiproxy/internal/web"
)

// Drives the REAL router with the dashboard mounted, which is the seam nothing
// else covers: internal/web's own tests exercise its handler in isolation, and
// the router's tests do not know the dashboard exists.
func TestDashboardMountsAndSessionRoundTrips(t *testing.T) {
	sessions := web.NewSessions(time.Hour)
	srv := httptest.NewServer(NewRouter(HandlerOptions{
		Manager: nil, Log: quietLogger(), APIKey: "secret",
		Sessions: sessions, Web: web.Handler(sessions, "secret"),
	}))
	defer srv.Close()

	// The shell is served.
	res, err := http.Get(srv.URL + ReservedPrefix + "/")
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", res.StatusCode)
	}

	// Sign in, and the cookie authorizes a subsequent API call.
	signIn, err := http.Post(srv.URL+ReservedPrefix+"/api/v1/session",
		"application/json", strings.NewReader(`{"apiKey":"secret"}`))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer signIn.Body.Close()
	if signIn.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-in status = %d, want 204", signIn.StatusCode)
	}
	cookies := signIn.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+ReservedPrefix+"/api/v1/status", nil)
	req.AddCookie(cookies[0])
	api, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated status call: %v", err)
	}
	defer api.Body.Close()
	if api.StatusCode == http.StatusUnauthorized {
		t.Error("the session cookie did not authorize an API call")
	}
}

// An unknown path under the reserved prefix stays a 404. Mounting the
// dashboard must not relax the guarantee that nothing under this prefix
// reaches an upstream.
func TestUnknownReservedPathIsStill404WithTheDashboardMounted(t *testing.T) {
	sessions := web.NewSessions(time.Hour)
	srv := httptest.NewServer(NewRouter(HandlerOptions{
		Log: quietLogger(), Sessions: sessions, Web: web.Handler(sessions, ""),
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + ReservedPrefix + "/definitely/not/a/route")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// Routing rules and account groups configured through the dashboard's own
// path — a signed-in browser session against the real router — come back
// from the same seam the TUI reads.
func TestDashboardConfiguresRoutingThroughASession(t *testing.T) {
	sessions := web.NewSessions(time.Hour)
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.APIKey = "secret"
		o.Sessions = sessions
		o.Web = web.Handler(sessions, "secret")
	}, testutil.Script{Status: 200, Body: `{}`})

	signIn, err := http.Post(h.srv.URL+ReservedPrefix+"/api/v1/session", "application/json", strings.NewReader(`{"apiKey":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	signIn.Body.Close()
	cookie := signIn.Cookies()[0]

	call := func(method, path, body string) []byte {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+ReservedPrefix+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d: %s", method, path, res.StatusCode, b)
		}
		return b
	}

	var s map[string]any
	json.Unmarshal(call("GET", "/settings", ""), &s)
	s["routingRules"] = []map[string]any{{"match": "code/GT", "group": "gt", "strict": false}}
	s["defaultGroup"] = "personal"
	body, _ := json.Marshal(s)
	call("POST", "/settings", string(body))
	call("POST", "/accounts/acct-0/group", `{"group":"gt"}`)

	var got struct {
		RoutingRules []struct{ Match, Group string } `json:"routingRules"`
		DefaultGroup string                          `json:"defaultGroup"`
	}
	json.Unmarshal(call("GET", "/settings", ""), &got)
	if len(got.RoutingRules) != 1 || got.RoutingRules[0].Group != "gt" || got.DefaultGroup != "personal" {
		t.Errorf("settings came back as %+v", got)
	}
	var accts []struct{ ID, Group string }
	json.Unmarshal(call("GET", "/accounts", ""), &accts)
	if len(accts) != 1 || accts[0].Group != "gt" {
		t.Errorf("accounts came back as %+v", accts)
	}
}
