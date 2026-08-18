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

// ErrQuotaThrottled reports that a provider's zero-spend usage/quota endpoint
// rate-limited us — distinct from a plain failure to read, and distinct from
// ErrCredentialRejected: the credential is fine, but polling this endpoint
// too hard gets it throttled in its own right (spec §6.2). It lives here,
// not only on the concrete provider that first needed it, because
// internal/prober backs off on it without importing any concrete provider;
// a provider's own sentinel (e.g. anthropic.ErrQuotaThrottled) wraps this one
// so callers can check either.
var ErrQuotaThrottled = errors.New("usage endpoint throttled")

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
	// OutcomeClientDisconnected is the proxy's own verdict when the CLIENT's
	// context is already done at the top of the attempt loop — a hang-up, not a
	// genuine upstream failure. It exists because that branch used to report
	// OutcomeServerError, which is deliberately narrowed to mean a real upstream
	// 5xx actually received; a client going away (Ctrl-C on a streaming agent,
	// routinely) is neither that nor a local admission failure, and folding it
	// into OutcomeServerError permanently polluted the upstream-error rate in
	// every outcome breakdown.
	//
	// Appended here, after OutcomeAdmissionError: see the note above about why
	// new kinds append rather than insert.
	OutcomeClientDisconnected
	// OutcomeBlocked is the proxy's own verdict when a request names a model on
	// the configured blocklist: it is refused locally, before any account is
	// selected and without contacting the upstream.
	//
	// It exists because that refusal used to be recorded as the zero value —
	// "ok" — so a blocked request appeared in every outcome breakdown as a
	// success. Spec §7.1 already names "blocked" in the outcome enum.
	//
	// Appended after OutcomeClientDisconnected; see the note above.
	OutcomeBlocked
	// OutcomeOverloaded is upstream reporting that IT has no capacity right now
	// — Anthropic's 529 — as opposed to this account having no allowance left.
	//
	// It is split out from OutcomeServerError because the two call for opposite
	// handling. A 500 or 503 says nothing about whether retrying helps and
	// nothing about which account to use, so it is relayed. A 529 is transient
	// by definition and account-independent: waiting is the fix, and rotating is
	// not, since every account faces the same exhausted upstream.
	//
	// Keeping it inside OutcomeServerError also made it invisible in the outcome
	// breakdown, where an overload storm and a genuine upstream fault are the
	// two things an operator most needs to tell apart.
	//
	// Appended after OutcomeBlocked; see the note above about why new kinds
	// append rather than insert.
	OutcomeOverloaded
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
	case OutcomeClientDisconnected:
		return "client_disconnected"
	case OutcomeBlocked:
		return "blocked"
	case OutcomeOverloaded:
		return "overloaded"
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

// LoginSession is a non-blocking, inspectable OAuth login flow in progress
// (spec §6.1). It is not JSON-serializable by design — Done is a channel and
// Cancel and SubmitCode are funcs — because a TUI drives it directly through
// view.Source.Login rather than over the wire; the control API instead keeps
// one of these server-side per session and exposes the poll shape spec §9
// asks for (begin / submit-code / poll), since a raw channel cannot cross
// HTTP. See LoginResult's doc comment for why Done never carries a
// credential.
type LoginSession struct {
	// URL is the provider's authorize URL, complete with PKCE challenge,
	// state, and redirect_uri. The caller shows it and may open it in a
	// browser; Login itself never does either (see Provider.Login's doc
	// comment) — a library that shells out to open a browser is untestable.
	URL string
	// Done receives exactly one LoginResult: on a verified callback, a
	// submitted code, a timeout, or a Cancel. It is deliberately never
	// closed after that single send — a second receive on a closed buffered
	// channel would return the zero LoginResult (Err == nil, an empty
	// Profile) with ok == false, indistinguishable by value alone from a
	// successful login. Only "v, ok := <-ch" is a safe read; a bare
	// "v := <-ch" after the first receive blocks forever instead of lying.
	Done <-chan LoginResult
	// Cancel abandons the flow: the loopback listener and every goroutine
	// Login started are torn down, and Done still receives exactly one
	// LoginResult (a cancellation error), never zero and never two.
	Cancel func()
	// SubmitCode accepts a pasted authorization code as an alternative to the
	// browser completing the loopback callback — the fallback that matters
	// the first time this runs over SSH, where no browser can reach the
	// listener at all (spec §6.1). The provider's real authorize response
	// page encodes the code as "code#state"; a bare code with no "#" is
	// accepted too, without a state check, since there is then nothing to
	// check it against. Calling it after the session has already completed
	// returns an error rather than silently doing nothing or sending a
	// second result on Done.
	SubmitCode func(code string) error
}

// LoginResult is delivered exactly once on LoginSession.Done. Profile is the
// only credential-adjacent thing it ever carries — deliberately: whatever
// exchanged the authorization code for a token persists that credential
// itself (see anthropic.Anthropic.OnLoginSuccess) before this is ever sent,
// and the credential never appears in this type, a log line, or a
// control-API response. Err is non-nil for a state mismatch, a timeout, a
// cancellation, an exchange failure, or a failed persist; Profile is the
// zero value whenever Err is set.
type LoginResult struct {
	Profile Profile
	Err     error
}

// Provider adapts one upstream API to the proxy core.
type Provider interface {
	Name() string

	Refresh(ctx context.Context, c Credential) (Credential, error)
	Profile(ctx context.Context, c Credential) (Profile, error)
	Quota(ctx context.Context, c Credential) (Quota, error)
	// Login starts a PKCE OAuth flow: a loopback callback listener on an
	// ephemeral port, and an authorize URL carrying its challenge (spec
	// §6.1). It returns immediately; the flow completes asynchronously on
	// LoginSession.Done. A provider with no interactive login (none in v1)
	// may return ErrUnsupported.
	Login(ctx context.Context) (LoginSession, error)

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
