package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/provider"
)

// hopByHop headers describe one connection and must not be forwarded.
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true,
	"te": true, "trailer": true, "upgrade": true,
	"proxy-authorization": true, "proxy-authenticate": true, "proxy-connection": true,
	"host": true,
}

// connectionSpecific must be stripped from responses: illegal on HTTP/2 and
// hop-by-hop on HTTP/1.1, so removing them is correct on both.
var connectionSpecific = map[string]bool{
	"connection": true, "keep-alive": true, "transfer-encoding": true,
	"upgrade": true, "proxy-connection": true, "te": true, "trailer": true,
}

// minRetryWait floors every inline retry wait, hinted or not.
//
// A hint of zero is a real statement — "retry promptly" — but taking it
// literally makes the retry free, and a free retry against a persistent
// 429/Retry-After: 0 is an unbounded hot loop: no wait to draw the budget down,
// no account excluded, straight back to the top. That shape is not
// hypothetical; Anthropic's own /api/oauth/usage endpoint has been observed
// answering exactly 429 with Retry-After: 0. Zero therefore means "as soon as
// this floor allows", not "instantly, forever".
const minRetryWait = 250 * time.Millisecond

// noHintBackoff is the schedule for a 429 that carried no usable hint. Short and
// finite: the point is to yield the socket briefly, not to guess a duration.
var noHintBackoff = []time.Duration{
	minRetryWait, 500 * time.Millisecond, time.Second,
}

// overloadedBackoff is the schedule for a 529 that carried no Retry-After. It
// climbs where noHintBackoff stays short, because the two are answers to
// different questions. A headerless 429 is a claim upstream never made, so
// guessing hard at a duration is unwarranted and rotating is cheap. A 529 is an
// explicit statement that upstream has no capacity, which rotating cannot fix
// and which typically clears in seconds — so waiting longer is the useful
// response rather than a guess.
//
// The last entry repeats for as long as the overload budget allows, so the
// schedule bounds the wait BETWEEN attempts and the budget bounds the total.
var overloadedBackoff = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
}

// maxSendsPerAccount bounds how many times one account may be sent to within a
// single request. Two is the initial try plus one absorbed retry; a forced
// refresh after a 401 earns one extra, which is itself bounded by the reauthed
// set. Without this cap the only thing standing between a misbehaving upstream
// and a spin is the wait schedule, and any wait that rounds to zero removes it.
const maxSendsPerAccount = 2

// maxHintHold caps how long a hinted rate limit may hold an account.
const maxHintHold = 5 * time.Minute

// defaultQuotaHold is how long an account is held out of SELECTION after a quota
// rejection whose reset time is unknown. This never makes a client wait — it
// only removes the account from selection — so a default here is safe in a way
// that a default client-facing delay is not.
const defaultQuotaHold = 5 * time.Minute

// defaultHeaderTimeout bounds how long ONE attempt may wait for response
// headers. It is a different clock from the budget and must stay that way.
//
// The budget (§4.2) covers only time WE add: retry backoff, waiting on a paused
// account in Admit, inline absorption of a hinted rate limit, credential
// refresh, and the drain of a response we are discarding. Waiting for an
// upstream's first token is not time we added — a large context with extended
// thinking legitimately takes seconds to produce response headers, and that is
// real work, not dead air. Charging it to the budget severed healthy requests:
// with budgetMs at 1000 against an upstream taking 1.3s to first token, every
// attempt was cancelled and the client got a 429 with nothing wrong upstream.
//
// What the two clocks bound together is the worst case, and it is chosen rather
// than accidental:
//
//	pre-first-byte <= budgetMs + (attempts x headerTimeoutMs)
//
// where attempts is itself capped by maxSendsPerAccount x the number of enabled
// accounts. With the defaults (10s budget, 60s header timeout, 2 sends each)
// three accounts bound the worst case at 10s + 6x60s. That is deliberately
// generous: it exists to make an upstream that accepts a connection and then
// withholds headers forever terminate at all, not to be tight. Tightness on the
// paths we control is the budget's job.
const defaultHeaderTimeout = 60 * time.Second

