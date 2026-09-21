package quota

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// Reset credits are provider-issued grants that restore an exhausted quota window
// immediately instead of waiting out the natural reset (~4 days for Codex weekly
// windows). Spending one is irreversible, so every path here is opt-in and the
// decision to spend is logged with the numbers that produced it.
const (
	codexResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

	defaultAutoResetInterval        = time.Minute
	defaultAutoResetCollectInterval = 24 * time.Hour
	defaultAutoResetThresholdPct    = 0.0
	defaultAutoResetWithinDays      = 1.0
	defaultAutoResetExpiryWindow    = 6 * time.Hour

	// A spend lease is held per credential so two Home nodes cannot redeem the same
	// credit concurrently. It reuses the probe lease column, which the collector
	// already treats as the single writer token for a credential's quota row.
	autoResetSpendLease = 2 * time.Minute
)

// AutoResetOptions configures the three independent loops. Each interval is separate
// on purpose: the collect loop is expensive and slow-moving, while the two spend
// rules only read the snapshots it produces.
type AutoResetOptions struct {
	Enabled bool

	// SpendInterval drives both spend rules (A and B).
	SpendInterval time.Duration
	// CollectInterval drives the forced-refresh loop that keeps snapshots of idle
	// credentials from going stale. See the comment on runCollect for why this is
	// needed at all.
	CollectInterval time.Duration

	// Rule A — spend to revive an exhausted credential.
	RuleAEnabled bool
	// ThresholdPercent is the remaining-percent at or below which a window counts as
	// exhausted. 0 means "only when completely out".
	ThresholdPercent float64
	// WithinDays suppresses rule A when the window resets naturally within this many
	// days, since spending a credit then buys almost nothing.
	WithinDays float64

	// Rule B — spend a credit that is about to expire unused.
	RuleBEnabled bool
	// ExpiryWindow is how far ahead of expiry rule B fires.
	ExpiryWindow time.Duration

	Now func() time.Time
}

