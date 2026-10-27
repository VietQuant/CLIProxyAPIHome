package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// billingBackfillUsageColumns lists the usage columns the charge computation
// reads. The raw payload is deliberately left out: pricing never reads it and it
// dominates the row size. VerifyBillingChargeRepricing proves on real charges
// that this column set reproduces what Home stored.
const billingBackfillUsageColumns = `id, timestamp, provider, model, executor_type, service_tier, response_service_tier,
	api_key, failed, input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens,
	cache_read_tokens_present, cache_creation_tokens, total_tokens, token_accounting_version,
	token_accounting_quality, accounting_total_tokens, accounting_input_tokens, uncached_input_tokens,
	accounting_cache_read_tokens, accounting_cache_write_tokens, accounting_output_tokens,
	non_reasoning_output_tokens, accounting_reasoning_tokens, unclassified_tokens`

// ErrBillingBackfillUsersPresent stops a backfill when any user or user-owned
// client key exists. Repricing a charge never touches balances, but the period
// limits of a user sum billing_charge.amount, so priced history could start
// blocking that user's requests.
var ErrBillingBackfillUsersPresent = errors.New("billing backfill refused: users or user-owned client keys exist")

// BillingZeroChargeBackfillOptions selects the zero charges to reprice.
type BillingZeroChargeBackfillOptions struct {
	// From and To bound billing_charge.created_at as [From, To).
	From time.Time
	To   time.Time
	// BatchSize is the number of charges read and written per transaction.
	BatchSize int
	// Apply writes the new prices. Without it the run only reports.
	Apply bool
	// Backup receives one JSON line per charge with its original values before
	// the batch that changes it is written. Required when Apply is set.
	Backup io.Writer
	// Location groups the report by local day.
	Location *time.Location
	// OnBatch is called after every batch with the running totals.
	OnBatch func(BillingZeroChargeBackfillResult)
}

// BillingZeroChargeBackfillGroup aggregates repriced charges per day and model.
type BillingZeroChargeBackfillGroup struct {
	Day      string  `json:"day"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Rule     string  `json:"rule"`
	Rows     int64   `json:"rows"`
	Amount   float64 `json:"amount"`
}

// BillingZeroChargeBackfillResult reports a dry run or an applied backfill.
type BillingZeroChargeBackfillResult struct {
	Scanned       int64                            `json:"scanned"`
	Priced        int64                            `json:"priced"`
	StillUnpriced int64                            `json:"still_unpriced"`
	Updated       int64                            `json:"updated"`
	Amount        float64                          `json:"amount"`
	Groups        []BillingZeroChargeBackfillGroup `json:"groups"`
	UnpricedBy    map[string]int64                 `json:"unpriced_by_model"`
}

type billingBackfillCharge struct {
	ID        string    `gorm:"column:id"`
	UsageID   uint      `gorm:"column:usage_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
	// Original values, kept for the backup.
	Amount           float64 `gorm:"column:amount"`
	MatchedPriceRule string  `gorm:"column:matched_price_rule"`
	PriceSnapshot    string  `gorm:"column:price_snapshot"`
}

type billingBackfillPriced struct {
	charge   billingBackfillCharge
	amount   float64
	rule     string
	snapshot JSONB
	provider string
	model    string
}

// billingBackfillPricer calls billingPriceSnapshotForUsage once per distinct
// lookup instead of once per charge. The lookup depends only on the provider,
// model, requested and response service tiers, and on which min_input_tokens
// thresholds the input reaches, so caching on exactly those keys returns the
// same price and snapshot Home would compute for every charge.
type billingBackfillPricer struct {
	db         *gorm.DB
	thresholds map[string][]int64
	cache      map[string]billingBackfillPriceResult
}

type billingBackfillPriceResult struct {
	price    *BillingModelPriceRecord
	snapshot BillingPriceSnapshot
}

func newBillingBackfillPricer(ctx context.Context, db *gorm.DB) (*billingBackfillPricer, error) {
	var rows []struct {
		Provider       string `gorm:"column:provider"`
		Model          string `gorm:"column:model"`
		MinInputTokens int64  `gorm:"column:min_input_tokens"`
	}
	if errFind := db.WithContext(ctx).Model(&BillingModelPriceRecord{}).
		Select("provider, model, min_input_tokens").
		Where("enabled = ?", true).
		Scan(&rows).Error; errFind != nil {
		return nil, errFind
	}
	thresholds := make(map[string][]int64)
	for _, row := range rows {
		key := billingBackfillModelKey(row.Provider, row.Model)
		thresholds[key] = append(thresholds[key], row.MinInputTokens)
	}
	for key := range thresholds {
		sort.Slice(thresholds[key], func(i, j int) bool { return thresholds[key][i] < thresholds[key][j] })
	}
	return &billingBackfillPricer{db: db, thresholds: thresholds, cache: make(map[string]billingBackfillPriceResult)}, nil
}

func billingBackfillModelKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimSpace(model)
}

func (p *billingBackfillPricer) lookup(ctx context.Context, usage *UsageRecord) (billingBackfillPriceResult, error) {
	modelKey := billingBackfillModelKey(usage.Provider, usage.Model)
	reached := 0
	for _, threshold := range p.thresholds[modelKey] {
		if threshold <= usage.InputTokens {
			reached++
		}
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", modelKey, usage.ServiceTier, usage.ResponseServiceTier, reached)
	if cached, ok := p.cache[key]; ok {
		return cached, nil
	}
	price, snapshot, errSnapshot := billingPriceSnapshotForUsage(ctx, p.db.WithContext(ctx), usage)
	if errSnapshot != nil {
		return billingBackfillPriceResult{}, errSnapshot
	}
	result := billingBackfillPriceResult{price: price, snapshot: snapshot}
	p.cache[key] = result
	return result, nil
}

func (p *billingBackfillPricer) price(ctx context.Context, usage *UsageRecord) (*BillingModelPriceRecord, float64, string, JSONB, error) {
	result, errLookup := p.lookup(ctx, usage)
	if errLookup != nil {
		return nil, 0, "", nil, errLookup
	}
	snapshot, errJSON := billingJSONB(result.snapshot)
	if errJSON != nil {
		return nil, 0, "", nil, errJSON
	}
	return result.price, billingChargeAmount(usage, result.snapshot), billingMatchedPriceRule(result.price), snapshot, nil
}

func billingBackfillEnsureNoUsers(ctx context.Context, db *gorm.DB) error {
	var users, ownedKeys int64
	if errUsers := db.WithContext(ctx).Model(&UserRecord{}).Count(&users).Error; errUsers != nil {
		return errUsers
	}
	if errKeys := db.WithContext(ctx).Model(&APIKeyRecord{}).Where("user_id IS NOT NULL").Count(&ownedKeys).Error; errKeys != nil {
		return errKeys
	}
	if users > 0 || ownedKeys > 0 {
		return fmt.Errorf("%w (users=%d, user-owned keys=%d)", ErrBillingBackfillUsersPresent, users, ownedKeys)
	}
	return nil
}

func billingBackfillLoadUsages(ctx context.Context, db *gorm.DB, ids []uint) (map[uint]*UsageRecord, error) {
	out := make(map[uint]*UsageRecord, len(ids))
	for start := 0; start < len(ids); start += 1000 {
		end := start + 1000
		if end > len(ids) {
			end = len(ids)
		}
		var usages []UsageRecord
		if errFind := db.WithContext(ctx).Select(billingBackfillUsageColumns).Where("id IN ?", ids[start:end]).Find(&usages).Error; errFind != nil {
			return nil, errFind
		}
		for index := range usages {
			out[usages[index].ID] = &usages[index]
		}
	}
	return out, nil
}

// BackfillZeroBillingCharges reprices charges written as default:zero, using the
// same price lookup and amount formula Home applies to a new request. It only
// touches charges without a user, never changes balances, and only rewrites
// amount, matched_price_rule and price_snapshot.
func (r *Repository) BackfillZeroBillingCharges(ctx context.Context, opts BillingZeroChargeBackfillOptions) (BillingZeroChargeBackfillResult, error) {
	result := BillingZeroChargeBackfillResult{UnpricedBy: map[string]int64{}}
	db, errDB := r.database()
	if errDB != nil {
		return result, errDB
	}
	ctx = contextOrBackground(ctx)
	if !opts.To.After(opts.From) {
		return result, fmt.Errorf("billing backfill: empty range [%s, %s)", opts.From, opts.To)
	}
	if opts.Apply && opts.Backup == nil {
		return result, fmt.Errorf("billing backfill: a backup writer is required to apply")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 20000
	}
	location := opts.Location
	if location == nil {
		location = time.UTC
	}
	if errGuard := billingBackfillEnsureNoUsers(ctx, db); errGuard != nil {
		return result, errGuard
	}
	pricer, errPricer := newBillingBackfillPricer(ctx, db)
	if errPricer != nil {
		return result, errPricer
	}

	groups := map[string]*BillingZeroChargeBackfillGroup{}
	cursorTime := opts.From
	cursorID := ""
	for {
		var charges []billingBackfillCharge
		if errFind := db.WithContext(ctx).Model(&BillingChargeRecord{}).
			Select("id, usage_id, created_at, amount, matched_price_rule, price_snapshot").
			Where("matched_price_rule = ? AND amount = ? AND user_id IS NULL", "default:zero", 0).
			Where("created_at >= ? AND created_at < ?", opts.From, opts.To).
			Where("(created_at > ? OR (created_at = ? AND id > ?))", cursorTime, cursorTime, cursorID).
			Order("created_at ASC, id ASC").
			Limit(opts.BatchSize).
			Scan(&charges).Error; errFind != nil {
			return result, errFind
		}
		if len(charges) == 0 {
			break
		}
		last := charges[len(charges)-1]
		cursorTime, cursorID = last.CreatedAt, last.ID

		usageIDs := make([]uint, 0, len(charges))
		for _, charge := range charges {
			usageIDs = append(usageIDs, charge.UsageID)
		}
		usages, errUsages := billingBackfillLoadUsages(ctx, db, usageIDs)
		if errUsages != nil {
			return result, errUsages
		}

		priced := make([]billingBackfillPriced, 0, len(charges))
		for _, charge := range charges {
			result.Scanned++
			usage := usages[charge.UsageID]
			if usage == nil {
				return result, fmt.Errorf("billing backfill: charge %s references missing usage %d", charge.ID, charge.UsageID)
			}
			price, amount, rule, snapshot, errPrice := pricer.price(ctx, usage)
			if errPrice != nil {
				return result, errPrice
			}
			if price == nil {
				result.StillUnpriced++
				result.UnpricedBy[strings.ToLower(strings.TrimSpace(usage.Provider))+"/"+strings.TrimSpace(usage.Model)]++
				continue
			}
			result.Priced++
			result.Amount += amount
			day := charge.CreatedAt.In(location).Format("2006-01-02")
			groupKey := strings.Join([]string{day, price.Provider, price.Model, rule}, "\x00")
			group := groups[groupKey]
			if group == nil {
				group = &BillingZeroChargeBackfillGroup{Day: day, Provider: price.Provider, Model: price.Model, Rule: rule}
				groups[groupKey] = group
			}
			group.Rows++
			group.Amount += amount
			priced = append(priced, billingBackfillPriced{charge: charge, amount: amount, rule: rule, snapshot: snapshot, provider: price.Provider, model: price.Model})
		}

		if opts.Apply && len(priced) > 0 {
			if errBackup := billingBackfillWriteBackup(opts.Backup, priced); errBackup != nil {
				return result, errBackup
			}
			updated, errUpdate := billingBackfillUpdate(ctx, db, priced)
			if errUpdate != nil {
				return result, errUpdate
			}
			result.Updated += updated
			if updated != int64(len(priced)) {
				return result, fmt.Errorf("billing backfill: batch ending at charge %s updated %d of %d rows; stopping", last.ID, updated, len(priced))
			}
		}
		result.Groups = billingBackfillSortedGroups(groups)
		if opts.OnBatch != nil {
			opts.OnBatch(result)
		}
		if len(charges) < opts.BatchSize {
			break
		}
	}
	result.Groups = billingBackfillSortedGroups(groups)
	return result, nil
}

func billingBackfillSortedGroups(groups map[string]*BillingZeroChargeBackfillGroup) []BillingZeroChargeBackfillGroup {
	out := make([]BillingZeroChargeBackfillGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day < out[j].Day
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

func billingBackfillWriteBackup(w io.Writer, priced []billingBackfillPriced) error {
	encoder := json.NewEncoder(w)
	for _, item := range priced {
		if errEncode := encoder.Encode(map[string]any{
			"id":                 item.charge.ID,
			"amount":             item.charge.Amount,
			"matched_price_rule": item.charge.MatchedPriceRule,
			"price_snapshot":     json.RawMessage(billingBackfillRawJSON(item.charge.PriceSnapshot)),
		}); errEncode != nil {
			return errEncode
		}
	}
	if syncer, ok := w.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}

func billingBackfillRawJSON(value string) string {
	if strings.TrimSpace(value) == "" || !json.Valid([]byte(value)) {
		return "null"
	}
	return value
}

// billingBackfillUpdate rewrites one batch in a single transaction. The WHERE
// clause repeats the selection guard so a charge that changed since it was read
// is left alone, and the caller stops if any row was skipped.
func billingBackfillUpdate(ctx context.Context, db *gorm.DB, priced []billingBackfillPriced) (int64, error) {
	var updated int64
	errTx := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			for start := 0; start < len(priced); start += 1000 {
				end := start + 1000
				if end > len(priced) {
					end = len(priced)
				}
				values := make([]string, 0, end-start)
				args := make([]any, 0, (end-start)*4)
				for _, item := range priced[start:end] {
					// billing_charge.amount is an unbounded numeric. Home's insert stores
					// the float64's shortest exact decimal, which is what pgx writes for a
					// float parameter. Going through double precision first would round to
					// 15 significant digits on the way into numeric (0.6606000000000001
					// became 0.6606), so send that same decimal text and cast to numeric.
					values = append(values, "(?, CAST(? AS numeric), ?, CAST(? AS jsonb))")
					args = append(args, item.charge.ID, strconv.FormatFloat(item.amount, 'f', -1, 64), item.rule, string(item.snapshot))
				}
				statement := `UPDATE "billing_charge" AS c
					SET amount = v.amount, matched_price_rule = v.rule, price_snapshot = v.snapshot
					FROM (VALUES ` + strings.Join(values, ", ") + `) AS v(id, amount, rule, snapshot)
					WHERE c.id = v.id AND c.matched_price_rule = 'default:zero' AND c.amount = 0 AND c.user_id IS NULL`
				exec := tx.Exec(statement, args...)
				if exec.Error != nil {
					return exec.Error
				}
				updated += exec.RowsAffected
			}
			return nil
		}
		for _, item := range priced {
			exec := tx.Model(&BillingChargeRecord{}).
				Where("id = ? AND matched_price_rule = ? AND amount = ? AND user_id IS NULL", item.charge.ID, "default:zero", 0).
				Updates(map[string]any{"amount": item.amount, "matched_price_rule": item.rule, "price_snapshot": item.snapshot})
			if exec.Error != nil {
				return exec.Error
			}
			updated += exec.RowsAffected
		}
		return nil
	})
	return updated, errTx
}

