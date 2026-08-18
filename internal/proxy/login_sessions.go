// A raw provider.LoginSession (a channel and two funcs) cannot cross HTTP.
// loginSessionRegistry keeps one server-side per in-flight control-API login,
// keyed by a generated session id, and synthesizes the pollable shape spec §9
// asks for — begin, submit-code, poll — on top of view.Source.Login's single
// "begin" route. A future view.HTTP will do the mirror image of this on the
// client side, reconstructing a channel out of polling.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/view"
)

// loginSessionRegistry is constructed once per router (see NewRouter) and
// closed over by the three login handlers.
type loginSessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*loginSessionState
}

func newLoginSessionRegistry() *loginSessionRegistry {
	return &loginSessionRegistry{sessions: map[string]*loginSessionState{}}
}

// loginSessionState is one session's poll-able state. sess itself is never
// exposed outside this file; only status/profile/errMsg (never credential
// material — see provider.LoginResult's doc comment, which this simply
// forwards) are ever handed to a handler.
type loginSessionState struct {
	sess provider.LoginSession

	mu      sync.Mutex
	status  string // "pending", "done", or "error"
	profile provider.Profile
	errMsg  string
}

func randomSessionID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// begin starts a login session for providerName through src.Login, registers
// it under a fresh session id, and spawns the one goroutine that drains
// sess.Done into pollable state. Returns the session id and the authorize
// URL the caller shows the user.
func (r *loginSessionRegistry) begin(ctx context.Context, src view.Source, providerName string) (id, url string, err error) {
	sess, err := src.Login(ctx, providerName)
	if err != nil {
		return "", "", err
	}
	id, err = randomSessionID()
	if err != nil {
		return "", "", err
	}

	st := &loginSessionState{sess: sess, status: "pending"}
	r.mu.Lock()
	r.sessions[id] = st
	r.mu.Unlock()

	go func() {
		res, ok := <-sess.Done
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case !ok:
			st.status = "error"
			st.errMsg = "login session ended with no result"
		case res.Err != nil:
			st.status = "error"
			st.errMsg = res.Err.Error()
		default:
			st.status = "done"
			st.profile = res.Profile
		}
	}()

	return id, sess.URL, nil
}

func (r *loginSessionRegistry) get(id string) (*loginSessionState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[id]
	return st, ok
}

// submitCode forwards a pasted code to the underlying session. A session
// whose provider never set SubmitCode (none in v1) reports a clear error
// rather than a nil-func panic.
func (st *loginSessionState) submitCode(code string) error {
	if st.sess.SubmitCode == nil {
		return errors.New("this login session does not accept a pasted code")
	}
	return st.sess.SubmitCode(code)
}

// poll reads the session's current state. profile and errMsg are only
// meaningful when status is "done" or "error" respectively.
func (st *loginSessionState) poll() (status string, profile provider.Profile, errMsg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.status, st.profile, st.errMsg
}
