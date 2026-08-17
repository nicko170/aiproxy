package anthropic

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestProfileParsesAccountAndOrganization(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body: `{"account":{"uuid":"acct-1","email":"a@example.com",
		        "display_name":"A","has_claude_max":true},
		        "organization":{"uuid":"org-1","name":"Acme"}}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	got, err := p.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got.AccountUUID != "acct-1" || got.Email != "a@example.com" ||
		got.OrgUUID != "org-1" || got.OrgName != "Acme" {
		t.Errorf("profile = %+v", got)
	}
	if got.Plan != "max" {
		t.Errorf("Plan = %q, want max", got.Plan)
	}
	if h := up.Requests()[0].Header.Get("Authorization"); h != "Bearer at" {
		t.Errorf("Authorization = %q", h)
	}
}

func TestQuotaNormalizesBuckets(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body: `{"five_hour":{"utilization":14,"resets_at":1786986000},
		        "seven_day":{"utilization":1,"resets_at":"2026-08-24T00:00:00Z"},
		        "limits":[{"group":"weekly","percent":63,"resets_at":1787065200,
		                   "scope":{"model":{"display_name":"Claude Fable 5"}}}]}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	got, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}

	byName := map[string]provider.QuotaBucket{}
	for _, b := range got.Buckets {
		byName[b.Name] = b
	}

	five, ok := byName["5h"]
	if !ok {
		t.Fatalf("no 5h bucket in %+v", got.Buckets)
	}
	if five.Utilization != 0.14 {
		t.Errorf("5h utilization = %v, want 0.14 (percent normalized)", five.Utilization)
	}
	if five.ResetsAt != 1786986000_000 {
		t.Errorf("5h resetsAt = %d, want unix ms", five.ResetsAt)
	}
	// "2026-08-24T00:00:00Z" as unix ms, exactly: a seconds/millis slip in
	// precisely the conversion the milliseconds constraint exists to protect
	// must fail this, not just an "!= 0" presence check.
	const wantSevenDayResetsAt = 1787529600_000
	if seven, ok := byName["7d"]; !ok || seven.Utilization != 0.01 {
		t.Errorf("7d bucket = %+v (ok=%v)", seven, ok)
	} else if seven.ResetsAt != wantSevenDayResetsAt {
		t.Errorf("7d resetsAt = %d, want %d (RFC3339 parsed to exact unix ms)", seven.ResetsAt, wantSevenDayResetsAt)
	}
	fable, ok := byName["7d_fable"]
	if !ok {
		t.Fatalf("no model-scoped bucket in %+v", got.Buckets)
	}
	if fable.Utilization != 0.63 {
		t.Errorf("scoped utilization = %v, want 0.63", fable.Utilization)
	}
	if got.ObservedAt == 0 {
		t.Error("ObservedAt should be stamped")
	}
}

// The usage endpoint has its own rate limit. A throttled probe must be
// distinguishable so the caller backs off instead of hammering, and so stale
// quota is not mistaken for fresh.
func TestQuotaReportsThrottling(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 429,
		Header: http.Header{"Retry-After": []string{"0"}},
		Body:   `{"error":{"message":"Rate limited"}}`,
	})
	p := New(http.DefaultClient)
	p.BaseURLOverride = up.URL()

	_, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at",
	})
	if !errors.Is(err, ErrQuotaThrottled) {
		t.Fatalf("err = %v, want ErrQuotaThrottled", err)
	}
}

// A display name with no identifiable model token must fail closed: it must
// not produce a name carrying a "_" suffix, since BucketAppliesTo treats any
// such suffix as a model scope. A spent bucket that ends up looking
// model-scoped when it isn't would be silently ignored for every real model —
// the opposite of safe. The conservative fallback is the plain "7d", which
// merges into the general weekly bucket and binds every model.
func TestModelBucketNameFailsClosedWhenNoTokenSurvives(t *testing.T) {
	cases := []struct {
		displayName string
		want        string
	}{
		{"Claude 5", "7d"}, // only a version number, no name token
		{"", "7d"},         // nothing at all
		{"5", "7d"},        // bare version number
		{"Claude Fable 5", "7d_fable"},
	}
	for _, c := range cases {
		if got := modelBucketName(c.displayName); got != c.want {
			t.Errorf("modelBucketName(%q) = %q, want %q", c.displayName, got, c.want)
		}
	}
}

func TestQuotaUnsupportedForAPIKeyCredential(t *testing.T) {
	p := New(http.DefaultClient)

	_, err := p.Quota(context.Background(), provider.Credential{
		Type: provider.CredentialAPIKey, APIKey: "sk",
	})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v, want provider.ErrUnsupported", err)
	}
}
