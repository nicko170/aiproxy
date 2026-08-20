package openai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// loginTimeout bounds the whole flow end to end: from returning the
// authorize URL to a callback (or pasted code) arriving. Mirrors
// anthropic.loginTimeout — two minutes is generous for a human to switch to
// a browser, sign in, and get redirected back, while still finite: an
// abandoned flow must not leak its listener forever.
const loginTimeout = 2 * time.Minute

// ErrStateMismatch reports that a callback or pasted code's state parameter
// did not match the one this session generated. This is a forged or replayed
// callback, or a code pasted from a different session — an error, never a
// warning silently ignored.
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

const (
	// callbackPort is the port Codex registers with the OAuth client. The
	// redirect_uri must match exactly, so this cannot be an ephemeral port.
	callbackPort = 1455
	// callbackFallbackPort is used when 1455 is already bound, which happens
	// when Codex itself is mid-login.
	callbackFallbackPort = 1457

	scopes = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// pkceVerifier is 64 random bytes base64url-encoded without padding, matching
// Codex's own generator and landing inside RFC 7636's 43..128 characters. The
// verifier itself must never be logged, returned, or otherwise leave the
// login flow; only its one-way S256 hash (pkceChallenge) is ever put in the
// authorize URL.
func pkceVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("openai: pkce verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randState returns a random, URL-safe state parameter — the same length and
// entropy as the PKCE verifier, though nothing ties the two together.
func randState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("openai: state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func redirectURI(port int) string {
	return "http://localhost:" + strconv.Itoa(port) + "/auth/callback"
}

func (o *OpenAI) loginTimeout() time.Duration {
	if o.LoginTimeoutOverride > 0 {
		return o.LoginTimeoutOverride
	}
	return loginTimeout
}

func (o *OpenAI) authorizeURL(port int, challenge, state string) string {
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {redirectURI(port)},
		"scope":                      {scopes},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"originator":                 {originator},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return defaultIssuer + "/oauth/authorize?" + q.Encode()
}

// bindCallbackFixed takes the registered port, falling back to the alternate
// one when it is already held — typically by Codex running its own login.
// This is the default; tests replace it via OpenAI.BindCallbackOverride with
// an ephemeral bind so the suite never touches the real, registered port (see
// that field's doc comment for why a fixed system-wide port is otherwise a
// flake-by-construction in a test).
func bindCallbackFixed() (net.Listener, int, error) {
	for _, p := range []int{callbackPort, callbackFallbackPort} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("openai: ports %d and %d are both in use",
		callbackPort, callbackFallbackPort)
}

// bindCallback resolves to BindCallbackOverride when set (tests), or the
// real fixed-port-with-fallback behaviour otherwise.
func (o *OpenAI) bindCallback() (net.Listener, int, error) {
	if o.BindCallbackOverride != nil {
		return o.BindCallbackOverride()
	}
	return bindCallbackFixed()
}

// Login implements provider.Provider.Login: PKCE with S256, a loopback
// callback listener on the registered port (falling back to an alternate
// one), and an authorize URL the caller shows and may open in a browser.
// Login never opens a browser or logs anything itself — see
// provider.LoginSession's doc comment.
func (o *OpenAI) Login(ctx context.Context) (provider.LoginSession, error) {
	verifier, err := pkceVerifier()
	if err != nil {
		return provider.LoginSession{}, err
	}
	state, err := randState()
	if err != nil {
		return provider.LoginSession{}, err
	}

	ln, port, err := o.bindCallback()
	if err != nil {
		return provider.LoginSession{}, fmt.Errorf("start login callback listener: %w", err)
	}

	authURL := o.authorizeURL(port, pkceChallenge(verifier), state)

	f := &loginFlow{
		o: o, verifier: verifier, state: state, redirectURI: redirectURI(port),
		done: make(chan provider.LoginResult, 1),
	}
	f.ctx, f.cancel = context.WithCancel(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", f.handleCallback)
	f.srv = &http.Server{Handler: mux}

	go f.serve(ln)
	go f.awaitTimeout(o.loginTimeout())

	return provider.LoginSession{
		URL:        authURL,
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
	o                            *OpenAI
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
// but exiting.
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
// context Login was called with being cancelled out from under the flow.
// Calling finish() here unconditionally is safe: if finish already ran on
// another path, once.Do makes this a no-op.
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
// the fixed callback port, a browser prefetch, or any other stray GET can
// arrive first. Claiming (tryClaim) before validating state would let
// exactly that stray request win the flow's one chance to complete and
// finish() it with ErrStateMismatch, so the real callback — arriving moments
// later with the correct state — would find the flow already claimed and get
// "already complete" instead of ever succeeding. A state mismatch is
// correctly an error for the request that sent it either way (this handler
// always answers it 400); the point is only to not let that request take the
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

// SubmitCode accepts a pasted authorization code — the fallback for when no
// browser can reach the loopback listener at all (e.g. over SSH). A user
// copying from the browser may paste any of three shapes: the provider's own
// "code#state" copy-paste format, the full callback URL (copied from the
// address bar when the redirect failed to reach the loopback listener), or a
// bare code with no state at all. A bare code is accepted without a state
// check, since a manually pasted bare code has nothing to check it against.
func (f *loginFlow) SubmitCode(code string) error {
	if !f.tryClaim() {
		return ErrLoginSessionComplete
	}
	c, state, hasState := parsePastedCode(code)
	if hasState {
		if state != f.state {
			f.finish(provider.LoginResult{Err: ErrStateMismatch})
			return ErrStateMismatch
		}
	}
	if c == "" {
		f.finish(provider.LoginResult{Err: errors.New("oauth: empty code")})
		return errors.New("oauth: empty code")
	}
	f.complete(c)
	return nil
}

// parsePastedCode extracts an authorization code and, if present, a state
// from manually pasted input, trying each accepted shape in turn: a full
// callback URL with ?code=&state= query parameters, the provider's
// "code#state" copy-paste format, and finally a bare code with no state.
func parsePastedCode(input string) (code, state string, hasState bool) {
	input = strings.TrimSpace(input)
	if u, err := url.Parse(input); err == nil && u.Scheme != "" && u.Host != "" {
		if c := u.Query().Get("code"); c != "" {
			s := u.Query().Get("state")
			return c, s, s != ""
		}
	}
	if c, s, found := strings.Cut(input, "#"); found {
		return c, s, true
	}
	return input, "", false
}

// complete exchanges the code, reads the profile, persists via
// OnLoginSuccess, and finishes the session — always off the calling
// goroutine (the HTTP handler, or SubmitCode's caller), so neither blocks on
// the network round trip this performs.
//
// exchange runs on f.ctx, not context.Background(): finish() cancels f.ctx
// as its very first action, so a Cancel or timeout that reaches finish while
// the exchange is still in flight now aborts it directly instead of letting
// it run to completion in the background. The terminated check immediately
// before OnLoginSuccess closes the remaining gap — the exchange happening to
// complete successfully in the narrow window before f.ctx's cancellation is
// observed — so a login the caller was already told failed or was cancelled
// can never still persist and go live.
func (f *loginFlow) complete(code string) {
	go func() {
		cred, err := f.exchange(code)
		if err != nil {
			f.finish(provider.LoginResult{Err: err})
			return
		}
		profile, err := f.o.Profile(f.ctx, cred)
		if err != nil {
			f.finish(provider.LoginResult{Err: fmt.Errorf("read profile after login: %w", err)})
			return
		}
		if f.terminated.Load() {
			// Cancel or the timeout already finished this flow while the
			// exchange above was in flight (see this method's doc comment) —
			// the caller has already received that result and must not also
			// see the credential persisted.
			return
		}
		if f.o.OnLoginSuccess != nil {
			if err := f.o.OnLoginSuccess(context.Background(), cred, profile); err != nil {
				f.finish(provider.LoginResult{Err: fmt.Errorf("persist login: %w", err)})
				return
			}
		}
		f.finish(provider.LoginResult{Profile: profile})
	}()
}

// exchange trades an authorization code plus PKCE verifier for a credential,
// through the same postToken helper Refresh uses (oauth.go), with an
// authorization_code grant instead of refresh_token.
func (f *loginFlow) exchange(code string) (provider.Credential, error) {
	return f.o.postToken(f.ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {f.redirectURI},
		"client_id":     {clientID},
		"code_verifier": {f.verifier},
	})
}