func (o *AutoResetOptions) applyDefaults() {
	if o.SpendInterval <= 0 {
		o.SpendInterval = defaultAutoResetInterval
	}
	if o.CollectInterval <= 0 {
		o.CollectInterval = defaultAutoResetCollectInterval
	}
	if o.ThresholdPercent < 0 {
		o.ThresholdPercent = defaultAutoResetThresholdPct
	}
	if o.WithinDays < 0 {
		o.WithinDays = defaultAutoResetWithinDays
	}
	if o.ExpiryWindow <= 0 {
		o.ExpiryWindow = defaultAutoResetExpiryWindow
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
}

// AutoReset spends provider reset credits on a schedule. It borrows the collector's
// HTTP path (proxy config, token resolution, response limits) rather than building a
// second one, so a credential that can be probed can also be reset.
//
// Settings live in the database and are re-read every tick, so a change made from the
// console takes effect without restarting Home.
type AutoReset struct {
	repo      *cluster.Repository
	collector *Collector

	optionsMu sync.RWMutex
	options   AutoResetOptions

	background sync.WaitGroup
}

func NewAutoReset(repo *cluster.Repository, collector *Collector, options AutoResetOptions) *AutoReset {
	if repo == nil || collector == nil {
		return nil
	}
	options.applyDefaults()
	return &AutoReset{repo: repo, collector: collector, options: options}
}

// currentOptions returns the settings snapshot the current tick should use.
func (a *AutoReset) currentOptions() AutoResetOptions {
	a.optionsMu.RLock()
	defer a.optionsMu.RUnlock()
	return a.options
}

// refreshOptions reloads the stored settings. A read failure leaves the previous
// snapshot in place and is logged: continuing with the last known-good settings is
// safer than falling back to defaults, which would silently change spend behavior.
func (a *AutoReset) refreshOptions(ctx context.Context) AutoResetOptions {
	record, errGet := a.repo.GetQuotaAutoResetConfig(ctx)
	if errGet != nil {
		log.WithError(errGet).Warn("quota auto reset: config read failed, keeping previous settings")
		return a.currentOptions()
	}
	spendInterval, collectInterval, expiryWindow, errDurations := record.Durations()
	if errDurations != nil {
		log.WithError(errDurations).Warn("quota auto reset: stored config is invalid, keeping previous settings")
		return a.currentOptions()
	}
	options := AutoResetOptions{
		Enabled:          record.Enabled,
		SpendInterval:    spendInterval,
		CollectInterval:  collectInterval,
		RuleAEnabled:     record.RuleAEnabled,
		ThresholdPercent: record.ThresholdPercent,
		WithinDays:       record.WithinDays,
		RuleBEnabled:     record.RuleBEnabled,
		ExpiryWindow:     expiryWindow,
		Now:              a.currentOptions().Now,
	}
	options.applyDefaults()
	a.optionsMu.Lock()
	a.options = options
	a.optionsMu.Unlock()
	return options
}

func (a *AutoReset) Start(ctx context.Context) {
	if a == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.background.Add(2)
	go func() {
		defer a.background.Done()
		a.loop(ctx, func(o AutoResetOptions) time.Duration { return o.SpendInterval }, a.runSpend)
	}()
	go func() {
		defer a.background.Done()
		a.loop(ctx, func(o AutoResetOptions) time.Duration { return o.CollectInterval }, a.runCollect)
	}()
	log.Info("quota auto reset started")
}

// Wait blocks until both loops have exited. Primarily for deterministic tests.
func (a *AutoReset) Wait() {
	if a == nil {
		return
	}
	a.background.Wait()
}

// loop re-reads the settings before each tick and re-arms the timer with the interval
// they specify, so an interval edited in the console applies from the next tick rather
// than at the next restart.
func (a *AutoReset) loop(ctx context.Context, interval func(AutoResetOptions) time.Duration, fn func(context.Context, AutoResetOptions)) {
	for {
		options := a.refreshOptions(ctx)
		if options.Enabled {
			fn(ctx, options)
		}
		timer := time.NewTimer(interval(options))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// runCollect forces a refresh of every eligible credential.
//
// The scheduled collector only probes credentials with request traffic in the last
// 30 minutes (see quotaProbeActivityWindow). A credential that is exhausted stops
// receiving requests, so it stops being probed, and its snapshot — including the
// expiry timestamps rule B reads — freezes at whatever it was when traffic stopped.
// Forcing a round bypasses that gate.
func (a *AutoReset) runCollect(ctx context.Context, _ AutoResetOptions) {
	accepted, errTrigger := a.collector.TriggerCollection(ctx, nil, nil)
	if errTrigger != nil {
		log.WithError(errTrigger).Warn("quota auto reset: forced collection failed")
		return
	}
	log.WithField("accepted", accepted).Debug("quota auto reset: forced collection queued")
}

func (a *AutoReset) runSpend(ctx context.Context, options AutoResetOptions) {
	if !options.RuleAEnabled && !options.RuleBEnabled {
		return
	}
	now := options.Now().UTC()
	result, errList := a.repo.ListQuotaCredentials(ctx, cluster.QuotaListQuery{Now: now})
	if errList != nil {
		log.WithError(errList).Warn("quota auto reset: list credentials failed")
		return
	}
	for i := range result.Items {
		item := result.Items[i]
		reason, ok := a.decide(&item, options, now)
		if !ok {
			continue
		}
		a.spend(ctx, &item, reason, now)
	}
}

// decide reports why a credential should have a credit spent on it, or false when
// none of the enabled rules apply. Rule B is checked first: a credit that is about
// to expire is lost whether or not the quota needs it, so rescuing it strictly
// dominates holding it back.
func (a *AutoReset) decide(item *cluster.QuotaCredentialSnapshot, options AutoResetOptions, now time.Time) (string, bool) {
	if item.ResetCredits == nil || len(item.ResetCredits.Credits) == 0 {
		return "", false
	}
	if options.RuleBEnabled {
		// Credits are stored sorted by expiry, nulls last, so the first dated credit
		// is the soonest to expire.
		for _, credit := range item.ResetCredits.Credits {
			if credit.ExpiresAt == nil {
				continue
			}
			// A credit that already expired cannot be redeemed, and it satisfies the
			// rescue window below on every tick, so the rule would spend on it forever.
			// Skip to the next credit, which the sort order says expires later.
			if !credit.ExpiresAt.After(now) {
				continue
			}
			if !credit.ExpiresAt.After(now.Add(options.ExpiryWindow)) {
				return fmt.Sprintf("credit_expiring_at_%s", credit.ExpiresAt.UTC().Format(time.RFC3339)), true
			}
			break
		}
	}
	if options.RuleAEnabled && a.exhausted(item, options, now) {
		return "quota_exhausted", true
	}
	return "", false
}

// exhausted reports whether any window is at or below the configured remaining
// threshold and is far enough from its natural reset to be worth a credit.
func (a *AutoReset) exhausted(item *cluster.QuotaCredentialSnapshot, options AutoResetOptions, now time.Time) bool {
	horizon := now.Add(time.Duration(options.WithinDays * float64(24*time.Hour)))
	for _, window := range item.Windows {
		if window.IsUnlimited {
			continue
		}
		remaining, ok := remainingPercent(window)
		if !ok || remaining > options.ThresholdPercent {
			continue
		}
		// A window that resets on its own shortly is not worth an irreversible credit.
		if window.ResetAt != nil && !window.ResetAt.After(horizon) {
			continue
		}
		return true
	}
	return false
}

// remainingPercent derives a 0-100 remaining figure. used_percent is reported as null
// by the provider even after an active probe, so it is never relied on here.
func remainingPercent(window cluster.QuotaWindow) (float64, bool) {
	if window.RemainingRatio != nil {
		return *window.RemainingRatio * 100, true
	}
	if window.UsedRatio != nil {
		return (1 - *window.UsedRatio) * 100, true
	}
	if window.Remaining != nil && window.Limit != nil && *window.Limit > 0 {
		return (*window.Remaining / *window.Limit) * 100, true
	}
	return 0, false
}

func (a *AutoReset) spend(ctx context.Context, item *cluster.QuotaCredentialSnapshot, reason string, now time.Time) {
	fields := log.Fields{
		"credential_id": item.CredentialID,
		"label":         item.Label,
		"reason":        reason,
	}
	if item.ResetCredits != nil && item.ResetCredits.AvailableCount != nil {
		fields["available_count"] = *item.ResetCredits.AvailableCount
	}

	// Claiming the probe lease is what makes this safe to run on every node: only the
	// node that wins the claim proceeds, and the lease expires on its own if this
	// node dies mid-spend.
	claimed, errClaim := a.repo.ClaimQuotaProbe(ctx, item.CredentialID, a.collector.options.Owner, now, autoResetSpendLease)
	if errClaim != nil {
		log.WithError(errClaim).WithFields(fields).Warn("quota auto reset: lease claim failed")
		return
	}
	if !claimed {
		return
	}

	auth, _, errAuth := a.repo.GetAuth(ctx, item.CredentialID)
	if errAuth != nil || auth == nil {
		log.WithError(errAuth).WithFields(fields).Warn("quota auto reset: credential resolve failed")
		return
	}
	if errConsume := a.consumeCredit(ctx, auth); errConsume != nil {
		log.WithError(errConsume).WithFields(fields).Warn("quota auto reset: credit consume failed")
		return
	}
	log.WithFields(fields).Info("quota auto reset: reset credit consumed")

	// Re-probe so the snapshot reflects the restored window immediately; without this
	// the next tick would read the pre-reset numbers and could spend a second credit.
	if _, errTrigger := a.collector.TriggerCollection(ctx, map[string]struct{}{item.CredentialID: {}}, nil); errTrigger != nil {
		log.WithError(errTrigger).WithFields(fields).Warn("quota auto reset: post-reset collection failed")
	}
}

func (a *AutoReset) consumeCredit(ctx context.Context, auth *coreauth.Auth) error {
	if normalizedQuotaProvider(auth.Provider) != "codex" {
		return fmt.Errorf("provider %q has no reset credit endpoint", auth.Provider)
	}
	headers := http.Header{"Content-Type": []string{"application/json"}, "User-Agent": []string{codexUserAgent}}
	if accountID := quotaMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	requestID, errRequestID := newRedeemRequestID()
	if errRequestID != nil {
		return errRequestID
	}
	body, errMarshal := json.Marshal(map[string]string{"redeem_request_id": requestID})
	if errMarshal != nil {
		return fmt.Errorf("encode redeem request: %w", errMarshal)
	}
	payload, _, errProbe := a.collector.probeRequest(ctx, auth, http.MethodPost, codexResetCreditsConsumeURL, body, headers)
	if errProbe != nil {
		return fmt.Errorf("consume reset credit: %s", errProbe.message)
	}
	// A 2xx does not prove the credit was redeemed: the provider answers 200 for
	// requests it declines too. Nothing here parses the body because its shape on the
	// decline path is unverified; recording it is what makes that shape knowable.
	log.WithField("response", truncateForLog(payload, maxConsumeLogBytes)).
		Debug("quota auto reset: consume response")
	return nil
}

// maxConsumeLogBytes caps how much of a consume response reaches the log. The body is
// small in practice; the cap only guards against an unexpected payload.
const maxConsumeLogBytes = 512

func truncateForLog(payload []byte, limit int) string {
	text := strings.TrimSpace(string(payload))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "...(truncated)"
}

// newRedeemRequestID produces the UUIDv4 the provider expects as an idempotency key.
func newRedeemRequestID() (string, error) {
	buf := make([]byte, 16)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", fmt.Errorf("generate redeem request id: %w", errRead)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return strings.Join([]string{hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32]}, "-"), nil
}
