package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/rules"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

const gtPrompt = `{"model":"claude-opus-5","system":"Primary working directory: /Users/n/code/GT/app","messages":[]}`
const plainPrompt = `{"model":"claude-opus-5","system":"Primary working directory: /Users/n/code/aiproxy","messages":[]}`

func groupedAccount(id, group string, priority int, upstream string) config.Account {
	return config.Account{
		ID: id, Provider: "anthropic", Label: id, Upstream: upstream, Priority: priority, Group: group,
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at-" + id, RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}
}

// personal ranks first on priority; gt only wins when the rules prefer it.
func newGroupedHarness(t *testing.T, opts func(*HandlerOptions), scripts ...testutil.Script) *routerHarness {
	t.Helper()
	if len(scripts) == 0 {
		for i := 0; i < 4; i++ {
			scripts = append(scripts, testutil.Script{Status: 200, Body: `{}`})
		}
	}
	return newRouterHarnessWith(t, func(up string) []config.Account {
		return []config.Account{
			groupedAccount("personal", "personal", 0, up),
			groupedAccount("gt", "gt", 5, up),
		}
	}, opts, scripts...)
}

func post(t *testing.T, h *routerHarness, body, session string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

func gtRules(strict bool) []config.RoutingRule {
	return []config.RoutingRule{{Match: "code/GT", Group: "gt", Strict: strict}}
}

func TestRouterRoutesMatchingWorkToItsGroup(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(false), "personal", false)

	post(t, h, gtPrompt, "")
	if r := h.lastResult(t); r.AccountID != "gt" || r.Group != "gt" || r.Spilled {
		t.Errorf("GT work: result = %+v, want served by gt, group gt, not spilled", r)
	}
	post(t, h, plainPrompt, "")
	if r := h.lastResult(t); r.AccountID != "personal" || r.Group != "personal" {
		t.Errorf("other work: result = %+v, want served by personal", r)
	}
}

func TestRouterMarksSpilledWork(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(false), "personal", false)
	h.mgr.MarkRateLimited("gt", time.Hour)

	post(t, h, gtPrompt, "")
	if r := h.lastResult(t); r.AccountID != "personal" || !r.Spilled || r.Group != "gt" {
		t.Errorf("result = %+v, want served by personal and marked spilled from gt", r)
	}
}

// The privacy filter rewrites the very text the rule looks for; the rule must
// still see the client's original body.
func TestRouterMatchesBeforeRedaction(t *testing.T) {
	h := newGroupedHarness(t, func(o *HandlerOptions) {
		d, err := rules.New(rules.Builtin(), nil)
		if err != nil {
			t.Fatal(err)
		}
		deny, err := rules.NewDenylist([]string{"code/GT"})
		if err != nil {
			t.Fatal(err)
		}
		o.Privacy = privacy.New(privacy.Options{
			Detectors:     []privacy.Detector{d, deny},
			Key:           []byte("0123456789abcdef0123456789abcdef"),
			Unresolved:    privacy.Passthrough,
			OnScanFailure: privacy.Closed,
		})
	})
	h.mgr.SetRouting(gtRules(false), "personal", false)

	post(t, h, gtPrompt, "")
	if sent := string(h.up.Requests()[0].Body); strings.Contains(sent, "code/GT") {
		t.Fatalf("precondition: the denylist should have redacted the path upstream: %s", sent)
	}
	if r := h.lastResult(t); r.AccountID != "gt" {
		t.Errorf("served by %q, want gt: matching must run on the pre-redaction body", r.AccountID)
	}
}

func TestRouterStrictRefusalUsesTheGroupsOwnReset(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(true), "personal", false)
	now := time.Now()
	h.mgr.UpdateQuota("gt", []provider.QuotaBucket{{Name: "5h", Utilization: 1, ResetsAt: now.Add(90 * time.Second).UnixMilli()}})
	h.mgr.UpdateQuota("personal", []provider.QuotaBucket{{Name: "5h", Utilization: 0.1, ResetsAt: now.Add(10 * time.Second).UnixMilli()}})

	res, body := post(t, h, gtPrompt, "")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", res.StatusCode, body)
	}
	if ra := res.Header.Get("Retry-After"); ra != "89" && ra != "90" {
		t.Errorf("Retry-After = %q, want ~90 from gt's own reset, not personal's 10", ra)
	}
	if r := h.lastResult(t); r.Outcome != provider.OutcomeNoAccountReady {
		t.Errorf("outcome = %v, want NoAccountReady", r.Outcome)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("strict work must not reach an account outside its group")
	}
}

func TestRouterStrictRefusalForAnEmptyGroupIsNotRetryable(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting([]config.RoutingRule{{Match: "code/GT", Group: "ghost", Strict: true}}, "personal", false)

	res, body := post(t, h, gtPrompt, "")
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `ghost`) || !strings.Contains(body, "strict") {
		t.Errorf("body should name the group and the strict rule: %s", body)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("upstream must not be called")
	}
}

func TestRouterStrictWorkWhoseGroupWasEmptiedMidSession(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(true), "personal", false)
	post(t, h, gtPrompt, "s1")
	if r := h.lastResult(t); r.AccountID != "gt" {
		t.Fatalf("first request served by %q, want gt", r.AccountID)
	}

	if err := h.mgr.SetGroup("gt", "renamed"); err != nil {
		t.Fatal(err)
	}
	res, body := post(t, h, plainPrompt, "s1")
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(body, `\"gt\"`) {
		t.Errorf("status = %d body = %s, want the non-retryable error naming gt", res.StatusCode, body)
	}
}

func TestRouterNonStrictWorkWhoseGroupWasEmptiedMidSessionSpills(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(false), "personal", false)
	post(t, h, gtPrompt, "s1")

	if err := h.mgr.SetGroup("gt", "renamed"); err != nil {
		t.Fatal(err)
	}
	post(t, h, plainPrompt, "s1")
	if r := h.lastResult(t); r.AccountID != "personal" || !r.Spilled {
		t.Errorf("result = %+v, want served by personal and marked spilled", r)
	}
}

// The group exists and is healthy but cannot serve this model at all — a
// path-only strict rule that also matches Codex bodies, against a Claude-only
// group. Waiting fixes nothing, so it must not be a retryable 429 forever.
func TestRouterStrictRefusalWhenTheGroupLacksTheModelIsNotRetryable(t *testing.T) {
	h := newGroupedHarness(t, nil)
	h.mgr.SetRouting(gtRules(true), "personal", false)
	h.mgr.UpdateModels("gt", []provider.Model{{ID: "claude-sonnet-5"}})

	res, body := post(t, h, gtPrompt, "") // asks for claude-opus-5
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d (Retry-After %q), want a non-retryable 502: %s", res.StatusCode, res.Header.Get("Retry-After"), body)
	}
	if !strings.Contains(body, "claude-opus-5") || !strings.Contains(body, `\"gt\"`) {
		t.Errorf("body should name the model and the group: %s", body)
	}
}
