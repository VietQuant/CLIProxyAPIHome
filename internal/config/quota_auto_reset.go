package config

import (
	"fmt"
	"time"
)

// QuotaAutoResetConfig controls automatic spending of provider-issued quota reset
// credits.
//
// Spending a credit is irreversible, so this is disabled by default and both rules
// are opt-in. Rule A trades a credit for an immediate recovery of an exhausted
// credential; rule B spends a credit that would otherwise expire unused.
type QuotaAutoResetConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// SpendInterval is how often both rules are evaluated against stored snapshots.
	SpendInterval string `yaml:"spend-interval" json:"spend-interval"`
	// CollectInterval is how often a forced quota collection runs. The scheduled
	// collector skips credentials without recent traffic, so an exhausted credential
	// would otherwise keep a frozen snapshot and never become visible to either rule.
	CollectInterval string `yaml:"collect-interval" json:"collect-interval"`

	// RuleAEnabled spends a credit when a credential is out of quota.
	RuleAEnabled bool `yaml:"rule-a-enabled" json:"rule-a-enabled"`
	// ThresholdPercent is the remaining percentage at or below which a window counts
	// as exhausted. 0 means only when fully depleted.
	ThresholdPercent float64 `yaml:"threshold-percent" json:"threshold-percent"`
	// WithinDays suppresses rule A when the window resets naturally within this many
	// days, because a credit then buys very little.
	WithinDays float64 `yaml:"within-days" json:"within-days"`

	// RuleBEnabled spends a credit that is close to expiring, regardless of quota.
	RuleBEnabled bool `yaml:"rule-b-enabled" json:"rule-b-enabled"`
	// ExpiryWindow is how far ahead of a credit's expiry rule B fires.
	ExpiryWindow string `yaml:"expiry-window" json:"expiry-window"`
}

// DefaultQuotaAutoResetConfig returns the auto reset defaults. Disabled, with both
// rules off, so enabling the feature never spends a credit by surprise.
func DefaultQuotaAutoResetConfig() QuotaAutoResetConfig {
	return QuotaAutoResetConfig{
		Enabled:          false,
		SpendInterval:    "1m",
		CollectInterval:  "24h",
		RuleAEnabled:     false,
		ThresholdPercent: 0,
		WithinDays:       1,
		RuleBEnabled:     false,
		ExpiryWindow:     "6h",
	}
}

// Durations parses and validates the auto reset durations.
func (c QuotaAutoResetConfig) Durations() (time.Duration, time.Duration, time.Duration, error) {
	spendInterval, errSpend := time.ParseDuration(c.SpendInterval)
	if errSpend != nil || spendInterval <= 0 {
		return 0, 0, 0, fmt.Errorf("quota-auto-reset.spend-interval must be positive")
	}
	collectInterval, errCollect := time.ParseDuration(c.CollectInterval)
	if errCollect != nil || collectInterval <= 0 {
		return 0, 0, 0, fmt.Errorf("quota-auto-reset.collect-interval must be positive")
	}
	expiryWindow, errExpiry := time.ParseDuration(c.ExpiryWindow)
	if errExpiry != nil || expiryWindow <= 0 {
		return 0, 0, 0, fmt.Errorf("quota-auto-reset.expiry-window must be positive")
	}
	return spendInterval, collectInterval, expiryWindow, nil
}

// Validate verifies the auto reset bounds.
func (c QuotaAutoResetConfig) Validate() error {
	if _, _, _, errDurations := c.Durations(); errDurations != nil {
		return errDurations
	}
	if c.ThresholdPercent < 0 || c.ThresholdPercent > 100 {
		return fmt.Errorf("quota-auto-reset.threshold-percent must be between 0 and 100")
	}
	if c.WithinDays < 0 {
		return fmt.Errorf("quota-auto-reset.within-days must not be negative")
	}
	return nil
}