// BillingRepricingCheck reports how the backfill pricing path compares with
// charges Home priced itself.
type BillingRepricingCheck struct {
	Checked    int64    `json:"checked"`
	Matched    int64    `json:"matched"`
	Mismatched int64    `json:"mismatched"`
	Examples   []string `json:"examples,omitempty"`
}

// VerifyBillingChargeRepricing reprices charges that Home already priced and
// compares amount, rule and snapshot with what Home stored. A backfill is only
// trustworthy if this reports zero mismatches on the current price table.
func (r *Repository) VerifyBillingChargeRepricing(ctx context.Context, from, to time.Time, limit int) (BillingRepricingCheck, error) {
	check := BillingRepricingCheck{}
	db, errDB := r.database()
	if errDB != nil {
		return check, errDB
	}
	ctx = contextOrBackground(ctx)
	pricer, errPricer := newBillingBackfillPricer(ctx, db)
	if errPricer != nil {
		return check, errPricer
	}
	var charges []billingBackfillCharge
	if errFind := db.WithContext(ctx).Model(&BillingChargeRecord{}).
		Select("id, usage_id, created_at, amount, matched_price_rule, price_snapshot").
		Where("matched_price_rule <> ? AND user_id IS NULL", "default:zero").
		Where("created_at >= ? AND created_at < ?", from, to).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Scan(&charges).Error; errFind != nil {
		return check, errFind
	}
	usageIDs := make([]uint, 0, len(charges))
	for _, charge := range charges {
		usageIDs = append(usageIDs, charge.UsageID)
	}
	usages, errUsages := billingBackfillLoadUsages(ctx, db, usageIDs)
	if errUsages != nil {
		return check, errUsages
	}
	for _, charge := range charges {
		usage := usages[charge.UsageID]
		if usage == nil {
			continue
		}
		check.Checked++
		_, amount, rule, snapshot, errPrice := pricer.price(ctx, usage)
		if errPrice != nil {
			return check, errPrice
		}
		var stored, computed any
		_ = json.Unmarshal([]byte(charge.PriceSnapshot), &stored)
		_ = json.Unmarshal(snapshot, &computed)
		if amount == charge.Amount && rule == charge.MatchedPriceRule && reflect.DeepEqual(stored, computed) {
			check.Matched++
			continue
		}
		check.Mismatched++
		if len(check.Examples) < 5 {
			check.Examples = append(check.Examples, fmt.Sprintf("charge %s: amount %v vs %v, rule %q vs %q", charge.ID, charge.Amount, amount, charge.MatchedPriceRule, rule))
		}
	}
	return check, nil
}
