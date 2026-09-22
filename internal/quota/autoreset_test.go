package quota

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func testAutoReset(options AutoResetOptions) *AutoReset {
	options.applyDefaults()
	return &AutoReset{options: options}
}

func floatPtr(v float64) *float64    { return &v }
func timePtr(v time.Time) *time.Time { return &v }

func creditsWithExpiry(expiry *time.Time) *cluster.QuotaResetCredits {
	count := 1
	return &cluster.QuotaResetCredits{
		AvailableCount: &count,
		Credits:        []cluster.QuotaResetCredit{{ID: "credit-1", Status: "available", ExpiresAt: expiry}},
	}
}

func TestDecideSkipsCredentialWithoutCredits(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleAEnabled: true, RuleBEnabled: true})
	item := &cluster.QuotaCredentialSnapshot{
		Windows: []cluster.QuotaWindow{{RemainingRatio: floatPtr(0)}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit on a credential that has none")
	}
}

func TestDecideRuleAExhausted(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleAEnabled: true, ThresholdPercent: 0, WithinDays: 1})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(nil),
		Windows: []cluster.QuotaWindow{{
			RemainingRatio: floatPtr(0),
			ResetAt:        timePtr(now.Add(72 * time.Hour)),
		}},
	}
	reason, ok := a.decide(item, a.currentOptions(), now)
	if !ok || reason != "quota_exhausted" {
		t.Fatalf("decide() = %q, %v; want quota_exhausted, true", reason, ok)
	}
}

// A window that resets on its own shortly is not worth an irreversible credit.
func TestDecideRuleASkipsImminentNaturalReset(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleAEnabled: true, ThresholdPercent: 0, WithinDays: 1})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(nil),
		Windows: []cluster.QuotaWindow{{
			RemainingRatio: floatPtr(0),
			ResetAt:        timePtr(now.Add(2 * time.Hour)),
		}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit on a window that resets within the day")
	}
}

func TestDecideRuleASkipsHealthyWindow(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleAEnabled: true, ThresholdPercent: 0, WithinDays: 1})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(nil),
		Windows: []cluster.QuotaWindow{{
			RemainingRatio: floatPtr(0.5),
			ResetAt:        timePtr(now.Add(72 * time.Hour)),
		}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit on a window with quota remaining")
	}
}

// Rule B rescues a credit that would otherwise expire unused, so it fires even when
// the credential still has plenty of quota.
func TestDecideRuleBFiresRegardlessOfQuota(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleBEnabled: true, ExpiryWindow: 6 * time.Hour})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(timePtr(now.Add(time.Hour))),
		Windows:      []cluster.QuotaWindow{{RemainingRatio: floatPtr(1)}},
	}
	reason, ok := a.decide(item, a.currentOptions(), now)
	if !ok {
		t.Fatal("decide() did not rescue a credit expiring within the window")
	}
	if reason == "quota_exhausted" {
		t.Fatalf("decide() = %q; want an expiry reason", reason)
	}
}

func TestDecideRuleBSkipsDistantExpiry(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleBEnabled: true, ExpiryWindow: 6 * time.Hour})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(timePtr(now.Add(72 * time.Hour))),
		Windows:      []cluster.QuotaWindow{{RemainingRatio: floatPtr(1)}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit that expires days from now")
	}
}

// An expired credit cannot be redeemed, yet it satisfies the rescue window on every
// tick. Left unguarded the rule spends on it forever: one stale snapshot holding an
// expired credit produced 2191 consume attempts in 30 hours.
func TestDecideRuleBIgnoresExpiredCredit(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleBEnabled: true, ExpiryWindow: 6 * time.Hour})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(timePtr(now.Add(-2 * time.Hour))),
		Windows:      []cluster.QuotaWindow{{RemainingRatio: floatPtr(1)}},
	}
	if reason, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatalf("decide() = %q; want no spend on an expired credit", reason)
	}
}

// Expiry sorts nulls last, so an expired credit sits ahead of live ones. Skipping it
// must not skip the rest: the credit behind it may genuinely need rescuing.
func TestDecideRuleBRescuesLiveCreditBehindExpiredOne(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleBEnabled: true, ExpiryWindow: 6 * time.Hour})
	count := 2
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: &cluster.QuotaResetCredits{
			AvailableCount: &count,
			Credits: []cluster.QuotaResetCredit{
				{ID: "expired", Status: "available", ExpiresAt: timePtr(now.Add(-2 * time.Hour))},
				{ID: "live", Status: "available", ExpiresAt: timePtr(now.Add(time.Hour))},
			},
		},
		Windows: []cluster.QuotaWindow{{RemainingRatio: floatPtr(1)}},
	}
	reason, ok := a.decide(item, a.currentOptions(), now)
	if !ok {
		t.Fatal("decide() did not rescue the live credit behind an expired one")
	}
	if reason == "quota_exhausted" {
		t.Fatalf("decide() = %q; want an expiry reason", reason)
	}
}

