package view

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func loadSettings(t *testing.T, h *testHarness) Settings {
	t.Helper()
	s, err := h.local.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	return s
}

func seedRouting(t *testing.T, h *testHarness) {
	t.Helper()
	s := loadSettings(t, h)
	s.RoutingRules = []RoutingRule{{Match: "GT", Group: "gt"}}
	s.DefaultGroup = strp("personal")
	s.DefaultStrict = boolp(true)
	if _, err := h.local.UpdateSettings(context.Background(), s); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestUpdateSettingsRoutingIsLiveAndAppliesToTheManager(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	s := loadSettings(t, h)
	s.RoutingRules = []RoutingRule{{Match: "GT", Group: "gt"}}
	s.DefaultGroup = strp("personal")

	applied, err := h.local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"routingRules", "defaultGroup"} {
		if !slices.Contains(applied.Live, f) {
			t.Errorf("Applied.Live = %v, want it to contain %s", applied.Live, f)
		}
	}
	cfg, _ := h.cs.Load()
	if len(cfg.Routing.Rules) != 1 || cfg.Routing.Rules[0].Group != "gt" || cfg.Routing.DefaultGroup != "personal" {
		t.Errorf("persisted routing = %+v", cfg.Routing)
	}
	if got := h.mgr.Classify("", []byte("GT")); got.Group != "gt" {
		t.Errorf("Manager did not pick up the rules: %+v", got)
	}
	if got := h.mgr.Classify("", []byte("x")); got.Group != "personal" {
		t.Errorf("Manager did not pick up the default: %+v", got)
	}
}

func TestSettingsReturnsRoutingFieldsNonNil(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	s := loadSettings(t, h)
	if s.RoutingRules == nil || s.DefaultGroup == nil || s.DefaultStrict == nil {
		t.Errorf("Settings() = rules %v group %v strict %v; all must be present", s.RoutingRules, s.DefaultGroup, s.DefaultStrict)
	}
}

func TestUpdateSettingsOmittedRoutingFieldsAreKept(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	seedRouting(t, h)

	s := loadSettings(t, h)
	s.RoutingRules, s.DefaultGroup, s.DefaultStrict = nil, nil, nil
	applied, err := h.local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := h.cs.Load()
	if len(cfg.Routing.Rules) != 1 || cfg.Routing.DefaultGroup != "personal" || !cfg.Routing.DefaultStrict {
		t.Errorf("omitted fields were not kept: %+v", cfg.Routing)
	}
	for _, f := range []string{"routingRules", "defaultGroup", "defaultStrict"} {
		if slices.Contains(applied.Live, f) || slices.Contains(applied.NeedsRestart, f) {
			t.Errorf("Applied mentions omitted field %s: %+v", f, applied)
		}
	}
}

func TestUpdateSettingsNullRoutingRulesIsAbsent(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	seedRouting(t, h)

	raw, _ := json.Marshal(loadSettings(t, h))
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["routingRules"] = nil
	raw, _ = json.Marshal(m)
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if _, err := h.local.UpdateSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := h.cs.Load(); len(cfg.Routing.Rules) != 1 {
		t.Errorf(`"routingRules": null cleared the rules: %+v`, cfg.Routing.Rules)
	}
}

func TestUpdateSettingsExplicitEmptyRoutingRulesClears(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	seedRouting(t, h)
	s := loadSettings(t, h)
	s.RoutingRules = []RoutingRule{}
	if _, err := h.local.UpdateSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := h.cs.Load(); len(cfg.Routing.Rules) != 0 {
		t.Errorf("explicit [] did not clear: %+v", cfg.Routing.Rules)
	}
}

func TestUpdateSettingsTrimsPastedRules(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	s := loadSettings(t, h)
	s.RoutingRules = []RoutingRule{{Match: "  code/GT\n", Group: " gt "}}
	s.DefaultGroup = strp(" personal ")
	if _, err := h.local.UpdateSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	cfg, _ := h.cs.Load()
	if r := cfg.Routing.Rules[0]; r.Match != "code/GT" || r.Group != "gt" || cfg.Routing.DefaultGroup != "personal" {
		t.Errorf("not trimmed: %+v default %q", r, cfg.Routing.DefaultGroup)
	}
}

func TestUpdateSettingsRejectsABlankMatch(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	s := loadSettings(t, h)
	s.RoutingRules = []RoutingRule{{Match: "   ", Group: "gt"}}
	if _, err := h.local.UpdateSettings(context.Background(), s); err == nil {
		t.Fatal("a blank match must be refused")
	}
	if cfg, _ := h.cs.Load(); len(cfg.Routing.Rules) != 0 {
		t.Errorf("a refused update persisted rules: %+v", cfg.Routing.Rules)
	}
}

func TestUpdateSettingsUnchangedRoutingDoesNotClearSessions(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	seedRouting(t, h)
	h.mgr.Classify("s", []byte("GT"))

	s := loadSettings(t, h)
	s.SwitchThreshold = 0.5
	if _, err := h.local.UpdateSettings(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := h.mgr.Classify("s", []byte("x")); got.Group != "gt" {
		t.Errorf("an unrelated save cleared session memory: %+v", got)
	}
}

func TestSetAccountGroupPersistsAndApplies(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	if err := h.local.SetAccountGroup(context.Background(), "a", " gt "); err != nil {
		t.Fatal(err)
	}
	cfg, _ := h.cs.Load()
	if cfg.Accounts[0].Group != "gt" {
		t.Errorf("persisted group = %q", cfg.Accounts[0].Group)
	}
	accts, _ := h.local.Accounts(context.Background())
	if accts[0].Group != "gt" {
		t.Errorf("Accounts() group = %q", accts[0].Group)
	}
	if err := h.local.SetAccountGroup(context.Background(), "nope", "x"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("unknown account: err = %v", err)
	}
}
