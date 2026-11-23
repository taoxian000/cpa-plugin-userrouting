package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	autoSortStrategyBorrow        = "borrow"
	autoSortStrategyWeeklyUrgency = "weekly_urgency"

	defaultAutoSortRefreshInterval = 5 * time.Minute
	minAutoSortRefreshInterval     = 30 * time.Second
	defaultAutoSortFiveHourReserve = 0.25
	autoSortInitialDelay           = 3 * time.Second
	autoSortRetryDelay             = 30 * time.Second

	codexPrimaryWindowLength   = 5 * time.Hour
	codexSecondaryWindowLength = 7 * 24 * time.Hour
	// Early in a weekly window the observed usage rate is too noisy, so the
	// owner is assumed to use the full remaining quota at a uniform pace.
	autoSortMinObservedWeek = 24 * time.Hour
	// Prevents the weekly urgency from diverging right before a reset.
	autoSortMinUrgencyHours = 0.5
)

// autoSortConfig is the raw quota_fallback.auto_sort YAML block.
type autoSortConfig struct {
	Enabled         bool            `yaml:"enabled"`
	Strategy        string          `yaml:"strategy"`
	RefreshInterval string          `yaml:"refresh_interval"`
	FiveHourReserve reserveFraction `yaml:"five_hour_reserve"`
	Prefixes        []string        `yaml:"prefixes"`
}

// reserveFraction accepts 0.25, 25 or "25%" and stores a 0..1 fraction.
type reserveFraction struct {
	Value float64
	Set   bool
}

func (f *reserveFraction) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("quota_fallback.auto_sort.five_hour_reserve must be a number or percentage")
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return nil
	}
	percent := strings.HasSuffix(raw, "%")
	value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(raw, "%")), 64)
	if err != nil || math.IsNaN(value) || value < 0 {
		return errors.New("quota_fallback.auto_sort.five_hour_reserve must be a non-negative number or percentage")
	}
	if percent || value > 1 {
		value /= 100
	}
	if value > 1 {
		return errors.New("quota_fallback.auto_sort.five_hour_reserve must not exceed 100%")
	}
	f.Value, f.Set = value, true
	return nil
}

type autoSortSettings struct {
	Enabled         bool
	Strategy        string
	RefreshInterval time.Duration
	FiveHourReserve float64
	Prefixes        []string
}

