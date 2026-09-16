package cluster

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newQuotaAutoResetTestRepository(t *testing.T) *Repository {
	t.Helper()
	db, errOpen := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if errOpen != nil {
		t.Fatalf("open sqlite db: %v", errOpen)
	}
	if errMigrate := db.AutoMigrate(&QuotaAutoResetConfigRecord{}); errMigrate != nil {
		t.Fatalf("AutoMigrate() error = %v", errMigrate)
	}
	return &Repository{db: db}
}

// A fresh install has no row; callers must still get a usable, disabled config.
func TestGetQuotaAutoResetConfigReturnsDefaultsWhenUnset(t *testing.T) {
	repo := newQuotaAutoResetTestRepository(t)
	record, errGet := repo.GetQuotaAutoResetConfig(context.Background())
	if errGet != nil {
		t.Fatalf("GetQuotaAutoResetConfig() error = %v", errGet)
	}
	if record != DefaultQuotaAutoResetConfigRecord() {
		t.Fatalf("GetQuotaAutoResetConfig() = %#v, want defaults", record)
	}
	if record.Enabled || record.RuleAEnabled || record.RuleBEnabled {
		t.Fatal("default config must not enable any spending")
	}
}

func TestSaveQuotaAutoResetConfigRoundTrips(t *testing.T) {
	repo := newQuotaAutoResetTestRepository(t)
	ctx := context.Background()
	input := DefaultQuotaAutoResetConfigRecord()
	input.Enabled = true
	input.RuleAEnabled = true
	input.ThresholdPercent = 15
	input.ExpiryWindow = "12h"

	saved, errSave := repo.SaveQuotaAutoResetConfig(ctx, input, "loc.nguyen")
	if errSave != nil {
		t.Fatalf("SaveQuotaAutoResetConfig() error = %v", errSave)
	}
	if saved.UpdatedBy != "loc.nguyen" || saved.UpdatedAt.IsZero() {
		t.Fatalf("SaveQuotaAutoResetConfig() did not stamp the audit fields: %#v", saved)
	}

	loaded, errGet := repo.GetQuotaAutoResetConfig(ctx)
	if errGet != nil {
		t.Fatalf("GetQuotaAutoResetConfig() error = %v", errGet)
	}
	if !loaded.Enabled || !loaded.RuleAEnabled || loaded.ThresholdPercent != 15 || loaded.ExpiryWindow != "12h" {
		t.Fatalf("GetQuotaAutoResetConfig() = %#v, want the saved values", loaded)
	}
}

// The row is a singleton: saving twice must update in place, never accumulate rows
// that different nodes could read inconsistently.
func TestSaveQuotaAutoResetConfigKeepsSingleRow(t *testing.T) {
	repo := newQuotaAutoResetTestRepository(t)
	ctx := context.Background()
	first := DefaultQuotaAutoResetConfigRecord()
	first.ThresholdPercent = 10
	if _, errSave := repo.SaveQuotaAutoResetConfig(ctx, first, "a"); errSave != nil {
		t.Fatalf("first SaveQuotaAutoResetConfig() error = %v", errSave)
	}
	second := DefaultQuotaAutoResetConfigRecord()
	second.ThresholdPercent = 20
	if _, errSave := repo.SaveQuotaAutoResetConfig(ctx, second, "b"); errSave != nil {
		t.Fatalf("second SaveQuotaAutoResetConfig() error = %v", errSave)
	}
	var count int64
	if errCount := repo.db.Model(&QuotaAutoResetConfigRecord{}).Count(&count).Error; errCount != nil {
		t.Fatalf("count rows: %v", errCount)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
	loaded, _ := repo.GetQuotaAutoResetConfig(ctx)
	if loaded.ThresholdPercent != 20 || loaded.UpdatedBy != "b" {
		t.Fatalf("GetQuotaAutoResetConfig() = %#v, want the second save", loaded)
	}
}

func TestSaveQuotaAutoResetConfigRejectsInvalidInput(t *testing.T) {
	repo := newQuotaAutoResetTestRepository(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*QuotaAutoResetConfigRecord)
	}{
		{"bad spend interval", func(c *QuotaAutoResetConfigRecord) { c.SpendInterval = "soon" }},
		{"zero collect interval", func(c *QuotaAutoResetConfigRecord) { c.CollectInterval = "0s" }},
		{"negative expiry window", func(c *QuotaAutoResetConfigRecord) { c.ExpiryWindow = "-1h" }},
		{"threshold above 100", func(c *QuotaAutoResetConfigRecord) { c.ThresholdPercent = 101 }},
		{"negative threshold", func(c *QuotaAutoResetConfigRecord) { c.ThresholdPercent = -1 }},
		{"negative within days", func(c *QuotaAutoResetConfigRecord) { c.WithinDays = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := DefaultQuotaAutoResetConfigRecord()
			tc.mutate(&input)
			if _, errSave := repo.SaveQuotaAutoResetConfig(ctx, input, ""); errSave == nil {
				t.Fatal("SaveQuotaAutoResetConfig() accepted an invalid config")
			}
		})
	}
}
