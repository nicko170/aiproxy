package tui

import (
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
	// picking is true between pressing "l" and choosing a provider: the
	// picker in front of the flow itself, mirroring how the Accounts
	// screen's "i" opens a source picker before ImportCredentials runs (see
	// acctsState.importing). Nothing else in loginState is meaningful while
	// this is true.
	picking  bool
	active   bool
	provider string
	sess     view.LoginSession
	code     textinput.Model
	err      string
}

// loginProviders is the fixed set of providers the login picker offers, in
// display order. This is a short literal rather than a list read back
// through view.Source: doing that properly needs a new Source method (and
// its own control-API route, per TestEveryViewSourceMethodHasAControlRoute)
// to expose what's actually registered, which is a larger change than one
// confined to the TUI. Keep this mirrored against cmd/aiproxy's providers
// map if a third provider ever lands.
var loginProviders = []struct{ key, label, name string }{
	{"a", "Anthropic", "anthropic"},
	{"c", "ChatGPT", "openai"},
}

// providerDisplayName is loginProviders' name→label lookup, used once a
// provider has already been chosen and the flow needs to talk about it (the
// panel title). Falls back to the raw name so an unrecognised provider
// still renders something rather than an empty title.
func providerDisplayName(name string) string {
	for _, p := range loginProviders {
		if p.name == name {
			return p.label
		}
	}
	return name
}

type loginStartedMsg struct {
	sess view.LoginSession
	err  error
}

type loginDoneMsg struct {
	res view.LoginResult
	ok  bool // false: the channel closed without a result — not a success
}

// startLogin begins the PKCE flow for providerName, which the caller has
// already chosen (see loginPickKey). A picking prompt in progress is not
// this function's concern — the caller clears it before calling in.
func (m Model) startLogin(providerName string) (tea.Model, tea.Cmd) {
	if m.login.active {
		return m, nil
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "paste the code here"
	ti.CharLimit = 400
	ti.Focus()
	m.login = loginState{active: true, code: ti, provider: providerName}
	src := m.src
	// The flow's context lifetime is the session, not this call: src.Login
	// returns as soon as its loopback listener is bound, long before the
	// user has even looked at the URL, let alone opened a browser. Wrapping
	// it in fetchTimeout (meant for quick read/write queries) used to cancel
	// the flow's context the instant this command returned — and since the
	// flow now correctly treats a cancelled parent as a real termination
	// (see loginFlow.awaitTimeout), that surfaced as an immediate "context
	// cancelled" in the pane instead of a working login. m.ctx — the app's
	// root context, live for the pane's whole lifetime — is what the flow
	// needs; it already enforces its own two-minute timeout internally, so
	// no outer one is needed here. esc (loginKey) and updateLogin's
	// loginStartedMsg branch still cancel the session explicitly on their
	// own paths; this only changes how long the context backing Login
	// itself survives.
	ctx := m.ctx
	return m, func() tea.Msg {
		sess, err := src.Login(ctx, providerName)
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

// loginPickKey handles a keypress while the provider picker is open —
// exactly the shape of accounts.go's import-source picker (s.importing):
// a recognised key clears the prompt and acts, anything else (including
// esc) clears it and does nothing, so cancelling never starts a login.
func (m Model) loginPickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	for _, p := range loginProviders {
		if p.key == key {
			m.login = loginState{}
			return m.startLogin(p.name)
		}
	}
	m.login = loginState{}
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

// viewLoginPick renders the provider picker, in the same visual language as
// the Accounts screen's import-source picker (see viewAccounts's "import
// from:" line): a dim label followed by each choice's accented key.
func (m Model) viewLoginPick(h int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.bold("log in") + "\n\n")
	b.WriteString(th.dim("log in with:  "))
	for _, p := range loginProviders {
		b.WriteString(th.accent(p.key) + " " + p.label + "  ")
	}
	b.WriteString(th.dim("esc cancel") + "\n")
	return overlay(b.String(), m.width, h)
}

func (m Model) viewLogin(h int) string {
	th := m.th
	l := m.login
	var b strings.Builder
	b.WriteString(th.bold("log in with "+providerDisplayName(l.provider)) + "\n\n")
	if l.sess.URL == "" && l.err == "" {
		b.WriteString(th.dim("starting the login flow…") + "\n")
	} else if l.sess.URL != "" {
		b.WriteString(th.dim("1.") + " open this url " + th.dim("(ctrl+o opens it — or select and copy the text below)") + "\n")
		// Hard-wrap, never truncate: a URL has no spaces to word-wrap on,
		// and a truncated authorize URL can be neither clicked nor pasted.
		// Each line also carries its own OSC 8 hyperlink (degraded away by
		// theme.hyperlink when the terminal cannot use it), since most
		// terminals' own URL detection breaks at a line boundary.
		for _, line := range wrapURL(l.sess.URL, max(20, m.width-16)) {
			b.WriteString("   " + th.accent(th.hyperlink(l.sess.URL, line)) + "\n")
		}
		b.WriteString("\n")
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
