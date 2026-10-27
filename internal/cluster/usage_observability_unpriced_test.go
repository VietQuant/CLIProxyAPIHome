package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func appendUnpricedTestUsage(t *testing.T, ctx context.Context, repo *Repository, at time.Time, model string, apiKey string, failed bool) {
	t.Helper()
	payload := fmt.Sprintf(`{"timestamp":%q,"provider":"openai","model":%q,"api_key":%q,"request_id":"req-%s-%d","endpoint":"/v1/chat/completions","auth_index":"auth-unpriced","auth_type":"oauth","failed":%t,"latency_ms":120,"tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
		at.UTC().Format(time.RFC3339), model, apiKey, model, at.UnixNano(), failed)
	if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.20"); errUsage != nil {
		t.Fatalf("AppendUsage(%s) error = %v", model, errUsage)
	}
}

func TestUsageObservabilityOverviewCountsUnpricedRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	runUsageObservabilityOverviewCountsUnpricedRequests(t, ctx, repo)
}

func TestUsageObservabilityOverviewCountsUnpricedRequestsOnPostgres(t *testing.T) {
	ctx := context.Background()
	runUsageObservabilityOverviewCountsUnpricedRequests(t, ctx, newBackfillPostgresTestRepository(t, ctx))
}

func runUsageObservabilityOverviewCountsUnpricedRequests(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()

	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	// Charged before any price exists: recorded as default:zero.
	appendUnpricedTestUsage(t, ctx, repo, base, "gpt-4.1", "client-key-a", false)
	appendUnpricedTestUsage(t, ctx, repo, base.Add(time.Minute), "gpt-4.1-mini", "client-key-b", false)
	// A failed request is never charged, so it is not "unpriced" either.
	appendUnpricedTestUsage(t, ctx, repo, base.Add(2*time.Minute), "gpt-4.1-mini", "client-key-b", true)

	if _, errCreatePrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1", RequestPrice: 1, Enabled: true}); errCreatePrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errCreatePrice)
	}
	// Charged after the price exists: priced.
	appendUnpricedTestUsage(t, ctx, repo, base.Add(3*time.Minute), "gpt-4.1", "client-key-a", false)
	// Still no price for this model: default:zero.
	appendUnpricedTestUsage(t, ctx, repo, base.Add(4*time.Minute), "gpt-4.1-mini", "client-key-a", false)

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{From: &from, To: &to})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	if overview.Totals.RequestCount != 5 {
		t.Fatalf("request count = %d, want 5", overview.Totals.RequestCount)
	}
	if overview.Totals.UnpricedRequests != 3 {
		t.Fatalf("totals unpriced = %d, want 3", overview.Totals.UnpricedRequests)
	}

	wantByModel := map[string]int64{"gpt-4.1": 1, "gpt-4.1-mini": 2}
	for name, items := range map[string][]UsageObservabilityAggregateItem{"top.models": overview.Top.Models, "model_efficiency": overview.ModelEfficiency} {
		if len(items) != len(wantByModel) {
			t.Fatalf("%s item count = %d, want %d", name, len(items), len(wantByModel))
		}
		for _, item := range items {
			want, ok := wantByModel[item.ID]
			if !ok {
				t.Fatalf("%s unexpected model %q", name, item.ID)
			}
			if item.UnpricedRequests != want {
				t.Fatalf("%s %s unpriced = %d, want %d", name, item.ID, item.UnpricedRequests, want)
			}
		}
	}
}

func TestUsageObservabilityActiveCountsMatchSequentialTotals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	runUsageObservabilityActiveCountsMatchSequentialTotals(t, ctx, repo)
}

func TestUsageObservabilityActiveCountsMatchSequentialTotalsOnPostgres(t *testing.T) {
	ctx := context.Background()
	runUsageObservabilityActiveCountsMatchSequentialTotals(t, ctx, newBackfillPostgresTestRepository(t, ctx))
}

func runUsageObservabilityActiveCountsMatchSequentialTotals(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()

	base := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	appendUnpricedTestUsage(t, ctx, repo, base, "gpt-4.1", "client-key-a", false)
	appendUnpricedTestUsage(t, ctx, repo, base.Add(time.Minute), "gpt-4.1", "client-key-a", false)
	appendUnpricedTestUsage(t, ctx, repo, base.Add(2*time.Minute), "gpt-4.1-mini", "client-key-b", false)
	appendUnpricedTestUsage(t, ctx, repo, base.Add(3*time.Minute), "o4-mini", "client-key-c", true)

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{From: &from, To: &to})
	if errOverview != nil {
		t.Fatalf("UsageObservabilityOverview() error = %v", errOverview)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	sequential, _, errTotals := usageObservabilityTotalsSQL(db.WithContext(ctx), usageObservabilityOverviewRecordQuery(UsageObservabilityOverviewQuery{From: &from, To: &to}))
	if errTotals != nil {
		t.Fatalf("usageObservabilityTotalsSQL() error = %v", errTotals)
	}

	got := overview.Totals
	if got.ActiveModelCount != 3 || got.ActiveCredentialCount != 1 {
		t.Fatalf("active models/credentials = %d/%d, want 3/1", got.ActiveModelCount, got.ActiveCredentialCount)
	}
	if got.ActiveUserCount != 0 {
		t.Fatalf("active users = %d, want 0 because no client key belongs to a user", got.ActiveUserCount)
	}
	for _, pair := range []struct {
		name      string
		got, want int64
	}{
		{"active_user_count", got.ActiveUserCount, sequential.ActiveUserCount},
		{"active_client_key_count", got.ActiveClientKeyCount, sequential.ActiveClientKeyCount},
		{"active_credential_count", got.ActiveCredentialCount, sequential.ActiveCredentialCount},
		{"active_model_count", got.ActiveModelCount, sequential.ActiveModelCount},
		{"request_count", got.RequestCount, sequential.RequestCount},
		{"unpriced_requests", got.UnpricedRequests, sequential.UnpricedRequests},
	} {
		if pair.got != pair.want {
			t.Fatalf("%s: concurrent overview = %d, sequential totals = %d", pair.name, pair.got, pair.want)
		}
	}
	if (got.P95LatencyMS == nil) != (sequential.P95LatencyMS == nil) || (got.P95LatencyMS != nil && *got.P95LatencyMS != *sequential.P95LatencyMS) {
		t.Fatalf("p95 differs: concurrent %v, sequential %v", got.P95LatencyMS, sequential.P95LatencyMS)
	}
}