// A distant credit behind an expired one still must not be spent early.
func TestDecideRuleBStopsAtFirstLiveDistantCredit(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleBEnabled: true, ExpiryWindow: 6 * time.Hour})
	count := 2
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: &cluster.QuotaResetCredits{
			AvailableCount: &count,
			Credits: []cluster.QuotaResetCredit{
				{ID: "expired", Status: "available", ExpiresAt: timePtr(now.Add(-2 * time.Hour))},
				{ID: "distant", Status: "available", ExpiresAt: timePtr(now.Add(72 * time.Hour))},
			},
		},
		Windows: []cluster.QuotaWindow{{RemainingRatio: floatPtr(1)}},
	}
	if reason, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatalf("decide() = %q; want no spend when the live credit expires days out", reason)
	}
}

// Both rules disabled must never spend, even when every other condition holds.
func TestDecideRespectsDisabledRules(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(timePtr(now.Add(time.Minute))),
		Windows: []cluster.QuotaWindow{{
			RemainingRatio: floatPtr(0),
			ResetAt:        timePtr(now.Add(72 * time.Hour)),
		}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit while both rules were disabled")
	}
}

// used_percent comes back null from the provider even after an active probe, so the
// remaining figure has to survive being derived from each of the other shapes.
func TestRemainingPercentSources(t *testing.T) {
	cases := []struct {
		name   string
		window cluster.QuotaWindow
		want   float64
		ok     bool
	}{
		{"remaining ratio", cluster.QuotaWindow{RemainingRatio: floatPtr(0.25)}, 25, true},
		{"used ratio", cluster.QuotaWindow{UsedRatio: floatPtr(0.9)}, 10, true},
		{"absolute", cluster.QuotaWindow{Remaining: floatPtr(5), Limit: floatPtr(50)}, 10, true},
		{"no data", cluster.QuotaWindow{}, 0, false},
		{"zero limit", cluster.QuotaWindow{Remaining: floatPtr(5), Limit: floatPtr(0)}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := remainingPercent(tc.window)
			// Binary floating point makes (1-0.9)*100 land a hair off 10, so compare
			// with a tolerance rather than exactly.
			if ok != tc.ok || (ok && math.Abs(got-tc.want) > 1e-9) {
				t.Fatalf("remainingPercent() = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDecideIgnoresUnlimitedWindow(t *testing.T) {
	now := time.Now().UTC()
	a := testAutoReset(AutoResetOptions{RuleAEnabled: true})
	item := &cluster.QuotaCredentialSnapshot{
		ResetCredits: creditsWithExpiry(nil),
		Windows:      []cluster.QuotaWindow{{IsUnlimited: true, RemainingRatio: floatPtr(0)}},
	}
	if _, ok := a.decide(item, a.currentOptions(), now); ok {
		t.Fatal("decide() spent a credit on an unlimited window")
	}
}

func TestNewRedeemRequestIDIsUUIDv4(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id, errID := newRedeemRequestID()
		if errID != nil {
			t.Fatalf("newRedeemRequestID() error = %v", errID)
		}
		if len(id) != 36 || id[14] != '4' {
			t.Fatalf("newRedeemRequestID() = %q; want a 36-char UUIDv4", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newRedeemRequestID() returned duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}

// codexResetCreditSpendStub serves the three Codex endpoints AutoReset.spend()'s
// path touches: usage (exhausted window), reset-credits balance, and consume. The
// balance served after consume is called drops by one, the way a real redemption
// would look; a stub that never moves the count would make the regression this
// test guards against invisible.
func codexResetCreditSpendStub(t *testing.T, consumed *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "reset-credits/consume"):
			*consumed++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(r.URL.Path, "reset-credits"):
			available := 2
			if *consumed > 0 {
				available = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"available_count": available, "credits": []any{}})
		default:
			_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":345600,"reset_at":1758542400}}}`))
		}
	}))
}

