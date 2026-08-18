package anthropic

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// loginTimeout bounds the whole flow end to end: from returning the
// authorize URL to a callback (or pasted code) arriving. Two minutes is
// generous for a human to switch to a browser, sign in, and get redirected
// back, while still finite — an abandoned flow must not leak its listener
// forever (spec: "Login — the PKCE flow").
const loginTimeout = 2 * time.Minute

// ErrStateMismatch reports that a callback or pasted code's state parameter
// did not match the one this session generated. This is a forged or replayed
// callback, or a code pasted from a different session — an error, never a
// warning silently ignored (spec §6.1: "State is verified on callback").
var ErrStateMismatch = errors.New("oauth: state mismatch")

// ErrLoginTimedOut reports that no callback or pasted code arrived within
// the session's timeout.
var ErrLoginTimedOut = errors.New("oauth: login timed out")

// ErrLoginCancelled reports that LoginSession.Cancel was called before the
// flow completed.
var ErrLoginCancelled = errors.New("oauth: login cancelled")

// ErrLoginSessionComplete reports that SubmitCode (or a callback) was
// received after the session had already delivered its one LoginResult — a
// double submit, rejected rather than silently ignored or sent again on an
// already-closed Done.
var ErrLoginSessionComplete = errors.New("oauth: login session already complete")

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// pkceChallenge returns the S256 code_challenge for a verifier. The verifier
// itself must never be logged, returned, or otherwise leave loginFlow; only
// this one-way hash is ever put in the authorize URL.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *Anthropic) loginTimeout() time.Duration {
	if a.LoginTimeoutOverride > 0 {
		return a.LoginTimeoutOverride
	}
	return loginTimeout
}

// Login implements provider.Provider.Login (spec §6.1): PKCE with S256, a
// loopback callback listener on an ephemeral port, and an authorize URL the
// caller shows and may open in a browser. Login never opens a browser or
// logs anything itself — see provider.LoginSession's doc comment.
func (a *Anthropic) Login(ctx context.Context) (provider.LoginSession, error) {
	verifier, err := randToken(32)
	if err != nil {
		return provider.LoginSession{}, err
	}
	state, err := randToken(16)
	if err != nil {
		return provider.LoginSession{}, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return provider.LoginSession{}, fmt.Errorf("start login callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	authURL, err := url.Parse(AuthorizeURL)
	if err != nil {
		ln.Close()
		return provider.LoginSession{}, fmt.Errorf("parse authorize URL: %w", err)
	}
	q := authURL.Query()
	q.Set("client_id", ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scopes)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	authURL.RawQuery = q.Encode()

	f := &loginFlow{
		a: a, verifier: verifier, state: state, redirectURI: redirectURI,
		done: make(chan provider.LoginResult, 1),
	}
	f.ctx, f.cancel = context.WithCancel(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", f.handleCallback)
	f.srv = &http.Server{Handler: mux}

	go f.serve(ln)
	go f.awaitTimeout(a.loginTimeout())

	return provider.LoginSession{
		URL:        authURL.String(),
		Done:       f.done,
		Cancel:     f.cancelSession,
		SubmitCode: f.SubmitCode,
	}, nil
}

// loginFlow is the state behind one Login call. Exactly one of handleCallback,
// SubmitCode, awaitTimeout, or cancelSession ever completes it, guarded by
// once so Done receives exactly one value on every path and the listener is
// torn down exactly once.
type loginFlow struct {
	a                            *Anthropic
	verifier, state, redirectURI string

	srv  *http.Server
	done chan provider.LoginResult

	ctx    context.Context
	cancel context.CancelFunc

	once      sync.Once
	completed atomic.Bool

	// terminated is set the instant any call to finish begins, before the
	// once-guarded shutdown-and-send even runs — so complete (running
	// concurrently on its own goroutine, mid-exchange or mid-profile-read)
	// can notice a Cancel or timeout that reached finish first and skip
	// OnLoginSuccess instead of persisting an account the caller has already
	// been told failed or was cancelled (see complete's doc comment).
	terminated atomic.Bool
}

// tryClaim reports whether the caller is the first to reach a terminal
// action (a callback, a submitted code) for this flow, claiming it
// atomically if so. A second caller (a double submit, or a callback racing a
// submitted code) gets false and must not start a second exchange.
func (f *loginFlow) tryClaim() bool {
	return f.completed.CompareAndSwap(false, true)
}

// finish delivers res exactly once, after tearing down the listener — so by
// the time any caller observes a value on Done, the loopback listener is
// already closed and no goroutine this flow started is still doing anything
// but exiting (Go's happens-before for a channel send guarantees the
// Shutdown call is complete before a receiver wakes up).
//
// The shutdown-and-send runs on a FRESH goroutine, never on the caller's own,
// deliberately: handleCallback's early-return paths (state mismatch, no
// code) call finish() directly from the HTTP handler goroutine that
// srv.Shutdown would then have to wait to go idle — calling Shutdown
// synchronously from inside the very handler it is waiting on is a
// self-inflicted stall until Shutdown's own grace period elapses. Detaching
// it here means the handler returns immediately, the connection goes idle
// right away, and Shutdown completes promptly instead of stalling every
// state-mismatch and no-code response for the length of its grace period.
//
// Done is deliberately never closed after the send: a second receive on a
// closed buffered channel returns the zero LoginResult (Err == nil, an empty
// Profile) with ok == false, which by value alone is indistinguishable from
// a successful login — a caller that reads with a bare "v := <-ch" instead
// of "v, ok := <-ch" would silently see success. Leaving the channel open
// means a second receive simply blocks forever instead of lying; only
// "v, ok := <-ch" is a safe read of Done, exactly once.
func (f *loginFlow) finish(res provider.LoginResult) {
	f.terminated.Store(true)
	f.once.Do(func() {
		f.cancel()
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.srv.Shutdown(shutdownCtx) //nolint:errcheck // best-effort; the listener is torn down either way
			f.done <- res
		}()
	})
}

func (f *loginFlow) serve(ln net.Listener) {
	if err := f.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		f.finish(provider.LoginResult{Err: fmt.Errorf("login callback listener: %w", err)})
	}
}

// awaitTimeout ends the flow once d elapses with no callback or submitted
// code, or once f.ctx is done for any other reason — including the parent
// context Login was called with being cancelled out from under the flow
// (e.g. the control API used to hand this an HTTP request's context, done
// the instant the begin handler returned). Previously the ctx.Done() branch
// assumed only finish() itself (via f.cancel) could ever reach it, and did
// nothing — so a parent cancellation left the flow never finished: the
// listener stayed bound, this goroutine and the one this flow started never
// exited, and (via the control API) the session's registry entry never
// released. Calling finish() here unconditionally fixes that; if finish
// already ran on another path, once.Do makes this a no-op.
func (f *loginFlow) awaitTimeout(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		f.finish(provider.LoginResult{Err: ErrLoginTimedOut})
	case <-f.ctx.Done():
		f.finish(provider.LoginResult{Err: f.ctx.Err()})
	}
}

