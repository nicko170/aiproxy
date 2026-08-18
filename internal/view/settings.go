package view

import (
	"fmt"
	"path"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// Validate rejects a negative or absurd value before UpdateSettings persists
// anything: a setting that fails silently and is then written is worse than
// refusing it, since a bad value on disk survives a restart.
func (s Settings) Validate() error {
	if s.SwitchThreshold <= 0 || s.SwitchThreshold > 1 {
		return fmt.Errorf("switchThreshold must be in (0, 1], got %v", s.SwitchThreshold)
	}
	if s.RetryBudgetMS <= 0 {
		return fmt.Errorf("retryBudgetMs must be positive, got %d", s.RetryBudgetMS)
	}
	if s.InlineAbsorbMaxMS < 0 {
		return fmt.Errorf("inlineAbsorbMaxMs must not be negative, got %d", s.InlineAbsorbMaxMS)
	}
	if s.HeaderTimeoutMS <= 0 {
		return fmt.Errorf("headerTimeoutMs must be positive, got %d", s.HeaderTimeoutMS)
	}
	if s.BodyIdleMS <= 0 {
		return fmt.Errorf("bodyIdleMs must be positive, got %d", s.BodyIdleMS)
	}
	if s.QuotaProbeIntervalSeconds <= 0 {
		return fmt.Errorf("quotaProbeIntervalSeconds must be positive, got %d", s.QuotaProbeIntervalSeconds)
	}
	if s.MetricsRetentionDays <= 0 {
		return fmt.Errorf("metricsRetentionDays must be positive, got %d", s.MetricsRetentionDays)
	}
	if s.UpdateCheckIntervalHours <= 0 {
		return fmt.Errorf("updateCheckIntervalHours must be positive, got %d", s.UpdateCheckIntervalHours)
	}
	for _, pattern := range s.BlockedModels {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("blockedModels pattern %q is not a valid glob: %w", pattern, err)
		}
	}
	// Empty is accepted here as the documented default (closed /
	// passthrough): config.loadLocked fills in a non-empty value on any real
	// read-modify-write, so only a from-scratch Settings literal (as several
	// in this package's own tests build) ever reaches Validate with "" — and
	// that must not be rejected as if it were a typo of an unknown mode.
	if s.PrivacyOnScanFailure != "" {
		if _, err := privacy.ParseFailureMode(s.PrivacyOnScanFailure); err != nil {
			return err
		}
	}
	// Zero is accepted and normalized to the default by UpdateSettings, so a
	// caller that never mentions the field is not rejected. Negative is not a
	// longer timeout, it is a scan that expires before it starts.
	if s.PrivacyScanTimeoutMS < 0 {
		return fmt.Errorf("privacyScanTimeoutMs must not be negative, got %d", s.PrivacyScanTimeoutMS)
	}
	switch s.PrivacyOnUnresolved {
	case "", "passthrough", "error":
	default:
		return fmt.Errorf("privacyOnUnresolved must be \"passthrough\" or \"error\", got %q", s.PrivacyOnUnresolved)
	}
	return nil
}