func normalizeAutoSort(input autoSortConfig) (autoSortSettings, error) {
	out := autoSortSettings{
		Enabled:         input.Enabled,
		Strategy:        strings.ToLower(strings.TrimSpace(input.Strategy)),
		FiveHourReserve: defaultAutoSortFiveHourReserve,
	}
	if out.Strategy == "" {
		out.Strategy = autoSortStrategyBorrow
	}
	if out.Strategy != autoSortStrategyBorrow && out.Strategy != autoSortStrategyWeeklyUrgency {
		return autoSortSettings{}, fmt.Errorf("quota_fallback.auto_sort.strategy must be %q or %q", autoSortStrategyBorrow, autoSortStrategyWeeklyUrgency)
	}
	if input.FiveHourReserve.Set {
		out.FiveHourReserve = input.FiveHourReserve.Value
	}
	interval, err := parsePositiveDuration(input.RefreshInterval, defaultAutoSortRefreshInterval, "quota_fallback.auto_sort.refresh_interval")
	if err != nil {
		return autoSortSettings{}, err
	}
	if interval < minAutoSortRefreshInterval {
		return autoSortSettings{}, fmt.Errorf("quota_fallback.auto_sort.refresh_interval must be at least %s", minAutoSortRefreshInterval)
	}
	out.RefreshInterval = interval
	seen := make(map[string]struct{}, len(input.Prefixes))
	for _, raw := range input.Prefixes {
		prefix := normalizePrefixName(raw)
		if prefix == "" {
			return autoSortSettings{}, errors.New("quota_fallback.auto_sort.prefixes contains an empty prefix")
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		out.Prefixes = append(out.Prefixes, prefix)
	}
	if out.Enabled && len(out.Prefixes) < 2 {
		return autoSortSettings{}, errors.New("quota_fallback.auto_sort.prefixes must list at least two prefixes when auto_sort is enabled")
	}
	return out, nil
}

type autoSortWindow struct {
	Present   bool
	Remaining float64
	ResetAt   time.Time
}

// advance treats a window whose cached reset time has passed as full again.
func (w autoSortWindow) advance(now time.Time, length time.Duration) autoSortWindow {
	if !w.Present || w.ResetAt.IsZero() || now.Before(w.ResetAt) {
		return w
	}
	w.Remaining = 1
	periods := now.Sub(w.ResetAt)/length + 1
	w.ResetAt = w.ResetAt.Add(periods * length)
	return w
}

type autoSortAccount struct {
	Primary   autoSortWindow
	Secondary autoSortWindow
	// CoolingUntil is CPA's own unavailable state; the zero time with Cooling
	// set means CPA reported no retry time.
	Cooling      bool
	CoolingUntil time.Time
}

type autoSortPrefixState struct {
	Accounts  []autoSortAccount
	Errors    int
	FetchedAt time.Time
}

type autoSorter struct {
	settings autoSortSettings
	now      func() time.Time

	mu       sync.RWMutex
	states   map[string]autoSortPrefixState
	marks    map[string]time.Time
	lifeOnce sync.Once
	cancel   context.CancelFunc
}

func newAutoSorter(settings autoSortSettings) *autoSorter {
	return &autoSorter{
		settings: settings,
		now:      time.Now,
		states:   make(map[string]autoSortPrefixState),
		marks:    make(map[string]time.Time),
	}
}

func (s *autoSorter) isMember(prefix string) bool {
	for _, member := range s.settings.Prefixes {
		if member == prefix {
			return true
		}
	}
	return false
}

// memberPrefixOf returns the longest participating prefix of a prefixed model.
func (s *autoSorter) memberPrefixOf(model string) (string, string) {
	model = strings.TrimSpace(model)
	best := ""
	for _, member := range s.settings.Prefixes {
		if strings.HasPrefix(model, member+"/") && len(member) > len(best) {
			best = member
		}
	}
	if best == "" {
		return "", ""
	}
	return best, strings.TrimPrefix(model, best+"/")
}

// markExhausted records a quota failure observed by a real request so that the
// prefix is demoted immediately instead of waiting for the next refresh.
func (s *autoSorter) markExhausted(prefix string) {
	if !s.isMember(prefix) {
		return
	}
	s.mu.Lock()
	s.marks[prefix] = s.now()
	s.mu.Unlock()
}

func (s *autoSorter) storeSnapshot(states map[string]autoSortPrefixState, startedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = states
	for prefix, markedAt := range s.marks {
		if markedAt.Before(startedAt) {
			delete(s.marks, prefix)
		}
	}
}

type autoSortRank struct {
	prefix string
	index  int
	tier   int
	key1   float64
	key2   float64
}

const (
	autoSortTierSafe = iota + 1
	autoSortTierUsable
	autoSortTierUnknown
	autoSortTierExhausted
	autoSortTierEmpty
)

// order returns every participating prefix except nominal, best first.
func (s *autoSorter) order(nominal string) []string {
	nominal = normalizePrefixName(nominal)
	now := s.now()
	s.mu.RLock()
	ranks := make([]autoSortRank, 0, len(s.settings.Prefixes))
	for index, prefix := range s.settings.Prefixes {
		if prefix == nominal {
			continue
		}
		state, known := s.states[prefix]
		_, marked := s.marks[prefix]
		rank := s.rankPrefix(state, known, marked, now)
		rank.prefix, rank.index = prefix, index
		ranks = append(ranks, rank)
	}
	s.mu.RUnlock()
	sort.SliceStable(ranks, func(i, j int) bool {
		a, b := ranks[i], ranks[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.key1 != b.key1 {
			return a.key1 < b.key1
		}
		if a.key2 != b.key2 {
			return a.key2 < b.key2
		}
		return a.index < b.index
	})
	out := make([]string, 0, len(ranks))
	for _, rank := range ranks {
		out = append(out, rank.prefix)
	}
	return out
}

// rankPrefix assigns a tier and ascending sort keys. Smaller keys sort first.
func (s *autoSorter) rankPrefix(state autoSortPrefixState, known, marked bool, now time.Time) autoSortRank {
	if !known || state.FetchedAt.IsZero() {
		return autoSortRank{tier: autoSortTierUnknown}
	}
	if len(state.Accounts) == 0 {
		if state.Errors > 0 {
			return autoSortRank{tier: autoSortTierUnknown}
		}
		return autoSortRank{tier: autoSortTierEmpty}
	}
	usable := make([]autoSortAccount, 0, len(state.Accounts))
	recovery := math.Inf(1)
	for _, raw := range state.Accounts {
		account := raw
		account.Primary = account.Primary.advance(now, codexPrimaryWindowLength)
		account.Secondary = account.Secondary.advance(now, codexSecondaryWindowLength)
		if marked {
			// Weekly-only accounts must be demoted too, not just 5h windows.
			if account.Primary.Present {
				account.Primary.Remaining = 0
			} else if account.Secondary.Present {
				account.Secondary.Remaining = 0
			}
		}
		if accountUsable(account, now) {
			usable = append(usable, account)
			continue
		}
		recovery = math.Min(recovery, accountRecovery(account, now))
	}
	if len(usable) == 0 {
		return autoSortRank{tier: autoSortTierExhausted, key1: recovery}
	}
	if s.settings.Strategy == autoSortStrategyWeeklyUrgency {
		urgency, bestPrimary := 0.0, 0.0
		for _, account := range usable {
			urgency += weeklyUrgency(account, now)
			bestPrimary = math.Max(bestPrimary, primaryRemaining(account))
		}
		return autoSortRank{tier: autoSortTierSafe, key1: -urgency, key2: -bestPrimary}
	}
	earliestSafeReset, safeSurplus := math.Inf(1), 0.0
	hasSafe := false
	totalSurplus, bestPrimary := 0.0, 0.0
	for _, account := range usable {
		surplus := weeklySurplus(account, now)
		totalSurplus += surplus
		bestPrimary = math.Max(bestPrimary, primaryRemaining(account))
		if surplus > 0 && primaryRemaining(account) >= s.settings.FiveHourReserve {
			hasSafe = true
			safeSurplus += surplus
			earliestSafeReset = math.Min(earliestSafeReset, timeKey(account.Primary.ResetAt))
		}
	}
	if hasSafe {
		return autoSortRank{tier: autoSortTierSafe, key1: earliestSafeReset, key2: -safeSurplus}
	}
	return autoSortRank{tier: autoSortTierUsable, key1: -totalSurplus, key2: -bestPrimary}
}

func accountCooling(account autoSortAccount, now time.Time) bool {
	return account.Cooling && (account.CoolingUntil.IsZero() || account.CoolingUntil.After(now))
}

func accountUsable(account autoSortAccount, now time.Time) bool {
	if accountCooling(account, now) || (!account.Primary.Present && !account.Secondary.Present) {
		return false
	}
	if account.Primary.Present && account.Primary.Remaining <= 0 {
		return false
	}
	return !account.Secondary.Present || account.Secondary.Remaining > 0
}

// accountRecovery is the latest time among every blocking condition, as Unix
// seconds. Unknown recovery times sort last.
func accountRecovery(account autoSortAccount, now time.Time) float64 {
	recovery := float64(now.Unix())
	if accountCooling(account, now) {
		recovery = math.Max(recovery, timeKey(account.CoolingUntil))
	}
	for _, window := range []autoSortWindow{account.Primary, account.Secondary} {
		if window.Present && window.Remaining <= 0 {
			recovery = math.Max(recovery, timeKey(window.ResetAt))
		}
	}
	if !account.Primary.Present && !account.Secondary.Present {
		return math.Inf(1)
	}
	return recovery
}

func timeKey(t time.Time) float64 {
	if t.IsZero() {
		return math.Inf(1)
	}
	return float64(t.Unix())
}

func primaryRemaining(account autoSortAccount) float64 {
	if !account.Primary.Present {
		return 1
	}
	return account.Primary.Remaining
}

// weeklySurplus estimates the weekly quota the owner will not use before the
// weekly reset, projecting the owner's observed pace over the rest of the week.
func weeklySurplus(account autoSortAccount, now time.Time) float64 {
	window := account.Secondary
	if !window.Present {
		return 1
	}
	remainingTime := codexSecondaryWindowLength
	if !window.ResetAt.IsZero() {
		remainingTime = window.ResetAt.Sub(now)
	}
	remainingTime = max(0, min(remainingTime, codexSecondaryWindowLength))
	elapsed := codexSecondaryWindowLength - remainingTime
	var expectedUse float64
	if elapsed < autoSortMinObservedWeek {
		expectedUse = remainingTime.Hours() / codexSecondaryWindowLength.Hours()
	} else {
		expectedUse = (1 - window.Remaining) / elapsed.Hours() * remainingTime.Hours()
	}
	return window.Remaining - expectedUse
}

func weeklyUrgency(account autoSortAccount, now time.Time) float64 {
	window := account.Secondary
	if !window.Present || window.ResetAt.IsZero() {
		return 0
	}
	hours := math.Max(window.ResetAt.Sub(now).Hours(), autoSortMinUrgencyHours)
	return window.Remaining / hours
}

func autoSortAccountFromQuota(quota pluginapi.QuotaFetchResponse) autoSortAccount {
	var account autoSortAccount
	for _, group := range quota.Groups {
		for _, bucket := range group.Buckets {
			window := autoSortWindow{Present: true, Remaining: bucket.RemainingFraction}
			if resetAt, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil {
				window.ResetAt = resetAt
			}
			switch bucket.Window {
			case "primary":
				account.Primary = window
			case "secondary":
				account.Secondary = window
			}
		}
	}
	return account
}

func (r *Runtime) autoSortActive() bool {
	return r != nil && r.autoSort != nil && r.config.QuotaFallback.Enabled && r.config.AutoSort.Enabled
}

// refreshAutoSort queries every enabled Codex credential under a participating
// prefix. It bypasses the request quota cache so a refresh always reflects
// live upstream state.
func (r *Runtime) refreshAutoSort(ctx context.Context) error {
	sorter := r.autoSort
	startedAt := sorter.now()
	listRaw, err := r.host.Call(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return fmt.Errorf("list CPA auths: %w", err)
	}
	var listed hostAuthListResponse
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		return fmt.Errorf("decode CPA auth list: %w", err)
	}
	states := make(map[string]autoSortPrefixState, len(sorter.settings.Prefixes))
	for _, entry := range listed.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Disabled || !strings.EqualFold(strings.TrimSpace(entry.Provider), quotaCodexProvider) {
			continue
		}
		rawAuth, errGet := r.getAuthJSON(entry.AuthIndex)
		if errGet != nil {
			continue
		}
		material := decodeAuthMaterial(rawAuth)
		prefix := material.prefix()
		if !sorter.isMember(prefix) {
			continue
		}
		state := states[prefix]
		quota, errQuota := fetchCodexQuota(ctx, pluginapi.QuotaFetchRequest{
			AuthIndex:   entry.AuthIndex,
			AuthID:      entry.ID,
			Provider:    quotaCodexProvider,
			StorageJSON: rawAuth,
			Metadata:    material.Metadata,
			Attributes:  material.Attributes,
			HTTPClient:  &quotaHTTPClient{host: r.host},
		})
		if errQuota != nil {
			state.Errors++
			states[prefix] = state
			continue
		}
		account := autoSortAccountFromQuota(quota)
		account.Cooling = entry.Unavailable
		account.CoolingUntil = entry.NextRetryAfter
		state.Accounts = append(state.Accounts, account)
		states[prefix] = state
	}
	fetchedAt := sorter.now()
	for _, prefix := range sorter.settings.Prefixes {
		state := states[prefix]
		state.FetchedAt = fetchedAt
		states[prefix] = state
	}
	sorter.storeSnapshot(states, startedAt)
	return nil
}

