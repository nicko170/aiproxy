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

type RetryConfig struct {
	Budget          time.Duration
	InlineAbsorbMax time.Duration
	BodyIdle        time.Duration
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
}

// Result is what happened, for logging and (in stage 2) accounting.
type Result struct {
	Status    int
	AccountID string
	Outcome   provider.OutcomeKind
	Attempts  int
	Rotated   bool
	WaitMS    int64
	TTFBMS    int64
	Bytes     int64
}

type Attempter struct {
	mgr       *account.Manager
	providers map[string]provider.Provider
	rt        http.RoundTripper
	cfg       RetryConfig
	log       *slog.Logger
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
	res = Result{TTFBMS: -1}

	exclude := map[string]bool{}
	refused := map[string]bool{}
	reauthed := map[string]bool{}
	sends := map[string]int{}
	noHintWaits := 0
	started := time.Now()

	defer func() {
		res.WaitMS = a.cfg.Budget.Milliseconds() - budget.Remaining().Milliseconds()
	}()

	for {
		if ctx.Err() != nil {
			res.Outcome = provider.OutcomeServerError
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
		if sends[acct.ID] >= allowance {
			a.log.Info("account exhausted its attempts for this request, rotating",
				"account", acct.Label, "sends", sends[acct.ID])
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		// A credential refresh is dead air like any other, so it is charged to
		// the budget rather than being free. Charge, not Spend: Spend is
		// all-or-nothing, so a refresh that outlasted the remaining budget used
		// to be charged nothing at all and the cost grew with the account count.
		refreshStart := time.Now()
		refreshErr := a.mgr.EnsureFresh(ctx, acct.ID, false)
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
			res.Outcome = provider.OutcomeServerError
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
		res.Attempts++
		upstreamRes, err := a.sendWithin(ctx, budget, prov, acct, req)
		a.mgr.Release(acct.ID)

		if err != nil {
			a.log.Warn("upstream request failed", "account", acct.Label, "err", err)
			exclude[acct.ID] = true
			res.Rotated = true
			continue
		}

		outcome := prov.ClassifyResponse(upstreamRes)
		res.Outcome = outcome.Kind
		a.mgr.UpdateQuota(acct.ID, outcome.Buckets)

		switch outcome.Kind {
		case provider.OutcomeQuotaRejected:
			drain(upstreamRes)
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
			drain(upstreamRes)
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
			drain(upstreamRes)
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
			drain(upstreamRes)
			if !reauthed[acct.ID] {
				reauthed[acct.ID] = true
				forceStart := time.Now()
				err := a.mgr.EnsureFresh(ctx, acct.ID, true)
				budget.Charge(time.Since(forceStart))
				if err == nil {
					continue // same account, fresh credential
				}
				a.log.Warn("forced refresh failed", "account", acct.Label, "err", err)
			}
			exclude[acct.ID] = true
			res.Rotated = true
			continue

		case provider.OutcomeCredentialRefused:
			drain(upstreamRes)
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
		a.relay(ctx, w, upstreamRes, prov, &res)
		return res
	}
}

// sendWithin performs one upstream attempt under the remaining budget, and
// charges whatever it consumed. Waiting for response headers is dead air exactly
// like a backoff, so leaving it unaccounted let ResponseHeaderTimeout (120s)
// govern instead of the budget — N attempts could each stall for nearly the
// whole allowance and the total grew with the number of accounts.
//
// The deadline must NOT survive into the response body: a streamed completion
// legitimately runs for minutes, and cancelling it mid-answer is the very defect
// this proxy exists to avoid. So the timer is armed only around the round trip.
// If it fires first the attempt is cancelled and fails. If the round trip wins,
// the timer is stopped WITHOUT cancelling and the cancel is transferred to the
// response body, firing when the body is closed.
func (a *Attempter) sendWithin(ctx context.Context, budget *Budget, prov provider.Provider, acct account.Account, req Request) (*http.Response, error) {
	attemptCtx, cancel := context.WithCancel(ctx)

	remaining := budget.Remaining()
	timedOut := make(chan struct{})
	timer := time.AfterFunc(remaining, func() {
		close(timedOut)
		cancel()
	})

	start := time.Now()
	res, err := a.send(attemptCtx, prov, acct, req)
	budget.Charge(time.Since(start))

	if !timer.Stop() {
		// The timer already fired, so the failure is ours rather than the
		// upstream's. Report it as such; the loop rotates either way.
		<-timedOut
		if res != nil {
			drain(res)
		}
		cancel()
		return nil, fmt.Errorf("upstream produced no response headers within the remaining %v budget", remaining)
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

func (a *Attempter) relay(ctx context.Context, w http.ResponseWriter, upstreamRes *http.Response, prov provider.Provider, res *Result) {
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

	streaming := strings.Contains(upstreamRes.Header.Get("Content-Type"), "text/event-stream")
	n, err := Relay(ctx, w, upstreamRes.Body, RelayOptions{
		BodyIdle:   a.cfg.BodyIdle,
		Streaming:  streaming,
		ParseUsage: prov.ParseUsage,
		OnUsage:    func(*provider.UsageDelta) {}, // wired to metrics in stage 2
	})
	res.Bytes = n
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

func drain(res *http.Response) {
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
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
	total := len(a.mgr.All())
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
