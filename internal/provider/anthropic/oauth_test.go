package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestNormalizeExpiresAtAcceptsSecondsAndMillis(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{0, 0},
		{1786986000, 1786986000_000},     // seconds promoted to millis
		{1786986000_000, 1786986000_000}, // already millis
	}
	for _, c := range cases {
		if got := NormalizeExpiresAt(c.in); got != c.want {
			t.Errorf("NormalizeExpiresAt(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestExpiryPredicates(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	future := now.Add(time.Hour).UnixMilli()
	past := now.Add(-time.Minute).UnixMilli()

	if IsExpired(future, now) {
		t.Error("a future expiry is not expired")
	}
	if !IsExpired(past, now) {
		t.Error("a past expiry is expired")
	}
	if IsExpired(0, now) {
		t.Error("an unknown expiry must not be treated as expired")
	}
	if !IsExpiringSoon(now.Add(2*time.Minute).UnixMilli(), now, 5*time.Minute) {
		t.Error("expiry inside the threshold is expiring soon")
	}
	if IsExpiringSoon(future, now, 5*time.Minute) {
		t.Error("expiry beyond the threshold is not expiring soon")
	}
}

func TestRefreshTokenReturnsRotatedCredential(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"access_token":  "new-at",
		"refresh_token": "new-rt",
		"expires_in":    3600,
	})
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   string(body),
	})

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "old-rt")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.AccessToken != "new-at" || got.RefreshToken != "new-rt" {
		t.Errorf("credential = %+v", got)
	}
	if got.ExpiresAt <= time.Now().UnixMilli() {
		t.Errorf("ExpiresAt = %d, want a future unix ms", got.ExpiresAt)
	}

	var sent map[string]any
	json.Unmarshal(up.Requests()[0].Body, &sent)
	if sent["grant_type"] != "refresh_token" || sent["refresh_token"] != "old-rt" {
		t.Errorf("request body = %+v", sent)
	}
}

// An endpoint that omits refresh_token means the old one is still valid;
// dropping it would lose the only way to re-authenticate.
func TestRefreshTokenKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Body:   `{"access_token":"new-at","expires_in":60}`,
	})

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "keep-me")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.RefreshToken != "keep-me" {
		t.Errorf("RefreshToken = %q, want the original", got.RefreshToken)
	}
}

func TestRefreshTokenRetriesServerErrorsThenSucceeds(t *testing.T) {
	up := testutil.NewFakeUpstream(t,
		testutil.Script{Status: 503, Body: `{"error":"unavailable"}`},
		testutil.Script{Status: 200, Body: `{"access_token":"at","expires_in":60}`},
	)

	got, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "rt")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if got.AccessToken != "at" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if n := len(up.Requests()); n != 2 {
		t.Errorf("made %d requests, want 2 (one retry)", n)
	}
}

// A 400 means the refresh token itself is dead. Retrying cannot help and the
// caller must be able to tell this from a transient failure so it can surface a
// re-login instead of looping.
func TestRefreshTokenDoesNotRetryRejection(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 400, Body: `{"error":"invalid_grant"}`,
	})

	_, err := RefreshToken(context.Background(), http.DefaultClient, up.URL(), "dead")
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("err = %v, want ErrRefreshRejected", err)
	}
	if n := len(up.Requests()); n != 1 {
		t.Errorf("made %d requests, want exactly 1", n)
	}
}
