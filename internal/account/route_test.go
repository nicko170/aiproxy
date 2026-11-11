package account

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

var (
	gtBody    = []byte(`{"system":"Primary working directory: /Users/n/code/GT/x"}`)
	plainBody = []byte(`{"system":"haiku title call"}`)
)

func gtRule() config.RoutingRule { return config.RoutingRule{Match: "code/GT", Group: "gt"} }

func TestClassifyFirstMatchingRuleWins(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{gtRule(), {Match: "code", Group: "other"}}, "personal", false)

	cases := []struct {
		body string
		want Route
	}{
		{`{"system":"Primary working directory: /Users/n/code/GT/x"}`, Route{Group: "gt", Rule: 0}},
		{`{"system":"/Users/n/code/aiproxy"}`, Route{Group: "other", Rule: 1}},
		{`{"system":"nothing"}`, Route{Group: "personal", Rule: -1}},
	}
	for _, c := range cases {
		if got := m.Classify("", []byte(c.body)); got != c.want {
			t.Errorf("Classify(%s) = %+v, want %+v", c.body, got, c.want)
		}
	}
}

func TestClassifyMatchesTheJSONEscapedForm(t *testing.T) {
	text := "say \"hi\"\n<cwd>/a</cwd>"
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{{Match: text, Group: "gt"}}, "", false)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // as clients send it
	if err := enc.Encode(map[string]string{"system": "prefix " + text + " suffix"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Classify("", buf.Bytes()); got.Group != "gt" {
		t.Errorf("escaped body %s did not match: %+v", buf.Bytes(), got)
	}
}

func TestClassifyMatchesNonASCIIText(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{{Match: "/Users/nick/Código", Group: "gt"}}, "", false)
	if got := m.Classify("", []byte(`{"system":"cwd /Users/nick/Código/app"}`)); got.Group != "gt" {
		t.Errorf("non-ASCII rule did not match raw UTF-8: %+v", got)
	}
}

func TestClassifyRemembersAMatchForTheSession(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{gtRule()}, "personal", false)
	m.Classify("s1", gtBody)
	if got := m.Classify("s1", plainBody); got != (Route{Group: "gt", Rule: 0}) {
		t.Errorf("follow-up in the session = %+v, want the remembered gt route", got)
	}
}

func TestClassifyDoesNotRememberTheDefault(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{gtRule()}, "personal", false)
	if got := m.Classify("s1", plainBody); got.Group != "personal" {
		t.Fatalf("unmatched = %+v, want personal", got)
	}
	if got := m.Classify("s1", gtBody); got.Group != "gt" {
		t.Errorf("a later matching request = %+v, want gt: the default must not have been recorded", got)
	}
}

func TestClassifyDoesNotMoveAMatchedSession(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{{Match: "GT", Group: "gt"}, {Match: "OTHER", Group: "other"}}, "", false)
	m.Classify("s1", []byte(`{"x":"GT"}`))
	if got := m.Classify("s1", []byte(`{"x":"OTHER"}`)); got.Group != "gt" {
		t.Errorf("session moved to %+v; the first match must stick", got)
	}
}

func TestSetRoutingClearsSessionMemory(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{gtRule(), {Match: "code", Group: "other"}}, "", false)
	if got := m.Classify("s1", gtBody); got.Group != "gt" {
		t.Fatalf("got %+v", got)
	}

	// Reordered: "code" now wins for the GT body too.
	m.SetRouting([]config.RoutingRule{{Match: "code", Group: "other"}, gtRule()}, "", false)
	if got := m.Classify("s1", plainBody); got.Group != "" {
		t.Errorf("after SetRouting the session still remembered %+v", got)
	}
	if got := m.Classify("s1", gtBody); got.Group != "other" {
		t.Errorf("re-classified = %+v, want other under the new order", got)
	}
}

func TestClassifyRemembersStrictness(t *testing.T) {
	m := mgr(t)
	m.SetRouting([]config.RoutingRule{{Match: "code/GT", Group: "gt", Strict: true}}, "", false)
	m.Classify("s1", gtBody)
	if got := m.Classify("s1", plainBody); !got.Strict {
		t.Errorf("remembered route %+v lost its strictness", got)
	}
}

func TestClassifyRemembersWithAffinityOff(t *testing.T) {
	m := New(nil, map[string]provider.Provider{"stub": &stubProvider{}}, Options{SessionAffinity: false})
	m.SetRouting([]config.RoutingRule{gtRule()}, "", false)
	m.Classify("s1", gtBody)
	if got := m.Classify("s1", plainBody); got.Group != "gt" {
		t.Errorf("with affinity off the session forgot its group: %+v", got)
	}
}

func TestClassifyWithNoRulesIsTheDefault(t *testing.T) {
	m := mgr(t)
	if got := m.Classify("s", gtBody); got != (Route{Rule: -1}) {
		t.Errorf("Classify with no routing configured = %+v, want the zero default", got)
	}
}

func TestSetGroup(t *testing.T) {
	m := mgr(t, acct("a", 0))
	if err := m.SetGroup("a", "gt"); err != nil {
		t.Fatal(err)
	}
	if a, _ := m.Get("a"); a.Group != "gt" {
		t.Errorf("Group = %q, want gt", a.Group)
	}
	if err := m.SetGroup("nope", "x"); err == nil {
		t.Error("an unknown account must be an error")
	}
}

func TestFromConfigCarriesTheGroup(t *testing.T) {
	c := acct("a", 0)
	c.Group = "gt"
	m := mgr(t, c)
	if a, _ := m.Get("a"); a.Group != "gt" {
		t.Errorf("Group = %q, want gt from config", a.Group)
	}
}
