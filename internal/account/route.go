package account

import (
	"bytes"
	"encoding/json"

	"github.com/nicko170/aiproxy/internal/config"
)

// Route is what the routing rules decided for one request: the group of
// accounts it prefers, whether it may spill outside that group, and which
// rule decided it (-1 for the default).
type Route struct {
	Group  string
	Strict bool
	Rule   int
}

// compiledRule is a RoutingRule ready to scan for. A request body is JSON, so
// a rule's text may appear in it escaped: escaped holds that form when it
// differs from the literal, and is nil otherwise.
type compiledRule struct {
	literal []byte
	escaped []byte
	route   Route
}

func compileRule(i int, r config.RoutingRule) compiledRule {
	c := compiledRule{
		literal: []byte(r.Match),
		route:   Route{Group: r.Group, Strict: r.Strict, Rule: i},
	}
	if esc := jsonEscape(r.Match); !bytes.Equal(esc, c.literal) {
		c.escaped = esc
	}
	return c
}

// jsonEscape is s as it appears inside a JSON string, quotes stripped. HTML
// escaping is off because clients do not do it: with it on, a Codex rule like
// "<cwd>/Users/…" would become "<cwd>…" and never match.
func jsonEscape(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return []byte(s)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return b[1 : len(b)-1]
}

func (c compiledRule) matches(body []byte) bool {
	if bytes.Contains(body, c.literal) {
		return true
	}
	return c.escaped != nil && bytes.Contains(body, c.escaped)
}

// SetRouting replaces the routing rules and the default. It clears every
// session's remembered route, so edited rules apply to sessions already in
// flight rather than only to new ones.
func (m *Manager) SetRouting(rules []config.RoutingRule, defaultGroup string, defaultStrict bool) {
	compiled := make([]compiledRule, len(rules))
	for i, r := range rules {
		compiled[i] = compileRule(i, r)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = compiled
	m.defaultRoute = Route{Group: defaultGroup, Strict: defaultStrict, Rule: -1}
	m.sessionRoutes = map[string]Route{}
}

// Classify decides a request's route. A session that has already matched a
// rule keeps that route, with no scan. Otherwise the rules are tried in order,
// first match wins, and a match is remembered for the session; no match is
// the default and is NOT remembered, so a later request in the same session
// carrying the text can still claim it.
//
// The scan runs outside the lock: bodies run to megabytes, and holding the
// Manager's mutex across bytes.Contains would serialise every request.
func (m *Manager) Classify(sessionID string, body []byte) Route {
	m.mu.Lock()
	if sessionID != "" {
		if r, ok := m.sessionRoutes[sessionID]; ok {
			m.mu.Unlock()
			return r
		}
	}
	rules, def := m.rules, m.defaultRoute
	m.mu.Unlock()

	for _, c := range rules {
		if !c.matches(body) {
			continue
		}
		if sessionID == "" {
			return c.route
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if r, ok := m.sessionRoutes[sessionID]; ok {
			return r // a concurrent request in this session decided first
		}
		m.sessionRoutes[sessionID] = c.route
		return c.route
	}
	return def
}
