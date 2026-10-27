package cluster

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newBackfillPostgresTestRepository returns a migrated repository in a throwaway
// schema, or skips when no test Postgres is configured.
func newBackfillPostgresTestRepository(t *testing.T, ctx context.Context) *Repository {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_HOME_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_HOME_TEST_POSTGRES_DSN is not configured")
	}
	adminDB, errOpen := gorm.Open(postgres.Open(dsn), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errOpen != nil {
		t.Fatalf("open postgres admin database: %v", errOpen)
	}
	schema := "billing_backfill_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if errCreate := adminDB.Exec("CREATE SCHEMA " + schema).Error; errCreate != nil {
		t.Fatalf("create postgres schema: %v", errCreate)
	}
	db, errSchemaOpen := gorm.Open(postgres.Open(postgresDSNWithSearchPath(dsn, schema)), databaseGORMConfig(defaultDatabaseSlowQueryThreshold))
	if errSchemaOpen != nil {
		t.Fatalf("open postgres schema database: %v", errSchemaOpen)
	}
	t.Cleanup(func() {
		if sqlDB, errDB := db.DB(); errDB == nil {
			_ = sqlDB.Close()
		}
		if errDrop := adminDB.Exec("DROP SCHEMA " + schema + " CASCADE").Error; errDrop != nil {
			t.Errorf("drop postgres schema: %v", errDrop)
		}
		if adminSQLDB, errAdmin := adminDB.DB(); errAdmin == nil {
			_ = adminSQLDB.Close()
		}
	})
	if errMigrate := AutoMigrateContext(ctx, db); errMigrate != nil {
		t.Fatalf("AutoMigrateContext() error = %v", errMigrate)
	}
	return NewRepository(db)
}

func appendBackfillTestUsage(t *testing.T, ctx context.Context, repo *Repository, at time.Time, requestID string, model string, inputTokens int64) {
	t.Helper()
	payload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","model":%q,"api_key":"client-key-backfill","request_id":%q,"endpoint":"/v1/responses","executor_type":"CodexExecutor","service_tier":"auto","auth_index":"auth-backfill","auth_type":"oauth","latency_ms":100,"tokens":{"input_tokens":%d,"cached_tokens":%d,"output_tokens":40,"reasoning_tokens":10,"total_tokens":%d}}`,
		at.UTC().Format(time.RFC3339), model, requestID, inputTokens, inputTokens/2, inputTokens+40)
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.30"); errUsage != nil {
		t.Fatalf("AppendUsage(%s) error = %v", requestID, errUsage)
	}
}

func loadBackfillTestCharge(t *testing.T, ctx context.Context, repo *Repository, requestID string) BillingChargeRecord {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	var charge BillingChargeRecord
	if errFind := db.WithContext(ctx).Where("request_id = ?", requestID).First(&charge).Error; errFind != nil {
		t.Fatalf("load charge %s: %v", requestID, errFind)
	}
	return charge
}

func backfillTestSnapshot(t *testing.T, raw JSONB) map[string]any {
	t.Helper()
	var out map[string]any
	if errUnmarshal := json.Unmarshal(raw, &out); errUnmarshal != nil {
		t.Fatalf("unmarshal snapshot: %v", errUnmarshal)
	}
	return out
}

func createBackfillTestPrices(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()
	for _, price := range []BillingModelPriceUpdate{
		{Provider: "codex", Model: "gpt-6-sol", ServiceTier: "*", MinInputTokens: 0, InputPricePerMillion: 2, CacheReadPricePerMillion: 0.2, OutputPricePerMillion: 10, Enabled: true},
		{Provider: "codex", Model: "gpt-6-sol", ServiceTier: "*", MinInputTokens: 272001, InputPricePerMillion: 4, CacheReadPricePerMillion: 0.4, OutputPricePerMillion: 15, Enabled: true},
	} {
		if _, errCreate := repo.CreateBillingModelPrice(ctx, price); errCreate != nil {
			t.Fatalf("CreateBillingModelPrice(%d) error = %v", price.MinInputTokens, errCreate)
		}
	}
}

func TestBackfillZeroBillingChargesMatchesHomePricingForNewRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	runBackfillMatchesHomePricing(t, ctx, repo)
}

// The Postgres variant exercises the bulk UPDATE ... FROM (VALUES ...) path that
// the live backfill uses; SQLite takes the per-row path.
func TestBackfillZeroBillingChargesMatchesHomePricingOnPostgres(t *testing.T) {
	ctx := context.Background()
	runBackfillMatchesHomePricing(t, ctx, newBackfillPostgresTestRepository(t, ctx))
}