// defaultOverloadedBudget bounds the total wait spent absorbing 529s for one
// request. It is a third clock alongside the budget and the header timeout, and
// it is separate from the budget rather than folded into it because the two
// bound different risks.
//
// The budget protects the paths that RECOVER a request: rotating to another
// account, refreshing a credential, absorbing a hinted throttle. Every one of
// those competes with the others, which is exactly why they share an allowance.
// Waiting out an overload competes with none of them — there is nothing else to
// try, because every account faces the same exhausted upstream. Charging it to
// the same allowance would mean a single 529 wait consuming the budget that a
// subsequent 401 or 429 needs to rotate, turning a transient overload into a
// failed request for an unrelated reason.
//
// 30s is chosen to outlast a typical overload while staying inside the client
// timeouts a coding agent is likely to impose. It extends the pre-first-byte
// worst case, which is stated rather than left to be discovered:
//
//	pre-first-byte <= budgetMs + overloadedBudgetMs + (attempts x headerTimeoutMs)
const defaultOverloadedBudget = 30 * time.Second

type RetryConfig struct {
	Budget          time.Duration
	InlineAbsorbMax time.Duration
	BodyIdle        time.Duration
	// HeaderTimeout bounds one attempt's wait for response headers. It does NOT
	// draw down the budget; see defaultHeaderTimeout.
	HeaderTimeout time.Duration
	// OverloadedBudget bounds the total time spent waiting out upstream
	// overloads (529) for one request. It is a THIRD clock, separate from
	// Budget on purpose; see defaultOverloadedBudget.
	OverloadedBudget time.Duration
}

// Request is a client request ready to be attempted, body already buffered so it
// can be replayed on another account.
type Request struct {
	Method    string
	Path      string
	Header    http.Header
	Body      []byte
	Model     string
	SessionID string
	// Restore carries the placeholders this request's body was redacted with, so
	// the relay can undo them in the response. Nil when the privacy filter is
	// disabled, which leaves the relay's write path untouched.
	Restore *privacy.Table
}