func (f *loginFlow) cancelSession() {
	f.finish(provider.LoginResult{Err: ErrLoginCancelled})
}

// handleCallback checks state before claiming the flow, deliberately: this
// listener only ever exists for one flow, but that does not mean every
// request it ever receives is the legitimate redirect — a scanner probing
// the ephemeral port, a browser prefetch, or any other stray GET can arrive
// first. Claiming (tryClaim) before validating state used to let exactly
// that stray request win the flow's one chance to complete and finish() it
// with ErrStateMismatch, so the real callback — arriving moments later with
// the correct state — found the flow already claimed and got "already
// complete" instead of ever succeeding. A state mismatch is correctly an
// error for the request that sent it either way (this handler always
// answers it 400); the fix is only about not letting that request take the
// whole flow down with it. A flow that only ever receives wrong-state
// requests still terminates on its own via awaitTimeout.
func (f *loginFlow) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	if state != f.state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if !f.tryClaim() {
		http.Error(w, "login session already complete", http.StatusGone)
		return
	}
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		f.finish(provider.LoginResult{Err: errors.New("oauth: callback carried no code")})
		return
	}
	fmt.Fprint(w, "Login complete. You can close this tab.")
	f.complete(code)
}

// SubmitCode accepts a pasted authorization code (spec §6.1's SSH fallback,
// where no browser can reach the loopback listener at all). The provider's
// real redirect page encodes the code as "code#state"; a bare code with no
// "#" is accepted without a state check, since a manually pasted bare code
// has nothing to check it against.
func (f *loginFlow) SubmitCode(code string) error {
	if !f.tryClaim() {
		return ErrLoginSessionComplete
	}
	c, state, hasState := strings.Cut(code, "#")
	if hasState {
		if state != f.state {
			f.finish(provider.LoginResult{Err: ErrStateMismatch})
			return ErrStateMismatch
		}
		code = c
	}
	if code == "" {
		f.finish(provider.LoginResult{Err: errors.New("oauth: empty code")})
		return errors.New("oauth: empty code")
	}
	f.complete(code)
	return nil
}

