package view

import "context"

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
// §9), enforced by TestEveryLockstepMethodHasARoute — except the three
// deliberately deferred to stage 4, named in that test's allowlist:
//
//   - Login and ImportCredentials drive an interactive credential flow (an
//     OAuth loopback callback, a paste-the-code prompt) that only the TUI
//     exists to drive. Building them now means building a caller for them
//     that does not exist yet.
//   - ProbeNow triggers an out-of-band quota probe. The background prober it
//     would signal is stage-4 scope; there is nothing yet for it to prod.
//
// They are omitted from Source itself, not merely unrouted, so the absence
// reads as a decision recorded here rather than something a future stage
// has to rediscover by noticing a gap.
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
	UpdateSettings(ctx context.Context, s Settings) error
}
