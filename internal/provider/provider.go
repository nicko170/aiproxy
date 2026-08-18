// Package provider defines the seam between the proxy core and a specific
// upstream API. The core never imports a concrete provider.
package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// ErrUnsupported is returned by optional Provider methods a provider does not
// implement, such as Quota for an endpoint with no usage API.
var ErrUnsupported = errors.New("unsupported by this provider")

// ErrCredentialRejected reports that the upstream REFUSED a credential, as
// opposed to the proxy having failed to reach the upstream at all. Providers
// wrap it around their own rejection errors.
//
// It lives here, on the provider seam, because the distinction has to be legible
// to internal/account — which decides whether a failed refresh sidelines an
// account — without internal/account importing any concrete provider. The
// distinction is load-bearing: a rejection is permanent and the account needs a
// fresh login, while a DNS hiccup or a dropped connection says nothing about the
// credential and must never remove an account from rotation.
var ErrCredentialRejected = errors.New("credential rejected")

type CredentialType string

const (
	CredentialOAuth  CredentialType = "oauth"
	CredentialAPIKey CredentialType = "apikey"
)

// Credential is what authorizes one account. Persisted in config.
type Credential struct {
	Type         CredentialType `json:"type"`
	AccessToken  string         `json:"accessToken,omitempty"`
	RefreshToken string         `json:"refreshToken,omitempty"`
	APIKey       string         `json:"apiKey,omitempty"`
	ExpiresAt    int64          `json:"expiresAt,omitempty"` // unix ms
}

// Profile identifies who a credential belongs to.
type Profile struct {
	AccountUUID string
	Email       string
	DisplayName string
	OrgUUID     string
	OrgName     string
	Plan        string
}

// QuotaBucket is one rate-limit window as reported by the upstream.
type QuotaBucket struct {
	Name        string  // "5h", "7d", "7d_oi"
	Utilization float64 // 0..1
	Status      string  // "allowed", "rejected", or ""
	ResetsAt    int64   // unix ms, 0 when unknown
}

// Quota is a zero-spend read of an account's current windows.
type Quota struct {
	Buckets    []QuotaBucket
	ObservedAt int64 // unix ms
}

type OutcomeKind int

const (
	OutcomeOK OutcomeKind = iota
	OutcomeQuotaRejected
	OutcomeThrottledWithHint
	OutcomeThrottledNoHint
	OutcomeCredentialStale
	OutcomeCredentialRefused
	OutcomeClientError
	OutcomeServerError
	// OutcomeNoAccountReady is the proxy's own verdict rather than a classified
	// upstream response: no account could be made ready, so nothing was sent.
	//
	// It exists because the zero value of this type is OutcomeOK, so a request
	// answered 429 without a single attempt reported "ok" — a failure recorded as
	// a success. Stage 2 writes this field into the metrics store, where that
	// would quietly corrupt every outcome breakdown derived from it.
	//
	// Append new kinds here, never insert: these values are persisted.
	OutcomeNoAccountReady
	// OutcomeUpstreamError is a transport-level failure reaching the upstream:
	// connection reset, TLS failure, or the per-attempt header timeout.
	OutcomeUpstreamError
	// OutcomeAdmissionError is a local failure before any request was sent.
	OutcomeAdmissionError
)

func (k OutcomeKind) String() string {
	switch k {
	case OutcomeOK:
		return "ok"
	case OutcomeQuotaRejected:
		return "quota_rejected"
	case OutcomeThrottledWithHint:
		return "throttled_with_hint"
	case OutcomeThrottledNoHint:
		return "throttled_no_hint"
	case OutcomeCredentialStale:
		return "credential_stale"
	case OutcomeCredentialRefused:
		return "credential_refused"
	case OutcomeClientError:
		return "client_error"
	case OutcomeServerError:
		return "server_error"
	case OutcomeNoAccountReady:
		return "no_account_ready"
	case OutcomeUpstreamError:
		return "upstream_error"
	case OutcomeAdmissionError:
		return "admission_error"
	}
	return "unknown"
}

// Outcome is a classified upstream response. RetryAfter is meaningful only for
// OutcomeThrottledWithHint. ScopedModel is set when a quota rejection applies to
// one model family rather than the whole account.
type Outcome struct {
	Kind        OutcomeKind
	RetryAfter  time.Duration
	Buckets     []QuotaBucket
	ScopedModel string
}

// UsageDelta is token accounting parsed from a streamed event.
type UsageDelta struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// StartsMessage is true for the event that opens a message. The accumulator
	// needs it because output_tokens is a running total scoped to one message:
	// without a boundary it cannot tell a new message's counter from a
	// continuation of the previous one.
	StartsMessage bool
}

// Account is the subset of account state a provider needs. Defined here rather
// than imported from internal/account so providers do not depend on the
// registry.
type Account struct {
	ID          string
	Label       string
	Credential  Credential
	AccountUUID string
	Upstream    string            // override base URL, empty for provider default
	ModelMap    map[string]string // request model -> upstream model
}

// Provider adapts one upstream API to the proxy core.
type Provider interface {
	Name() string

	Refresh(ctx context.Context, c Credential) (Credential, error)
	Profile(ctx context.Context, c Credential) (Profile, error)
	Quota(ctx context.Context, c Credential) (Quota, error)

	Endpoint(a Account) *url.URL
	Authorize(r *http.Request, c Credential)
	RewriteBody(body []byte, a Account) ([]byte, error)
	ClassifyResponse(r *http.Response) Outcome
	ParseUsage(sseEvent []byte) (*UsageDelta, bool)
	// ParseUsageBody extracts token counts from a complete non-streaming
	// response body. A streamed response reports usage through ParseUsage
	// instead; covering only one shape silently loses every non-streaming
	// request's accounting.
	ParseUsageBody(body []byte) (*UsageDelta, bool)
}