// complete exchanges the code, reads the profile, persists via
// OnLoginSuccess, and finishes the session — always off the calling
// goroutine (the HTTP handler, or SubmitCode's caller), so neither blocks on
// the network round trips this performs.
//
// Both network calls run on f.ctx, not context.Background(): finish()
// cancels f.ctx as its very first action, so a Cancel or timeout that reaches
// finish while either call is still in flight now aborts it directly instead
// of letting it run to completion in the background. The terminated check
// immediately before OnLoginSuccess closes the remaining gap — both calls
// happening to complete successfully in the narrow window before f.ctx's
// cancellation is observed — so a login the caller was already told failed
// or was cancelled can never still persist and go live via mgr.Add.
func (f *loginFlow) complete(code string) {
	go func() {
		cred, err := f.exchange(code)
		if err != nil {
			f.finish(provider.LoginResult{Err: err})
			return
		}
		profile, err := f.a.Profile(f.ctx, cred)
		if err != nil {
			f.finish(provider.LoginResult{Err: fmt.Errorf("read profile after login: %w", err)})
			return
		}
		if f.terminated.Load() {
			// Cancel or the timeout already finished this flow while the
			// exchange or profile read above was in flight (see this method's
			// doc comment) — the caller has already received that result and
			// must not also see the credential persisted.
			return
		}
		if f.a.OnLoginSuccess != nil {
			if err := f.a.OnLoginSuccess(context.Background(), cred, profile); err != nil {
				f.finish(provider.LoginResult{Err: fmt.Errorf("persist login: %w", err)})
				return
			}
		}
		f.finish(provider.LoginResult{Profile: profile})
	}()
}

// exchange trades an authorization code plus PKCE verifier for a credential.
// Mirrors refreshOnce's shape (spec's token endpoint is the same one), but
// with an authorization_code grant instead of refresh_token.
func (f *loginFlow) exchange(code string) (provider.Credential, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"state":         f.state,
		"client_id":     ClientID,
		"redirect_uri":  f.redirectURI,
		"code_verifier": f.verifier,
	})
	if err != nil {
		return provider.Credential{}, err
	}

	req, err := http.NewRequestWithContext(f.ctx, http.MethodPost,
		f.a.tokenEndpoint(), bytes.NewReader(payload))
	if err != nil {
		return provider.Credential{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := f.a.hc.Do(req)
	if err != nil {
		return provider.Credential{}, fmt.Errorf("code exchange request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return provider.Credential{}, fmt.Errorf("code exchange failed with %d: %s",
			res.StatusCode, summarizeExchangeError(body))
	}

	var tr tokenResponse
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		return provider.Credential{}, fmt.Errorf("decode code exchange response: %w", err)
	}
	if tr.AccessToken == "" {
		return provider.Credential{}, errors.New("oauth: no access token in code exchange response")
	}

	cred := provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    NormalizeExpiresAt(tr.ExpiresAt),
	}
	if cred.ExpiresAt == 0 {
		secs := tr.ExpiresIn
		if secs == 0 {
			secs = 3600
		}
		cred.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
	}
	return cred, nil
}

// maxExchangeErrorSummary bounds how much of the token endpoint's error body
// reaches LoginResult.Err, and from there a control-API poll response
// verbatim. It carries no credential today, but it is upstream-controlled
// text on a path everything else here works hard to keep clean, so it is
// bounded harder than a raw body read and reduced to a single-line summary
// rather than echoed as-is.
const maxExchangeErrorSummary = 200

// summarizeExchangeError collapses an upstream error body to a short,
// single-line summary: newlines and other control characters (an HTML error
// page, an escape sequence) are stripped rather than passed through, and the
// result is capped well below the 4096-byte read that produced body.
func summarizeExchangeError(body []byte) string {
	body = bytes.TrimSpace(body)
	s := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20:
			return -1
		default:
			return r
		}
	}, string(body))
	s = strings.TrimSpace(s)
	if len(s) > maxExchangeErrorSummary {
		return s[:maxExchangeErrorSummary] + "..."
	}
	return s
}