// Start launches background work owned by this runtime. It is safe to call
// more than once.
func (r *Runtime) Start() {
	if !r.autoSortActive() {
		return
	}
	sorter := r.autoSort
	sorter.lifeOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		sorter.mu.Lock()
		sorter.cancel = cancel
		sorter.mu.Unlock()
		go r.autoSortLoop(ctx)
	})
}

// Close stops background work and drops idle catalog connections. Host calls
// already in flight are not awaited because the plugin ABI cannot cancel them.
func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.catalog.close()
	if r.autoSort == nil {
		return
	}
	sorter := r.autoSort
	sorter.lifeOnce.Do(func() {})
	sorter.mu.Lock()
	cancel := sorter.cancel
	sorter.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Runtime) autoSortLoop(ctx context.Context) {
	delay := autoSortInitialDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := r.refreshAutoSort(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logBackground("warn", "user-routing auto-sort refresh failed", map[string]any{"error": err.Error()})
			delay = min(r.config.AutoSort.RefreshInterval, autoSortRetryDelay)
			continue
		}
		r.logBackground("debug", "user-routing auto-sort refreshed", map[string]any{"ranking": r.autoSort.order("")})
		delay = r.config.AutoSort.RefreshInterval
	}
}

func (r *Runtime) logBackground(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = PluginIdentifier
	_, _ = r.host.Call(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: message, Fields: fields})
}
