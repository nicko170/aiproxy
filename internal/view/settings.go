package view

import (
	"fmt"
	"path"
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
	return nil
}
