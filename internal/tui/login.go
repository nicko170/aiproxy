package tui

import (
	"context"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// loginState drives one PKCE flow. It holds the session's URL and the
// user's pasted authorization code — never the credential the flow obtains;
// that is persisted inside the flow and does not pass through this package
// at all (view.LoginResult carries only a profile).
type loginState struct {
	active bool
	sess   view.LoginSession
	code   textinput.Model
	err    string
}

type loginStartedMsg struct {
	sess view.LoginSession
	err  error
}

type loginDoneMsg struct {
	res view.LoginResult
	ok  bool // false: the channel closed without a result — not a success
}

func (m Model) startLogin() (tea.Model, tea.Cmd) {
	if m.login.active {
		return m, nil
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "paste the code here"
	ti.CharLimit = 400
	ti.Focus()
	m.login = loginState{active: true, code: ti}
	src := m.src
	parent := m.ctx
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		sess, err := src.Login(ctx, "anthropic")
		return loginStartedMsg{sess: sess, err: err}
	}
}

// waitLogin receives the flow's single result. A closed channel with no
// value is "no result", never success: a zero LoginResult is
// indistinguishable from a clean login, so only a received value counts.
func waitLogin(done <-chan view.LoginResult) tea.Cmd {
	return func() tea.Msg {
		v, ok := <-done
		return loginDoneMsg{res: v, ok: ok}
	}
}

func (m Model) updateLogin(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case loginStartedMsg:
		if !m.login.active {
			// Cancelled before the session arrived; tear the flow down.
			if msg.err == nil && msg.sess.Cancel != nil {
				msg.sess.Cancel()
			}
			return m, nil
		}
		if msg.err != nil {
			m.login.err = msg.err.Error()
			return m, nil
		}
		m.login.sess = msg.sess
		return m, waitLogin(msg.sess.Done)

	case loginDoneMsg:
		if !m.login.active {
			return m, nil
		}
		if !msg.ok {
			m.login.err = "the login flow ended without a result — press esc and try again"
			return m, nil
		}
		if msg.res.Err != nil {
			m.login.err = msg.res.Err.Error()
			return m, nil
		}
		m.login = loginState{}
		label := msg.res.Profile.Email
		if label == "" {
			label = "new account"
		}
		m.flash = m.newFlash(sevOK, "logged in as "+label)
		m.fetchingStatus = true
		return m, m.fetchStatus()
	}
	return m, nil
}

func (m Model) loginKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.login.sess.Cancel != nil {
			m.login.sess.Cancel()
		}
		m.login = loginState{}
		return m, nil
	case "enter":
		code := strings.TrimSpace(m.login.code.Value())
		if code == "" || m.login.sess.SubmitCode == nil {
			return m, nil
		}
		if err := m.login.sess.SubmitCode(code); err != nil {
			m.login.err = err.Error()
			return m, nil
		}
		m.login.err = ""
		return m, nil
	case "ctrl+o":
		if url := m.login.sess.URL; url != "" {
			return m, openURL(url)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.login.code, cmd = m.login.code.Update(msg)
	return m, cmd
}

func openURL(url string) tea.Cmd {
	return func() tea.Msg {
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", url)
		case "windows":
			c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			c = exec.Command("xdg-open", url)
		}
		return openedMsg{err: c.Start()}
	}
}

func (m Model) viewLogin(h int) string {
	th := m.th
	l := m.login
	var b strings.Builder
	b.WriteString(th.bold("log in with Anthropic") + "\n\n")
	if l.sess.URL == "" && l.err == "" {
		b.WriteString(th.dim("starting the login flow…") + "\n")
	} else if l.sess.URL != "" {
		b.WriteString(th.dim("1.") + " open this url " + th.dim("(ctrl+o opens it for you)") + "\n")
		b.WriteString("   " + th.accent(truncate(l.sess.URL, max(20, m.width-16))) + "\n\n")
		b.WriteString(th.dim("2.") + " approve access in the browser\n\n")
		b.WriteString(th.dim("3.") + " if the browser cannot reach this machine, paste the code:\n")
		b.WriteString("   " + l.code.View() + "\n\n")
		b.WriteString(th.dim("waiting for the browser or a pasted code…") + "\n")
	}
	if l.err != "" {
		b.WriteString("\n" + th.bad(truncate(l.err, max(20, m.width-12))) + "\n")
	}
	return overlay(b.String(), m.width, h)
}