// Result is what happened, for logging and accounting.
type Result struct {
	Status    int
	AccountID string
	Outcome   provider.OutcomeKind
	Attempts  int
	Rotated   bool
	// StartedAt is unix ms when Do began, stamped once at the top of the loop.
	StartedAt int64
	// DurationMS is the whole request's wall-clock time, set in the same
	// deferred block that sets WaitMS.
	DurationMS int64
	WaitMS     int64
	TTFBMS     int64
	Bytes      int64
	// Stream reports whether the upstream answered with an event stream, taken
	// from the response Content-Type in relay below. It is NOT "did a body
	// arrive": deriving it from Bytes > 0 made the persisted stream column a
	// duplicate of bytes > 0 and marked every non-streaming JSON response as
	// streamed.
	Stream bool

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

type Attempter struct {
	mgr       *account.Manager
	providers map[string]provider.Provider
	rt        http.RoundTripper
	cfg       RetryConfig
	log       *slog.Logger

	// privacy builds the per-request restorer that undoes redaction in the
	// response. Set by proxyHandler from HandlerOptions.Privacy, alongside
	// OnResult below, rather than threaded through NewAttempter: the handler
	// owns the filter's lifecycle and the attempter only ever needs to build a
	// restorer from a table the request already carries.
	privacy *privacy.Filter

	// OnResult, when set, receives every completed attempt — including one that
	// ends in panic(http.ErrAbortHandler) on a truncated relay. It is invoked
	// from Do's own deferred block for exactly that reason: a caller that reads
	// Do's return value cannot see it on the panic path, since the panic unwinds
	// past the return entirely. See the defer in Do.
	OnResult func(Request, Result)
}

func NewAttempter(m *account.Manager, providers map[string]provider.Provider, rt http.RoundTripper, cfg RetryConfig, log *slog.Logger) *Attempter {
	if cfg.Budget <= 0 {
		cfg.Budget = 10 * time.Second
	}
	if cfg.InlineAbsorbMax <= 0 {
		cfg.InlineAbsorbMax = 5 * time.Second
	}
	if cfg.BodyIdle <= 0 {
		cfg.BodyIdle = 120 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = defaultHeaderTimeout
	}
	if cfg.OverloadedBudget <= 0 {
		cfg.OverloadedBudget = defaultOverloadedBudget
	}
	return &Attempter{mgr: m, providers: providers, rt: rt, cfg: cfg, log: log}
}

// Do runs the attempt loop until a response is relayed or the budget runs out.
//
// The loop's contract: nothing is written to w until an attempt produces a
// relayable response, and every path that does not write is bounded by the
// budget. That is what makes an unbounded silent hang unreachable — not the
// choice of any individual backoff constant below.
//
// The result is NAMED so the deferred WaitMS accounting below is observable. With
// an unnamed result, `return res` copies into the return slot before the defer
// runs, and the defer then mutates a dead local — every caller saw WaitMS == 0
// no matter how much dead air the request actually spent.
func (a *Attempter) Do(ctx context.Context, w http.ResponseWriter, req Request) (res Result) {
	budget := NewBudget(a.cfg.Budget)
	// overload is the second allowance, spent only on 529s. Kept apart from
	// budget so waiting out an upstream capacity problem cannot starve the
	// rotation and refresh paths; see defaultOverloadedBudget.
	overload := NewBudget(a.cfg.OverloadedBudget)
	res = Result{TTFBMS: -1}

	exclude := map[string]bool{}
	refused := map[string]bool{}
	reauthed := map[string]bool{}
	sends := map[string]int{}
	// overloadRetries counts 529 retries per account, which is what buys those
	// retries an exemption from maxSendsPerAccount below.
	overloadRetries := map[string]int{}
	noHintWaits := 0
	overloadWaits := 0
	started := time.Now()
	res.StartedAt = started.UnixMilli()
	// lastSent is the account the previous attempt in THIS request went to, which
	// is what makes a rotation onto a different account observable below.
	lastSent := ""

	defer func() {
		res.WaitMS = (a.cfg.Budget.Milliseconds() - budget.Remaining().Milliseconds()) +
			(a.cfg.OverloadedBudget.Milliseconds() - overload.Remaining().Milliseconds())
		res.DurationMS = time.Since(started).Milliseconds()
		// Invoked from THIS defer, not by the caller after Do returns: a defer
		// runs during panic unwinding, before the panic continues up the stack,
		// so this is the one place that observes a truncated relay's result. The
		// panic is not recovered here — http.ErrAbortHandler must keep propagating
		// so net/http still severs the connection instead of finishing it cleanly.
		if a.OnResult != nil {
			a.OnResult(req, res)
		}
	}()

	for {
		if ctx.Err() != nil {
			// The CLIENT's context is what is checked here, not an upstream
			// response — a hang-up (Ctrl-C on a streaming agent) is routine and is
			// neither a genuine upstream 5xx nor a local admission failure.
			// OutcomeServerError is deliberately narrowed to mean the former;
			// reporting it here would permanently pollute the upstream-error rate
			// in every outcome breakdown with disconnects that were never upstream's
			// doing.
			res.Outcome = provider.OutcomeClientDisconnected
			return res
		}
		if budget.Exhausted() {
			a.writeExhausted(w, &res, "pre-first-byte budget exhausted")
			return res
		}

		// acct is a value copy: the manager never hands out a pointer into live
		// state, because a concurrent EnsureFresh rewrites Credential under the
		// lock and a torn read of those strings yields a garbage token.
		acct, err := a.mgr.Select(account.SelectRequest{
			Model: req.Model, SessionID: req.SessionID, Exclude: exclude,
		})
		if err != nil {
			a.writeNoAccount(w, &res, refused)
			return res
		}
		res.AccountID = acct.ID

		prov := a.providers[acct.Provider]
		if prov == nil {
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// Checked before the refresh so an account that has used up its tries
		// costs nothing further. A forced refresh earns one extra send; reauthed
		// is set at most once per account, so the bonus cannot compound.
		allowance := maxSendsPerAccount
		if reauthed[acct.ID] {
			allowance++
		}
		// Each absorbed 529 earns one more send on this account. The cap exists to
		// stop a spin, and an overload retry is not one: it is paced by
		// overloadedBackoff and bounded in total by the overload budget, so the
		// thing the cap protects against is already ruled out. Without this,
		// maxSendsPerAccount would rotate after a single retry onto accounts
		// facing the identical exhausted upstream.
		allowance += overloadRetries[acct.ID]
		if sends[acct.ID] >= allowance {
			a.log.Info("account exhausted its attempts for this request, rotating",
				"account", acct.Label, "sends", sends[acct.ID])
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// Spec §4.1 step 6: pace the burst onto an account we have just failed over
		// to. This is the ramp's whole stated purpose and its only production
		// trigger — without it the mechanism arms solely from PauseAccount, which
		// covers the hinted-throttle queue and nothing else. A fleet of agents
		// rotating at the same instant otherwise arrives on the next account as one
		// burst, trips its limit, and cascades onward. Armed before Admit, so this
		// request is itself subject to the cap it just created.
		if lastSent != "" && acct.ID != lastSent {
			a.mgr.BeginRamp(acct.ID)
		}

		// A credential refresh is dead air like any other, so it is both CAPPED by
		// the remaining budget and CHARGED for what it used. Charging alone was not
		// enough: the cost was only recognised after the fact, so one slow token
		// endpoint could still spend far more than the whole allowance before the
		// client heard anything. Capping alone would not be enough either — a
		// refresh cut short still consumed real wall-clock.
		//
		// Cancelling this derived context ends only THIS wait. The refresh itself
		// runs on the manager's own background context, so it completes and the
		// next request finds the result instead of starting a second one.
		refreshStart := time.Now()
		refreshErr := a.ensureFreshWithin(ctx, budget, acct.ID, false)
		budget.Charge(time.Since(refreshStart))
		if refreshErr != nil {
			a.log.Warn("credential refresh failed", "account", acct.Label, "err", refreshErr)
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}
		// A refresh can drain the budget on its own. Answer now rather than
		// spending another round trip discovering it.
		if budget.Exhausted() {
			a.writeExhausted(w, &res, "credential refresh exhausted the budget")
			return res
		}

		if err := a.mgr.Admit(ctx, acct.ID, budget); err != nil {
			if errors.Is(err, ErrBudgetExhausted) {
				a.writeExhausted(w, &res, "admission exceeded the budget")
				return res
			}
			// Anything else here is a local failure — an unknown account, or a
			// cancelled context. Returning without writing lets net/http emit a
			// clean, empty 200, which a client reads as a successful but empty
			// answer instead of an error worth retrying.
			a.log.Error("admission failed", "account", acct.Label, "err", err)
			res.Outcome = provider.OutcomeAdmissionError
			res.Status = http.StatusBadGateway
			a.writeJSON(w, http.StatusBadGateway, nil, "proxy_error",
				"Could not admit the request to an account.")
			return res
		}

		// Re-read after EnsureFresh: the copy taken by Select predates any
		// rotation it performed, and sending the superseded token would produce a
		// 401 that looks like the upstream's fault.
		if fresh, ok := a.mgr.Get(acct.ID); ok {
			acct = fresh
		}

		sends[acct.ID]++
		lastSent = acct.ID
		res.Attempts++
		upstreamRes, err := a.sendWithin(ctx, budget, prov, acct, req)
		a.mgr.Release(acct.ID)

		if err != nil {
			a.log.Warn("upstream request failed", "account", acct.Label, "err", err)
			// An attempt that was SENT and failed is not "no account was ready".
			// Leaving the outcome at its zero value let writeExhausted default it to
			// OutcomeNoAccountReady, so a header timeout — or a dropped connection —
			// was reported as a fleet with no capacity, which is the opposite of what
			// happened: an account was selected, admitted, and sent to.
			//
			// OutcomeUpstreamError: a transport-level failure reaching the upstream
			// (connection reset, TLS failure, or the per-attempt header timeout),
			// distinct from OutcomeServerError, which is a genuine upstream 5xx
			// response actually received.
			res.Outcome = provider.OutcomeUpstreamError
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		outcome := prov.ClassifyResponse(upstreamRes)
		res.Outcome = outcome.Kind
		a.mgr.UpdateQuota(acct.ID, outcome.Buckets)

		switch outcome.Kind {
		case provider.OutcomeQuotaRejected:
			drainWithin(budget, upstreamRes)
			// A model-scoped rejection leaves the account fine for other models,
			// so the recorded bucket does the excluding rather than a global hold.
			if outcome.ScopedModel == "" {
				a.mgr.MarkRateLimited(acct.ID, holdFor(outcome, defaultQuotaHold))
			}
			a.log.Info("quota rejected, rotating", "account", acct.Label, "scoped", outcome.ScopedModel)
			exclude[acct.ID] = true
			res.Rotated = true
			continue

		case provider.OutcomeThrottledWithHint:
			drainWithin(budget, upstreamRes)
			hold := outcome.RetryAfter
			if hold > maxHintHold {
				hold = maxHintHold
			}
			a.mgr.PauseAccount(acct.ID, hold)
			if outcome.RetryAfter <= a.cfg.InlineAbsorbMax {
				// Upstream stated a short duration, so waiting genuinely works and
				// the same account keeps its warm cache. Floored: a hint of zero
				// would otherwise make the retry free and spin.
				wait := max(outcome.RetryAfter, minRetryWait)
				if err := budget.Wait(ctx, wait); err != nil {
					a.writeRetryAfter(w, &res, outcome.RetryAfter, "rate limited")
					return res
				}
				continue
			}
			a.writeRetryAfter(w, &res, outcome.RetryAfter, "rate limited")
			return res

		case provider.OutcomeThrottledNoHint:
			drainWithin(budget, upstreamRes)
			// Nothing was stated, so no duration is invented. Yield briefly and
			// let another account try — spending the rest of the budget here
			// would be betting on a claim upstream never made.
			backoff := noHintBackoff[min(noHintWaits, len(noHintBackoff)-1)]
			noHintWaits++
			exclude[acct.ID] = true
			res.Rotated = true
			a.log.Info("rate limited with no hint, rotating",
				"account", acct.Label, "backoff", backoff)
			if err := budget.Wait(ctx, backoff); err != nil {
				a.writeExhausted(w, &res, "rate limited with no retry hint")
				return res
			}
			continue

		case provider.OutcomeCredentialStale:
			drainWithin(budget, upstreamRes)
			if !reauthed[acct.ID] {
				reauthed[acct.ID] = true
				forceStart := time.Now()
				err := a.ensureFreshWithin(ctx, budget, acct.ID, true)
				budget.Charge(time.Since(forceStart))
				if err == nil {
					continue // same account, fresh credential
				}
				a.log.Warn("forced refresh failed", "account", acct.Label, "err", err)
			}
			exclude[acct.ID] = true
			res.Rotated = true
			continue

		case provider.OutcomeOverloaded:
			// Upstream has no capacity. Rotating cannot help — every account
			// reaches the same exhausted upstream — so this waits in place and
			// keeps the account's warm cache. Nothing is held, paused, or
			// excluded: the account did nothing wrong.
			//
			// A zero or absent Retry-After both take the schedule. On the 429
			// path the two are distinguished, because there a zero hint is a
			// real statement worth honouring promptly. Here they are not, and
			// deliberately so: retrying an out-of-capacity API immediately is
			// what turns an overload into a stampede, so the one case where
			// upstream asks for exactly that is the case to ignore.
			wait := outcome.RetryAfter
			if wait <= 0 {
				wait = overloadedBackoff[min(overloadWaits, len(overloadedBackoff)-1)]
			}
			wait = max(wait, minRetryWait)
			if wait > overload.Remaining() {
				// No room to wait it out. Falling out of the switch relays
				// upstream's own 529 and body, which is more use to the client
				// than a proxy-invented error: the agent already knows what a
				// 529 means and retries on its own.
				break
			}
			drainWithin(overload, upstreamRes)
			overloadWaits++
			overloadRetries[acct.ID]++
			a.log.Info("upstream overloaded, waiting on the same account",
				"account", acct.Label, "wait", wait, "attempt", overloadWaits)
			if err := overload.Wait(ctx, wait); err != nil {
				a.writeExhausted(w, &res, "upstream overloaded")
				return res
			}
			continue

		case provider.OutcomeCredentialRefused:
			drainWithin(budget, upstreamRes)
			// Never relayed: the client has no part in this and reads a 403 as
			// its own session being dead.
			a.log.Error("upstream refused the account credential", "account", acct.Label)
			refused[acct.ID] = true
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// Relayable. Any non-throttled response is live proof a hold no longer
		// binds, which is what restores an account after its window passes.
		a.mgr.ClearRateLimited(acct.ID)
		a.mgr.RecordSession(req.SessionID, acct.ID)

		res.Status = upstreamRes.StatusCode
		res.TTFBMS = time.Since(started).Milliseconds()
		a.relay(ctx, w, upstreamRes, prov, req, &res)
		return res
	}
}

// ensureFreshWithin waits for a credential refresh for no longer than the budget
// has left.
//
// The cap is on this caller's WAIT, not on the refresh: account.Manager runs the
// refresh on its own background context precisely so a caller that gives up
// neither kills it nor forces the next request to start again from scratch.
func (a *Attempter) ensureFreshWithin(ctx context.Context, budget *Budget, id string, force bool) error {
	remaining := budget.Remaining()
	if remaining <= 0 {
		return ErrBudgetExhausted
	}
	refreshCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	return a.mgr.EnsureFresh(refreshCtx, id, force)
}

// sendWithin performs one upstream attempt under the HEADER TIMEOUT, which is a
// different clock from the budget on purpose.
//
// The budget is not consulted here and the round trip is not charged to it. The
// budget covers time the proxy ADDS (§4.2); an upstream thinking its way to a
// first token adds nothing — it is the work being paid for. Bounding the round
// trip by budget.Remaining() conflated the two and severed healthy requests: a
// 1s budget against a 1.3s time-to-first-token cancelled every attempt and
// answered 429 with a perfectly healthy account on the other end. See
// defaultHeaderTimeout for the worst case the two clocks bound together.
//
// What survives from that earlier fix is the concern underneath it, which was
// real: an upstream may accept a connection and then withhold headers
// indefinitely, and the transport's own ResponseHeaderTimeout (120s) is too
// coarse to be the only answer. HeaderTimeout is that answer instead — finite,
// per attempt, and generous enough for a slow first token.
//
// The deadline must NOT survive into the response body: a streamed completion
// legitimately runs for minutes, and cancelling it mid-answer is the very defect
// this proxy exists to avoid. So the timer is armed only around the round trip.
// If it fires first the attempt is cancelled and fails. If the round trip wins,
// the timer is stopped WITHOUT cancelling and the cancel is transferred to the
// response body, firing when the body is closed.
func (a *Attempter) sendWithin(ctx context.Context, budget *Budget, prov provider.Provider, acct account.Account, req Request) (*http.Response, error) {
	attemptCtx, cancel := context.WithCancel(ctx)

	limit := a.cfg.HeaderTimeout
	timedOut := make(chan struct{})
	timer := time.AfterFunc(limit, func() {
		close(timedOut)
		cancel()
	})

	res, err := a.send(attemptCtx, prov, acct, req)

	if !timer.Stop() {
		// The timer already fired, so the failure is ours rather than the
		// upstream's. Report it as such; the loop rotates either way. The drain
		// below IS budget-bounded: discarding a body we have decided not to relay
		// is time the proxy adds.
		<-timedOut
		if res != nil {
			drainWithin(budget, res)
		}
		cancel()
		return nil, fmt.Errorf("upstream produced no response headers within the %v header timeout", limit)
	}
	if err != nil {
		cancel()
		return nil, err
	}

	// Transfer the cancel to the body so the attempt context is released when the
	// relay (or drain) finishes with it, and not a moment before.
	res.Body = &cancelOnCloseBody{ReadCloser: res.Body, cancel: cancel}
	return res, nil
}

// cancelOnCloseBody releases an attempt's context when its body is closed.
// context.CancelFunc is idempotent, so a double Close is harmless.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// send builds and performs one upstream attempt. acct is taken by value: see the
// note in Do about why the manager never hands out a pointer.
func (a *Attempter) send(ctx context.Context, prov provider.Provider, acct account.Account, req Request) (*http.Response, error) {
	pa := acct.ToProvider()
	body, err := prov.RewriteBody(req.Body, pa)
	if err != nil {
		return nil, err
	}

	target := strings.TrimSuffix(prov.Endpoint(pa).String(), "/") + req.Path

	var reader io.Reader
	if len(body) > 0 && req.Method != http.MethodGet && req.Method != http.MethodHead {
		reader = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, target, reader)
	if err != nil {
		return nil, err
	}

	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || strings.HasPrefix(lk, ":") {
			continue
		}
		// The client's proxy key authenticates it to US and must never leak
		// upstream. Accept-Encoding is dropped so response framing always matches
		// what we tell the client. Content-Length is recomputed below.
		if lk == "x-api-key" || lk == "authorization" || lk == "accept-encoding" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	prov.Authorize(out, acct.Credential)
	if reader != nil {
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}

	return a.rt.RoundTrip(out)
}

func (a *Attempter) relay(ctx context.Context, w http.ResponseWriter, upstreamRes *http.Response, prov provider.Provider, req Request, res *Result) {
	defer upstreamRes.Body.Close()

	for k, vs := range upstreamRes.Header {
		lk := strings.ToLower(k)
		if connectionSpecific[lk] || lk == "content-encoding" || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(upstreamRes.StatusCode)

	acc := NewUsageAccumulator()
	streaming := strings.Contains(upstreamRes.Header.Get("Content-Type"), "text/event-stream")
	// Carried onto the result so accounting records what the upstream actually
	// did, rather than re-deriving it from the byte count downstream.
	res.Stream = streaming
	ropts := RelayOptions{
		BodyIdle:   a.cfg.BodyIdle,
		Streaming:  streaming,
		ParseUsage: prov.ParseUsage,
		ParseBody:  prov.ParseUsageBody,
		OnUsage: func(d *provider.UsageDelta) {
			if d != nil && d.StartsMessage {
				acc.StartMessage()
			}
			acc.Observe(d)
		},
	}
	// Len() > 0, not merely non-nil. Filter.Redact ALWAYS returns a table, so
	// req.Restore is non-nil for every request once the filter is on — and an
	// empty table provably cannot resolve anything, because the table is complete
	// by construction when redaction finishes (see privacy.Table). Arming the
	// restorer anyway put restoreChunk in the path of every stream, which buffers
	// to the last event terminator and turns a token stream into a batch: with
	// the filter on, a 121-byte stream that wrote 113/7 bytes wrote 0/120
	// instead. Property 3 — no sentinel, no added buffering — is restored exactly
	// for the majority of requests, which redact nothing.
	if a.privacy != nil && req.Restore.Len() > 0 {
		ropts.Restore = a.privacy.Restorer(req.Restore)
	}
	n, err := Relay(ctx, w, upstreamRes.Body, ropts)
	res.Bytes = n
	totals := acc.Totals()
	res.InputTokens = totals.InputTokens
	res.OutputTokens = totals.OutputTokens
	res.CacheReadTokens = totals.CacheReadTokens
	res.CacheWriteTokens = totals.CacheWriteTokens
	if err != nil && !errors.Is(err, context.Canceled) {
		a.log.Warn("relay ended early", "err", err, "bytes", n)
		// Abort instead of returning normally. A normal return lets net/http
		// terminate the chunked body cleanly, which looks to the client like a
		// complete answer and suppresses the retry a truncated stream needs.
		// ErrAbortHandler severs the connection without the recovery middleware
		// treating it as a crash: chi's middleware.Recoverer re-panics this
		// sentinel rather than converting it to a 500.
		panic(http.ErrAbortHandler)
	}
}

// holdFor derives a selection hold from a rejected bucket's reset time.
func holdFor(o provider.Outcome, fallback time.Duration) time.Duration {
	now := time.Now().UnixMilli()
	var soonest int64
	for _, b := range o.Buckets {
		if b.Status != "rejected" || b.ResetsAt <= now {
			continue
		}
		if soonest == 0 || b.ResetsAt < soonest {
			soonest = b.ResetsAt
		}
	}
	if soonest == 0 {
		return fallback
	}
	if d := time.Duration(soonest-now) * time.Millisecond; d < time.Hour {
		return d
	}
	return time.Hour
}

// maxDrain caps how long discarding a rotated-away response body may take, on
// top of the budget cap. A body we have already decided not to relay is worth a
// moment's read so the connection can be reused, and not one second more.
const maxDrain = 250 * time.Millisecond

// drainWithin discards a response body we are rotating away from, bounded by
// what the request has left and charged for what it costs.
//
// The bound is the point. sendWithin stops its timer the instant response
// headers arrive and transfers cancellation to the body, so from here on the
// attempt context carries NO deadline. An upstream that flushes a bare 429
// promptly and then withholds the body therefore stalled the request forever —
// once per account, on every rotation path — with nothing written to the client.
// That is the same unbounded-silence class the budget exists to make unreachable.
//
// Closing the body is what unblocks the read: Close fires cancelOnCloseBody,
// which cancels the attempt context and severs the connection underneath the
// pending Read. Charging afterwards matters as much as capping: a drain cut
// short still burned real wall-clock, and an uncharged cost lets the next
// account spend the same allowance again.
func drainWithin(b *Budget, res *http.Response) {
	limit := min(b.Remaining(), maxDrain)
	if limit <= 0 {
		res.Body.Close()
		return
	}
	t := time.AfterFunc(limit, func() { res.Body.Close() })
	defer t.Stop()

	start := time.Now()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
	b.Charge(time.Since(start))
}

func (a *Attempter) writeJSON(w http.ResponseWriter, status int, hdr map[string]string, errType, msg string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}

// writeExhausted answers a request whose budget ran out. The Retry-After comes
// from observed reset times rather than a guess, so the client backs off on
// something real.
func (a *Attempter) writeExhausted(w http.ResponseWriter, res *Result, reason string) {
	retry := a.retryAfterHint()
	secs := strconv.Itoa(int(retry.Seconds()))
	res.Status = http.StatusTooManyRequests
	// Nothing upstream classified this request, so without an explicit verdict the
	// zero value would report it as OutcomeOK — a 429 logged as a success. A real
	// classification from an earlier attempt (a throttle, say) is left alone.
	if res.Outcome == provider.OutcomeOK {
		res.Outcome = provider.OutcomeNoAccountReady
	}
	a.log.Warn("answering 429", "reason", reason, "retryAfter", retry)
	a.writeJSON(w, http.StatusTooManyRequests,
		map[string]string{"Retry-After": secs}, "rate_limit_error",
		"No account could serve this request in time ("+reason+"). Retry in "+secs+"s.")
}

func (a *Attempter) writeRetryAfter(w http.ResponseWriter, res *Result, d time.Duration, reason string) {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	res.Status = http.StatusTooManyRequests
	a.writeJSON(w, http.StatusTooManyRequests,
		map[string]string{"Retry-After": strconv.Itoa(secs)},
		"rate_limit_error", reason+"; retry in "+strconv.Itoa(secs)+"s.")
}

// writeNoAccount distinguishes "every account was refused", which waiting cannot
// fix, from "everything is out of quota", which a reset will.
func (a *Attempter) writeNoAccount(w http.ResponseWriter, res *Result, refused map[string]bool) {
	// Enabled accounts only. Counting disabled ones too made "every account
	// refused" all but unreachable the moment an operator switched one off, so a
	// dead credential — which no amount of waiting fixes — came back as a 429
	// telling the client to retry later.
	var total int
	for _, acct := range a.mgr.All() {
		if !acct.Disabled {
			total++
		}
	}
	if len(refused) > 0 && len(refused) >= total {
		names := make([]string, 0, len(refused))
		for id := range refused {
			if acct, ok := a.mgr.Get(id); ok {
				names = append(names, acct.Label)
			}
		}
		res.Status = http.StatusBadGateway
		res.Outcome = provider.OutcomeCredentialRefused
		a.writeJSON(w, http.StatusBadGateway, nil, "proxy_error",
			"Upstream refused the credential for every account ("+strings.Join(names, ", ")+
				"). Check the accounts and log in again.")
		return
	}
	a.writeExhausted(w, res, "no eligible account")
}

// retryAfterHint is the soonest observed reset across all accounts, floored at
// one second and capped at five minutes.
func (a *Attempter) retryAfterHint() time.Duration {
	now := time.Now().UnixMilli()
	var soonest int64
	consider := func(ts int64) {
		if ts > now && (soonest == 0 || ts < soonest) {
			soonest = ts
		}
	}
	for _, acct := range a.mgr.Snapshot() {
		for _, b := range acct.Buckets {
			consider(b.ResetsAt)
		}
		consider(acct.RateLimitedUntil)
	}
	if soonest == 0 {
		return 5 * time.Second
	}
	d := time.Duration(soonest-now) * time.Millisecond
	if d < time.Second {
		return time.Second
	}
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}
