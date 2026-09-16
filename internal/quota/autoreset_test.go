package quota

import (
	"math"
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