func runBackfillMatchesHomePricing(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()
	base := time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC)
	// Written before any price: default:zero.
	appendBackfillTestUsage(t, ctx, repo, base, "old-short", "gpt-6-sol", 1000)
	appendBackfillTestUsage(t, ctx, repo, base.Add(time.Minute), "old-long", "gpt-6-sol", 300000)
	appendBackfillTestUsage(t, ctx, repo, base.Add(2*time.Minute), "old-unpriced-model", "gpt-unpriced", 1000)
	// Outside the backfill window: must stay untouched.
	appendBackfillTestUsage(t, ctx, repo, base.Add(-48*time.Hour), "before-window", "gpt-6-sol", 1000)

	createBackfillTestPrices(t, ctx, repo)
	// Identical requests after the prices exist: Home prices these itself.
	appendBackfillTestUsage(t, ctx, repo, base.Add(time.Hour), "new-short", "gpt-6-sol", 1000)
	appendBackfillTestUsage(t, ctx, repo, base.Add(time.Hour+time.Minute), "new-long", "gpt-6-sol", 300000)

	from := base.Add(-time.Hour)
	to := base.Add(30 * time.Minute)
	dryRun, errDry := repo.BackfillZeroBillingCharges(ctx, BillingZeroChargeBackfillOptions{From: from, To: to, BatchSize: 1})
	if errDry != nil {
		t.Fatalf("dry run error = %v", errDry)
	}
	if dryRun.Scanned != 3 || dryRun.Priced != 2 || dryRun.StillUnpriced != 1 || dryRun.Updated != 0 {
		t.Fatalf("dry run = %+v, want scanned 3, priced 2, still unpriced 1, updated 0", dryRun)
	}
	if got := loadBackfillTestCharge(t, ctx, repo, "old-short"); got.MatchedPriceRule != "default:zero" || got.Amount != 0 {
		t.Fatalf("dry run changed a charge: %+v", got)
	}

	var backup bytes.Buffer
	applied, errApply := repo.BackfillZeroBillingCharges(ctx, BillingZeroChargeBackfillOptions{From: from, To: to, BatchSize: 1, Apply: true, Backup: &backup})
	if errApply != nil {
		t.Fatalf("apply error = %v", errApply)
	}
	if applied.Updated != 2 || applied.Amount != dryRun.Amount {
		t.Fatalf("apply = %+v, want 2 updated and the dry-run amount %v", applied, dryRun.Amount)
	}

	for _, pair := range [][2]string{{"old-short", "new-short"}, {"old-long", "new-long"}} {
		backfilled := loadBackfillTestCharge(t, ctx, repo, pair[0])
		homePriced := loadBackfillTestCharge(t, ctx, repo, pair[1])
		if homePriced.MatchedPriceRule == "default:zero" || homePriced.Amount <= 0 {
			t.Fatalf("precondition: Home did not price %s: %+v", pair[1], homePriced)
		}
		if backfilled.Amount != homePriced.Amount || backfilled.MatchedPriceRule != homePriced.MatchedPriceRule {
			t.Fatalf("%s amount/rule = %v/%q, Home wrote %v/%q for the same request", pair[0], backfilled.Amount, backfilled.MatchedPriceRule, homePriced.Amount, homePriced.MatchedPriceRule)
		}
		if !reflect.DeepEqual(backfillTestSnapshot(t, backfilled.PriceSnapshot), backfillTestSnapshot(t, homePriced.PriceSnapshot)) {
			t.Fatalf("%s snapshot differs from Home's\nbackfill: %s\nhome:     %s", pair[0], backfilled.PriceSnapshot, homePriced.PriceSnapshot)
		}
		if backfilled.BalanceBefore != 0 || backfilled.BalanceAfter != 0 || backfilled.UserID != nil {
			t.Fatalf("%s balance fields changed: %+v", pair[0], backfilled)
		}
	}
	if long := loadBackfillTestCharge(t, ctx, repo, "old-long"); long.MatchedPriceRule != "codex:gpt-6-sol:*:272001" {
		t.Fatalf("long-context rule = %q, want the 272001 tier", long.MatchedPriceRule)
	}
	for _, requestID := range []string{"old-unpriced-model", "before-window"} {
		if got := loadBackfillTestCharge(t, ctx, repo, requestID); got.MatchedPriceRule != "default:zero" || got.Amount != 0 {
			t.Fatalf("%s should stay default:zero, got %+v", requestID, got)
		}
	}

	lines := 0
	scanner := bufio.NewScanner(&backup)
	for scanner.Scan() {
		var row map[string]any
		if errUnmarshal := json.Unmarshal(scanner.Bytes(), &row); errUnmarshal != nil {
			t.Fatalf("backup line %d: %v", lines, errUnmarshal)
		}
		if row["matched_price_rule"] != "default:zero" || row["amount"] != float64(0) {
			t.Fatalf("backup line %d does not hold the original values: %v", lines, row)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("backup lines = %d, want 2", lines)
	}

	rerun, errRerun := repo.BackfillZeroBillingCharges(ctx, BillingZeroChargeBackfillOptions{From: from, To: to, Apply: true, Backup: &bytes.Buffer{}})
	if errRerun != nil || rerun.Updated != 0 || rerun.Priced != 0 {
		t.Fatalf("second run = %+v, %v; want nothing left to price", rerun, errRerun)
	}

	check, errCheck := repo.VerifyBillingChargeRepricing(ctx, base.Add(30*time.Minute), base.Add(2*time.Hour), 10)
	if errCheck != nil {
		t.Fatalf("VerifyBillingChargeRepricing() error = %v", errCheck)
	}
	if check.Checked != 2 || check.Mismatched != 0 {
		t.Fatalf("repricing check = %+v, want 2 checked and no mismatch", check)
	}
}

func TestBackfillZeroBillingChargesRefusesWhenUsersExist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()

	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	if errCreate := db.WithContext(ctx).Create(&UserRecord{Username: "backfill-guard", Credits: 1}).Error; errCreate != nil {
		t.Fatalf("create user: %v", errCreate)
	}
	from := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	_, errBackfill := repo.BackfillZeroBillingCharges(ctx, BillingZeroChargeBackfillOptions{From: from, To: from.Add(time.Hour)})
	if !errors.Is(errBackfill, ErrBillingBackfillUsersPresent) {
		t.Fatalf("error = %v, want ErrBillingBackfillUsersPresent", errBackfill)
	}
}
