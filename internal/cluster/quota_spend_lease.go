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

// QuotaSpendLeaseRecord serializes AutoReset's credit-consume call across Home
// nodes. It is a dedicated, fork-only table rather than columns on the
// upstream-shared quota_snapshot table: quota_snapshot's claim/completion paths
// (ClaimQuotaProbe/ClaimEligibleQuotaProbe/ForceClaimEligibleQuotaProbe) stamp
// collection_status="collecting" as part of claiming, and a spend lease that
// reused that table previously left the row stuck in "collecting" forever,
// because spend()'s own follow-up probe claim always lost the race against the
// lease spend() had just set on the very same row.
type QuotaSpendLeaseRecord struct {
	CredentialID string     `gorm:"column:credential_id;primaryKey;size:128"`
	Owner        string     `gorm:"column:owner;size:256"`
	ExpiresAt    *time.Time `gorm:"column:expires_at;index"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (QuotaSpendLeaseRecord) TableName() string { return "quota_spend_lease" }

// ClaimQuotaSpendLease claims the spend lease for a credential. The row is
// created on first use so a credential need not have been probed yet to be
// spent on. An unexpired lease held by any owner (including this one) wins.
func (r *Repository) ClaimQuotaSpendLease(ctx context.Context, credentialID string, owner string, now time.Time, leaseDuration time.Duration) (bool, error) {
	credentialID = strings.TrimSpace(credentialID)
	owner = strings.TrimSpace(owner)
	if credentialID == "" || owner == "" {
		return false, fmt.Errorf("quota spend lease credential and owner are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	claimed := false
	errTransaction := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var record QuotaSpendLeaseRecord
		errFind := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&record, "credential_id = ?", credentialID).Error
		if errFind != nil && !errors.Is(errFind, gorm.ErrRecordNotFound) {
			return errFind
		}
		leaseExpiresAt := now.Add(leaseDuration)
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			record = QuotaSpendLeaseRecord{
				CredentialID: credentialID, Owner: owner, ExpiresAt: &leaseExpiresAt,
				CreatedAt: now, UpdatedAt: now,
			}
			if errCreate := tx.Create(&record).Error; errCreate != nil {
				return errCreate
			}
			claimed = true
			return nil
		}
		if record.ExpiresAt != nil && record.ExpiresAt.After(now) {
			return nil
		}
		updates := map[string]any{
			"owner": owner, "expires_at": leaseExpiresAt, "updated_at": now,
		}
		if errUpdate := tx.Model(&QuotaSpendLeaseRecord{}).Where("credential_id = ?", credentialID).Updates(updates).Error; errUpdate != nil {
			return errUpdate
		}
		claimed = true
		return nil
	})
	return claimed, errTransaction
}
