package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// quotaAutoResetConfigID is the primary key of the single configuration row. The
// setting is cluster-wide, so a singleton row keeps every Home node reading the
// same values without a coordination protocol.
const quotaAutoResetConfigID = 1

// QuotaAutoResetConfigRecord persists the cluster-wide quota auto reset settings so
// they can be changed from the console without redeploying a config file.
type QuotaAutoResetConfigRecord struct {
	ID               int       `gorm:"column:id;primaryKey" json:"-"`
	Enabled          bool      `gorm:"column:enabled;not null" json:"enabled"`
	SpendInterval    string    `gorm:"column:spend_interval;not null" json:"spend_interval"`
	CollectInterval  string    `gorm:"column:collect_interval;not null" json:"collect_interval"`
	RuleAEnabled     bool      `gorm:"column:rule_a_enabled;not null" json:"rule_a_enabled"`
	ThresholdPercent float64   `gorm:"column:threshold_percent;not null" json:"threshold_percent"`
	WithinDays       float64   `gorm:"column:within_days;not null" json:"within_days"`
	RuleBEnabled     bool      `gorm:"column:rule_b_enabled;not null" json:"rule_b_enabled"`
	ExpiryWindow     string    `gorm:"column:expiry_window;not null" json:"expiry_window"`
	UpdatedAt        time.Time `gorm:"column:updated_at" json:"updated_at"`
	UpdatedBy        string    `gorm:"column:updated_by" json:"updated_by"`
}

func (QuotaAutoResetConfigRecord) TableName() string {
	return "quota_auto_reset_config"
}

// DefaultQuotaAutoResetConfigRecord returns the row used before an operator has
// saved one. Everything is off: spending a reset credit cannot be undone, so the
// feature never activates without a deliberate change.
func DefaultQuotaAutoResetConfigRecord() QuotaAutoResetConfigRecord {
	return QuotaAutoResetConfigRecord{
		ID:               quotaAutoResetConfigID,
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

// Validate rejects settings that would make the loops misbehave. Durations are
// stored as strings so the console can round-trip them unchanged, which means they
// have to be parsed here rather than trusted.
func (c QuotaAutoResetConfigRecord) Validate() error {
	if _, _, _, errDurations := c.Durations(); errDurations != nil {
		return errDurations
	}
	if c.ThresholdPercent < 0 || c.ThresholdPercent > 100 {
		return fmt.Errorf("threshold_percent must be between 0 and 100")
	}
	if c.WithinDays < 0 {
		return fmt.Errorf("within_days must not be negative")
	}
	return nil
}

// Durations parses the three configured intervals.
func (c QuotaAutoResetConfigRecord) Durations() (time.Duration, time.Duration, time.Duration, error) {
	spendInterval, errSpend := time.ParseDuration(strings.TrimSpace(c.SpendInterval))
	if errSpend != nil || spendInterval <= 0 {
		return 0, 0, 0, fmt.Errorf("spend_interval must be a positive duration")
	}
	collectInterval, errCollect := time.ParseDuration(strings.TrimSpace(c.CollectInterval))
	if errCollect != nil || collectInterval <= 0 {
		return 0, 0, 0, fmt.Errorf("collect_interval must be a positive duration")
	}
	expiryWindow, errExpiry := time.ParseDuration(strings.TrimSpace(c.ExpiryWindow))
	if errExpiry != nil || expiryWindow <= 0 {
		return 0, 0, 0, fmt.Errorf("expiry_window must be a positive duration")
	}
	return spendInterval, collectInterval, expiryWindow, nil
}

// GetQuotaAutoResetConfig reads the settings, returning defaults when no row exists
// yet so callers never have to special-case a fresh install.
func (r *Repository) GetQuotaAutoResetConfig(ctx context.Context) (QuotaAutoResetConfigRecord, error) {
	if r == nil || r.db == nil {
		return QuotaAutoResetConfigRecord{}, fmt.Errorf("repository is unavailable")
	}
	var record QuotaAutoResetConfigRecord
	errFind := r.db.WithContext(ctx).First(&record, "id = ?", quotaAutoResetConfigID).Error
	if errors.Is(errFind, gorm.ErrRecordNotFound) {
		return DefaultQuotaAutoResetConfigRecord(), nil
	}
	if errFind != nil {
		return QuotaAutoResetConfigRecord{}, errFind
	}
	return record, nil
}

// SaveQuotaAutoResetConfig validates and upserts the settings.
func (r *Repository) SaveQuotaAutoResetConfig(ctx context.Context, input QuotaAutoResetConfigRecord, updatedBy string) (QuotaAutoResetConfigRecord, error) {
	if r == nil || r.db == nil {
		return QuotaAutoResetConfigRecord{}, fmt.Errorf("repository is unavailable")
	}
	if errValidate := input.Validate(); errValidate != nil {
		return QuotaAutoResetConfigRecord{}, errValidate
	}
	input.ID = quotaAutoResetConfigID
	input.UpdatedAt = time.Now().UTC()
	input.UpdatedBy = strings.TrimSpace(updatedBy)
	errSave := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"enabled", "spend_interval", "collect_interval", "rule_a_enabled",
			"threshold_percent", "within_days", "rule_b_enabled", "expiry_window",
			"updated_at", "updated_by",
		}),
	}).Create(&input).Error
	if errSave != nil {
		return QuotaAutoResetConfigRecord{}, errSave
	}
	return input, nil
}
