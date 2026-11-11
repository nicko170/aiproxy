package proxy

import (
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/testutil"
)

func statusWith(t *testing.T, h *routerHarness, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+ReservedPrefix+"/api/v1/status", nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func keyedHarness(t *testing.T) *routerHarness {
	return newRouterHarness(t, func(o *HandlerOptions) { o.APIKey = "secret" }, testutil.Script{Status: 200, Body: `{}`})
}

// A TLS terminator or tunnel on the same host (Caddy, nginx, cloudflared,
// tailscale serve) connects from 127.0.0.1 on behalf of the INTERNET. Trusting
// its loopback address handed every caller the control plane and the
// accounts with no key at all.
func TestLoopbackBehindAReverseProxyNeedsTheKey(t *testing.T) {
	h := keyedHarness(t)
	for _, hdr := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "Cf-Connecting-Ip"} {
		if code := statusWith(t, h, map[string]string{hdr: "203.0.113.9"}); code != http.StatusUnauthorized {
			t.Errorf("loopback request carrying %s: status %d, want 401", hdr, code)
		}
		if code := statusWith(t, h, map[string]string{hdr: "203.0.113.9", "x-api-key": "secret"}); code != http.StatusOK {
			t.Errorf("forwarded request WITH the key: status %d, want 200", code)
		}
	}
}

// DNS rebinding: a page on evil.example re-resolves its own name to 127.0.0.1
// and reads the control API. The Host header still names the attacker's
// domain, which no local client ever sends.
func TestLoopbackWithAForeignHostNeedsTheKey(t *testing.T) {
	h := keyedHarness(t)
	if code := statusWith(t, h, map[string]string{"Host": "evil.example:3456"}); code != http.StatusUnauthorized {
		t.Errorf("foreign Host on loopback: status %d, want 401", code)
	}
	for _, host := range []string{"127.0.0.1:3456", "localhost:3456", "[::1]:3456", "0.0.0.0:3456"} {
		if code := statusWith(t, h, map[string]string{"Host": host}); code != http.StatusOK {
			t.Errorf("Host %s on loopback: status %d, want 200", host, code)
		}
	}
}

// Any website open in the operator's browser can POST to 127.0.0.1. The
// browser says where the request came from; a cross-site one gets no loopback
// trust. The dashboard itself is same-origin and keeps working.
func TestCrossSiteBrowserRequestNeedsTheKey(t *testing.T) {
	h := keyedHarness(t)
	cases := []struct {
		hdr  map[string]string
		want int
	}{
		{map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, http.StatusUnauthorized},
		{map[string]string{"Origin": "https://evil.example"}, http.StatusUnauthorized},
		{map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusUnauthorized},
		{map[string]string{"Origin": h.srv.URL, "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK}, // typed into the address bar
	}
	for _, c := range cases {
		if code := statusWith(t, h, c.hdr); code != c.want {
			t.Errorf("%v: status %d, want %d", c.hdr, code, c.want)
		}
	}
}