// TestSpendReleasesProbeLeaseForImmediateReprobe is the regression test for the
// collecting-status livelock: AutoReset.spend() used to claim the probe lease
// (ClaimQuotaProbe) purely to serialize its own HTTP call, which stamped
// collection_status="collecting" as a side effect. Its own follow-up re-probe then
// always lost the claim race against the lease it had just set two lines earlier,
// so collectCredential's silent !claimed no-op left the row stuck at "collecting"
// forever with the credit never confirmed spent or not.
//
// This test fails on the pre-fix code (spend() calling ClaimQuotaProbe) because
// the forced re-probe claim loses that race and the snapshot is never rewritten:
// collection_status stays "collecting" and the reset-credit balance is never
// re-read, so available_count still reads 2. It passes once spend() claims its
// own lease (ClaimQuotaSpendLease) instead, leaving the probe lease free for the
// re-probe to claim, complete, and observe the balance drop to 1.
func TestSpendReleasesProbeLeaseForImmediateReprobe(t *testing.T) {
	ctx := context.Background()
	repo := newCollectorTestRepository(t)
	const credentialID = "codex-spend-livelock"
	seedCollectorProviderAuth(t, repo, credentialID, "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})

	now := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	credits := 2
	expiresAt := now.Add(-time.Minute) // already stale so exhaustion is visible immediately
	if _, errSeed := repo.UpsertQuotaSnapshot(ctx, cluster.QuotaSnapshotWrite{
		CredentialID: credentialID, QuotaStatus: "exhausted", CollectionStatus: "success", Source: "active_probe",
		ObservedAt: &now, ExpiresAt: &expiresAt, LastSuccessAt: &now,
		ReplaceWindows: true, Windows: []cluster.QuotaWindow{{
			ID: "codex-1-week", Scope: "account", Mode: "rolling", Status: "exhausted", Unit: "percentage",
			RemainingRatio: floatPtr(0), ResetAt: timePtr(now.Add(96 * time.Hour)), PeriodUnit: "week", PeriodValue: floatPtr(1), Source: "active_probe", ObservedAt: now,
		}},
		ResetCredits:        &cluster.QuotaResetCredits{AvailableCount: &credits, ObservedAt: now, Credits: []cluster.QuotaResetCredit{{ID: "credit-1", Status: "available", GrantedAt: now.Add(-24 * time.Hour), ExpiresAt: nil}}},
		ReplaceResetCredits: true,
	}); errSeed != nil {
		t.Fatalf("UpsertQuotaSnapshot() error = %v", errSeed)
	}

	var consumed int32
	server := codexResetCreditSpendStub(t, &consumed)
	defer server.Close()

	collector := NewCollector(repo, Options{
		Owner: "home-a", CodexUsageURL: server.URL + "/usage", CodexResetCreditsURL: server.URL + "/reset-credits",
		CodexResetCreditsConsumeURL: server.URL + "/reset-credits/consume", Now: func() time.Time { return now },
	})
	autoReset := NewAutoReset(repo, collector, AutoResetOptions{Enabled: true, RuleAEnabled: true, ThresholdPercent: 0, WithinDays: 1, Now: func() time.Time { return now }})
	if autoReset == nil {
		t.Fatal("NewAutoReset() returned nil")
	}

	item, errGet := repo.GetQuotaCredential(ctx, credentialID, now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	reason, ok := autoReset.decide(item, autoReset.currentOptions(), now)
	if !ok || reason != "quota_exhausted" {
		t.Fatalf("decide() = %q, %v; want quota_exhausted, true", reason, ok)
	}

	autoReset.spend(ctx, item, reason, now)

	if consumed != 1 {
		t.Fatalf("consume requests = %d, want exactly 1", consumed)
	}

	refreshed, errRefresh := repo.GetQuotaCredential(ctx, credentialID, now)
	if errRefresh != nil {
		t.Fatalf("GetQuotaCredential() after spend error = %v", errRefresh)
	}
	if refreshed.CollectionStatus != "success" {
		t.Fatalf("collection_status after spend = %q, want %q (the probe lease must not be blocked by spend's own lease)", refreshed.CollectionStatus, "success")
	}
	if refreshed.ResetCredits == nil || refreshed.ResetCredits.AvailableCount == nil {
		t.Fatal("reset_credits.available_count missing after spend; the re-probe must have run to confirm the redemption")
	}
	if *refreshed.ResetCredits.AvailableCount != 1 {
		t.Fatalf("available_count after spend = %d, want 1 (the re-probe must observe the post-consume balance)", *refreshed.ResetCredits.AvailableCount)
	}
}

// TestClaimQuotaSpendLeaseDoesNotBlockProbeLease is a narrower, single-purpose
// proof of the same fix at the repository layer: claiming the spend lease must
// never touch collection_status or the probe lease, so a probe claim on the same
// credential succeeds immediately afterward.
func TestClaimQuotaSpendLeaseDoesNotBlockProbeLease(t *testing.T) {
	ctx := context.Background()
	repo := newCollectorTestRepository(t)
	const credentialID = "codex-spend-lease-isolated"
	seedCollectorProviderAuth(t, repo, credentialID, "codex", map[string]any{"type": "codex", "access_token": "probe-secret"})
	now := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)

	claimed, errClaim := repo.ClaimQuotaSpendLease(ctx, credentialID, "home-a", now, time.Minute)
	if errClaim != nil || !claimed {
		t.Fatalf("ClaimQuotaSpendLease() = %v, %v; want true, nil", claimed, errClaim)
	}

	item, errGet := repo.GetQuotaCredential(ctx, credentialID, now)
	if errGet != nil {
		t.Fatalf("GetQuotaCredential() error = %v", errGet)
	}
	if item.CollectionStatus == "collecting" {
		t.Fatalf("collection_status = %q after claiming the spend lease; the spend lease must not touch collection_status", item.CollectionStatus)
	}

	// spend()'s own re-probe goes through the forced path (collectCredential with
	// force=true), so this mirrors that rather than the scheduled non-forced claim.
	probeClaimed, errProbeClaim := repo.ForceClaimEligibleQuotaProbe(ctx, credentialID, "home-a", now, time.Minute)
	if errProbeClaim != nil {
		t.Fatalf("ForceClaimEligibleQuotaProbe() error = %v", errProbeClaim)
	}
	if !probeClaimed {
		t.Fatal("ForceClaimEligibleQuotaProbe() = false; the spend lease must not block the probe lease")
	}
}
