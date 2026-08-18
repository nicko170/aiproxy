package view

import (
	"context"
	"errors"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrUnknownAccount is the sentinel every Source mutation that names an
// account by id wraps its "no such account" error in. Distinguishing this
// from every other failure (e.g. a config-store write error) matters at the
// control API boundary: a caller naming something that does not exist is a
// 404, not a 500 (see internal/proxy.writeMutationError).
var ErrUnknownAccount = errors.New("unknown account")

// Source is the presentation seam (spec §3.1): the single query interface
// both the TUI (stage 4) and the web dashboard (stage 5) read through, so
// neither computes its own aggregate and the two can never disagree (spec
// invariant 4).
//
// internal/tui and internal/web import Source and nothing below it — not
// account, not metrics, not proxy. Two implementations are anticipated:
// Local (this stage) calls the underlying services directly; a future HTTP
// calls the control API and reads its SSE stream. Moving to a detached
// daemon means constructing a different Source in cmd/aiproxy; no other
// package changes. That is the only reason this interface exists, and it is
// why every value crossing it is a plain struct rather than a live pointer
// into proxy state (see types.go).
//
// Every method here has a corresponding route under /_aiproxy/api/v1 (spec
// §9), enforced by TestEveryViewSourceMethodHasAControlRoute. Login maps to
// exactly one route (POST .../accounts/login, "begin a session") like every
// other method; the control API additionally exposes two routes with no
// Source method of their own — submit-code and poll — because a raw
// provider.LoginSession (a channel and funcs) cannot cross HTTP. Those two
// are the seam doing its job: a future view.HTTP synthesizes the channel
// back out of polling (see internal/proxy's login session registry).
type Source interface {
	ServerStatus(ctx context.Context) (Status, error)
	Accounts(ctx context.Context) ([]Account, error)
	UsageSeries(ctx context.Context, q SeriesQuery) (Series, error)
	Totals(ctx context.Context, w Window) (Totals, error)
	LatencyPercentiles(ctx context.Context, w Window) (Latency, error)
	AccountQuotaHistory(ctx context.Context, accountID string, w Window) ([]QuotaPoint, error)
	// Subscribe returns a channel of live request events. The channel is
	// closed-out by cancelling ctx, not by any explicit unsubscribe call; see
	// hub.subscribe. A slow or abandoned subscriber never blocks a proxied
	// request (spec invariant 3): events are dropped for it instead.
	Subscribe(ctx context.Context) (<-chan Event, error)

	SetAccountEnabled(ctx context.Context, accountID string, enabled bool) error
	SetPriority(ctx context.Context, accountID string, priority int) error
	RemoveAccount(ctx context.Context, accountID string) error

	// Settings reads the live-tunable subset of config back (spec §6.2), so a
	// caller can read-modify-write through the seam instead of reaching
	// around Source to the config store directly. Without this, a caller
	// wanting to change one field of Settings has to reconstruct every other
	// field itself — and since UpdateSettings writes every field it is
	// given unconditionally, a caller that gets one of those reconstructed
	// values wrong (or leaves a zero-value bool/slice where the real value
	// was non-zero) silently overwrites it.
	Settings(ctx context.Context) (Settings, error)

	// UpdateSettings validates s, persists it, and applies whichever fields
	// take effect on the running proxy without a restart. The returned
	// Applied says which fields actually took effect versus which were
	// written but require a restart (see Applied's doc comment and
	// Local.UpdateSettings for why that split exists and is returned as data
	// rather than documented).
	UpdateSettings(ctx context.Context, s Settings) (Applied, error)

	// Login starts a PKCE OAuth flow for the named provider (e.g.
	// "anthropic") and returns immediately: the caller shows the returned
	// session's URL (opening a browser is the caller's job, never Login's —
	// see provider.LoginSession's doc comment) and observes completion on
	// its Done channel, or drives SubmitCode for a pasted code. On success
	// the account is already persisted and serving traffic without a
	// restart (spec §6.1) — that happens inside the flow itself, before
	// Done ever fires; Login here is a thin lookup of the named provider.
	Login(ctx context.Context, providerName string) (provider.LoginSession, error)

	// ImportCredentials reads accounts from an external credential file
	// (config.ImportSourceLegacy or config.ImportSourceClaudeCode, spec
	// §6.3), persists any not already present, and adds them to the live
	// Manager without a restart. added counts only the accounts actually
	// added; importing the same source twice adds nothing the second time
	// (deduped on the credential's account uuid, or on label when no uuid is
	// present).
	ImportCredentials(ctx context.Context, source config.ImportSource) (added int, err error)

	// ProbeNow triggers one out-of-band quota-probe cycle (see
	// internal/prober). A cycle already running — the background loop's or
	// another ProbeNow's — is joined rather than duplicated.
	ProbeNow(ctx context.Context) error

	// ApplyUpdate downloads, verifies, and installs the latest release over
	// the running binary, and reports what it did. It deliberately does not
	// restart anything: the running process keeps its open inode and serves
	// the old code until the operator quits it, so the caller shows
	// UpdateResult.Message ("restart to apply") rather than acting on it.
	//
	// Availability is NOT read here — it rides on ServerStatus's Update field,
	// off a background cache, so no UI ever blocks a frame on github.com.
	ApplyUpdate(ctx context.Context) (UpdateResult, error)
}
