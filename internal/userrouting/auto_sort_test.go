package userrouting

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var autoSortTestNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func testAutoSorter(strategy string, prefixes ...string) *autoSorter {
	sorter := newAutoSorter(autoSortSettings{
		Enabled:         true,
		Strategy:        strategy,
		RefreshInterval: defaultAutoSortRefreshInterval,
		FiveHourReserve: defaultAutoSortFiveHourReserve,
		Prefixes:        prefixes,
	})
	sorter.now = func() time.Time { return autoSortTestNow }
	return sorter
}

func codexAccount(primary float64, primaryReset time.Duration, secondary float64, secondaryReset time.Duration) autoSortAccount {
	return autoSortAccount{
		Primary:   autoSortWindow{Present: true, Remaining: primary, ResetAt: autoSortTestNow.Add(primaryReset)},
		Secondary: autoSortWindow{Present: true, Remaining: secondary, ResetAt: autoSortTestNow.Add(secondaryReset)},
	}
}

func setAutoSortState(sorter *autoSorter, prefix string, accounts ...autoSortAccount) {
	sorter.states[prefix] = autoSortPrefixState{Accounts: accounts, FetchedAt: autoSortTestNow}
}

const day = 24 * time.Hour

func TestNormalizeAutoSortDefaultsAndValidation(t *testing.T) {
	settings, err := normalizeAutoSort(autoSortConfig{Enabled: true, Prefixes: []string{"a/", " b ", "a"}})
	if err != nil {
		t.Fatalf("normalizeAutoSort() error = %v", err)
	}
	if settings.Strategy != autoSortStrategyBorrow || settings.FiveHourReserve != 0.25 || settings.RefreshInterval != 5*time.Minute {
		t.Fatalf("defaults = %+v", settings)
	}
	if !equalStrings(settings.Prefixes, []string{"a", "b"}) {
		t.Fatalf("prefixes = %#v", settings.Prefixes)
	}
	invalid := []autoSortConfig{
		{Enabled: true, Prefixes: []string{"a"}},
		{Enabled: true, Strategy: "random", Prefixes: []string{"a", "b"}},
		{Enabled: true, RefreshInterval: "10s", Prefixes: []string{"a", "b"}},
		{Enabled: true, Prefixes: []string{"a", " / "}},
	}
	for _, input := range invalid {
		if _, err := normalizeAutoSort(input); err == nil {
			t.Fatalf("normalizeAutoSort(%+v) error = nil", input)
		}
	}
}

func TestFiveHourReserveAcceptsFractionPercentAndPercentString(t *testing.T) {
	for raw, want := range map[string]float64{"0.3": 0.3, "30": 0.3, `"30%"`: 0.3, "1": 1, "0": 0} {
		var cfg autoSortConfig
		if err := yaml.Unmarshal([]byte("five_hour_reserve: "+raw), &cfg); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", raw, err)
		}
		if math.Abs(cfg.FiveHourReserve.Value-want) > 1e-9 || !cfg.FiveHourReserve.Set {
			t.Fatalf("five_hour_reserve %s = %+v, want %v", raw, cfg.FiveHourReserve, want)
		}
	}
	for _, raw := range []string{"-1", `"150%"`, "abc"} {
		var cfg autoSortConfig
		if err := yaml.Unmarshal([]byte("five_hour_reserve: "+raw), &cfg); err == nil {
			t.Fatalf("Unmarshal(%s) error = nil", raw)
		}
	}
}

func TestDecodeRuntimeConfigReadsAutoSort(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	raw := "cpa_config_path: " + quotedYAML(path) + "\n" +
		"quota_fallback:\n  enabled: true\n  auto_sort:\n    enabled: true\n    strategy: weekly_urgency\n    refresh_interval: 2m\n    five_hour_reserve: 40%\n    prefixes: [team-a, team-b/]\n"
	runtime, err := NewRuntime(&fakeHost{}, []byte(raw), ConfigureOptions{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	got := runtime.config.AutoSort
	if !got.Enabled || got.Strategy != autoSortStrategyWeeklyUrgency || got.RefreshInterval != 2*time.Minute || math.Abs(got.FiveHourReserve-0.4) > 1e-9 || !equalStrings(got.Prefixes, []string{"team-a", "team-b"}) {
		t.Fatalf("AutoSort = %+v", got)
	}
	if !runtime.autoSortActive() {
		t.Fatal("autoSortActive() = false")
	}
}

func TestWeeklySurplusProjectsOwnerPace(t *testing.T) {
	// Four days elapsed with 20% used: the owner needs about 15% more.
	steady := codexAccount(1, time.Hour, 0.8, 3*day)
	if got := weeklySurplus(steady, autoSortTestNow); math.Abs(got-0.65) > 1e-9 {
		t.Fatalf("steady surplus = %v, want 0.65", got)
	}
	heavy := codexAccount(1, time.Hour, 0.3, 3*day)
	if got := weeklySurplus(heavy, autoSortTestNow); got >= 0 {
		t.Fatalf("heavy surplus = %v, want negative", got)
	}
	// Less than a day of history assumes the owner uses the full remainder.
	early := codexAccount(1, time.Hour, 0.95, 7*day-12*time.Hour)
	want := 0.95 - (156.0 / 168.0)
	if got := weeklySurplus(early, autoSortTestNow); math.Abs(got-want) > 1e-9 {
		t.Fatalf("early surplus = %v, want %v", got, want)
	}
}

func TestBorrowOrderProtectsOwners(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "nominal", "later-reset", "soon-reset", "owner-busy", "unknown", "five-hour-out", "weekly-out", "heavy-user")
	setAutoSortState(sorter, "nominal", codexAccount(0, time.Hour, 0.5, 3*day))
	setAutoSortState(sorter, "later-reset", codexAccount(0.9, 4*time.Hour, 0.9, 3*day))
	setAutoSortState(sorter, "soon-reset", codexAccount(0.6, 20*time.Minute, 0.7, 3*day))
	setAutoSortState(sorter, "owner-busy", codexAccount(0.1, 10*time.Minute, 0.9, 3*day))
	setAutoSortState(sorter, "five-hour-out", codexAccount(0, time.Hour, 0.9, 3*day))
	setAutoSortState(sorter, "weekly-out", codexAccount(0.8, time.Hour, 0, 2*day))
	setAutoSortState(sorter, "heavy-user", codexAccount(0.9, 10*time.Minute, 0.2, 3*day))

	got := sorter.order("nominal")
	want := []string{"soon-reset", "later-reset", "owner-busy", "heavy-user", "unknown", "five-hour-out", "weekly-out"}
	if !equalStrings(got, want) {
		t.Fatalf("order() = %#v, want %#v", got, want)
	}
}

func TestBorrowReserveIsConfigurable(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "a", "b", "c")
	sorter.settings.FiveHourReserve = 0.5
	setAutoSortState(sorter, "b", codexAccount(0.4, 10*time.Minute, 0.9, 3*day))
	setAutoSortState(sorter, "c", codexAccount(0.6, 3*time.Hour, 0.9, 3*day))
	if got := sorter.order("a"); !equalStrings(got, []string{"c", "b"}) {
		t.Fatalf("order() = %#v, want c before b", got)
	}
	sorter.settings.FiveHourReserve = 0.25
	if got := sorter.order("a"); !equalStrings(got, []string{"b", "c"}) {
		t.Fatalf("order() = %#v, want b before c", got)
	}
}

func TestWeeklyUrgencyOrderPrefersQuotaExpiringSoon(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyWeeklyUrgency, "a", "relaxed", "urgent", "exhausted")
	setAutoSortState(sorter, "relaxed", codexAccount(0.9, time.Hour, 0.9, 6*day))
	setAutoSortState(sorter, "urgent", codexAccount(0.2, time.Hour, 0.9, 6*time.Hour))
	setAutoSortState(sorter, "exhausted", codexAccount(0, time.Hour, 0.9, 6*time.Hour))
	if got, want := sorter.order("a"), []string{"urgent", "relaxed", "exhausted"}; !equalStrings(got, want) {
		t.Fatalf("order() = %#v, want %#v", got, want)
	}
}

func TestAutoSortTreatsPassedResetAsRecovered(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "a", "stale", "other")
	setAutoSortState(sorter, "stale", codexAccount(0, -time.Minute, 0.9, 3*day))
	setAutoSortState(sorter, "other", codexAccount(0, time.Hour, 0.9, 3*day))
	if got := sorter.order("a"); !equalStrings(got, []string{"stale", "other"}) {
		t.Fatalf("order() = %#v, want recovered stale prefix first", got)
	}
}

func TestAutoSortCPACoolingAccountIsNotUsable(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "a", "cooling", "ready")
	cooling := codexAccount(0.9, time.Hour, 0.9, 3*day)
	cooling.Cooling, cooling.CoolingUntil = true, autoSortTestNow.Add(time.Hour)
	setAutoSortState(sorter, "cooling", cooling)
	setAutoSortState(sorter, "ready", codexAccount(0.3, 4*time.Hour, 0.2, 3*day))
	if got := sorter.order("a"); !equalStrings(got, []string{"ready", "cooling"}) {
		t.Fatalf("order() = %#v", got)
	}
}

func TestMarkExhaustedDemotesUntilNextRefresh(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "a", "b", "c")
	setAutoSortState(sorter, "b", codexAccount(0.9, 10*time.Minute, 0.9, 3*day))
	setAutoSortState(sorter, "c", codexAccount(0.9, 3*time.Hour, 0.9, 3*day))
	sorter.markExhausted("b")
	if got := sorter.order("a"); !equalStrings(got, []string{"c", "b"}) {
		t.Fatalf("order() after mark = %#v", got)
	}
	states := map[string]autoSortPrefixState{
		"b": {Accounts: []autoSortAccount{codexAccount(0.9, 10*time.Minute, 0.9, 3*day)}, FetchedAt: autoSortTestNow},
		"c": sorter.states["c"],
	}
	sorter.storeSnapshot(states, autoSortTestNow.Add(time.Second))
	if got := sorter.order("a"); !equalStrings(got, []string{"b", "c"}) {
		t.Fatalf("order() after refresh = %#v", got)
	}
}

func TestQuotaFallbackTargetsUseAutoSortOnlyForMembers(t *testing.T) {
	r := &Runtime{config: runtimeConfig{
		QuotaFallback: quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"outsider": {"backup"}, "a": {"backup"}}},
		AutoSort:      autoSortSettings{Enabled: true},
	}}
	r.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b", "c")
	setAutoSortState(r.autoSort, "b", codexAccount(0.9, 3*time.Hour, 0.9, 3*day))
	setAutoSortState(r.autoSort, "c", codexAccount(0.9, 10*time.Minute, 0.9, 3*day))
	if got, want := r.quotaFallbackTargets("a/gpt-5"), []string{"a/gpt-5", "c/gpt-5", "b/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("member targets = %#v, want %#v", got, want)
	}
	if got, want := r.quotaFallbackTargets("outsider/gpt-5"), []string{"outsider/gpt-5", "backup/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("outsider targets = %#v, want %#v", got, want)
	}
	r.config.QuotaFallback.Enabled = false
	if got := r.quotaFallbackTargets("a/gpt-5"); !equalStrings(got, []string{"a/gpt-5"}) {
		t.Fatalf("disabled targets = %#v", got)
	}
}

func TestExecuteFollowsAutoSortAndMarksFailedPrefix(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5"})
	host := &quotaFallbackHost{quotaError: `host_call_failed: {"error":{"type":"usage_limit_reached"}}`}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"team-a": {"team-b"}}}
	runtime.config.AutoSort = autoSortSettings{Enabled: true}
	runtime.autoSort = testAutoSorter(autoSortStrategyBorrow, "team-a", "team-b", "team-c")
	setAutoSortState(runtime.autoSort, "team-a", codexAccount(0.9, time.Hour, 0.9, 3*day))
	setAutoSortState(runtime.autoSort, "team-b", codexAccount(0.9, 4*time.Hour, 0.9, 3*day))
	setAutoSortState(runtime.autoSort, "team-c", codexAccount(0.9, 10*time.Minute, 0.9, 3*day))

	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
		OriginalRequest: []byte(`{"model":"gpt-5"}`),
	}, "callback-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := host.models, []string{"team-a/gpt-5", "team-c/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
	if got := runtime.autoSort.order("team-b"); !equalStrings(got, []string{"team-c", "team-a"}) {
		t.Fatalf("team-a was not demoted after quota failure: %#v", got)
	}
}

func TestRefreshAutoSortCollectsParticipatingCodexAccounts(t *testing.T) {
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"a1":    {prefix: "a", email: "a1@example.com", token: "token-a1", remaining: 0.7, hasSecondary: true, secondaryRemaining: 0.6},
		"b1":    {prefix: "b", email: "b1@example.com", token: "token-b1", remaining: 0.2, hasSecondary: true, secondaryRemaining: 0.9, unavailable: true, retryAt: autoSortTestNow.Add(time.Hour)},
		"other": {prefix: "outside", email: "o@example.com", token: "token-o", remaining: 1},
	}}
	r := &Runtime{host: host, config: runtimeConfig{QuotaFallback: quotaFallbackConfig{Enabled: true}, AutoSort: autoSortSettings{Enabled: true}}}
	r.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b", "c")
	if err := r.refreshAutoSort(context.Background()); err != nil {
		t.Fatalf("refreshAutoSort() error = %v", err)
	}
	a := r.autoSort.states["a"]
	if len(a.Accounts) != 1 || math.Abs(a.Accounts[0].Primary.Remaining-0.7) > 1e-9 || math.Abs(a.Accounts[0].Secondary.Remaining-0.6) > 1e-9 || a.Accounts[0].Primary.ResetAt.IsZero() {
		t.Fatalf("state a = %+v", a)
	}
	b := r.autoSort.states["b"]
	if len(b.Accounts) != 1 || !b.Accounts[0].Cooling || !b.Accounts[0].CoolingUntil.Equal(autoSortTestNow.Add(time.Hour)) {
		t.Fatalf("state b = %+v", b)
	}
	if c, ok := r.autoSort.states["c"]; !ok || len(c.Accounts) != 0 || c.FetchedAt.IsZero() {
		t.Fatalf("state c = %+v", c)
	}
	if _, ok := r.autoSort.states["outside"]; ok {
		t.Fatal("non-participating prefix was cached")
	}
	if host.usageCalls != 2 {
		t.Fatalf("usage calls = %d, want 2", host.usageCalls)
	}
}

func TestQuotaResourceActualPrefixFollowsAutoSort(t *testing.T) {
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"a": {prefix: "a", email: "a@example.com", token: "token-a", remaining: 0, hasSecondary: true, secondaryRemaining: 0.5},
		"b": {prefix: "b", email: "b@example.com", token: "token-b", remaining: 0.9, hasSecondary: true, secondaryRemaining: 0.9},
		"c": {prefix: "c", email: "c@example.com", token: "token-c", remaining: 0.9, hasSecondary: true, secondaryRemaining: 0.9},
	}}
	r := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, PrefixMap: PrefixMap{"key-1": "a/"}, CPAConfig: NewCPAConfigReader(writeCPAConfig(t, "key-1")),
		QuotaFallback: quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"a": {"b"}}},
		AutoSort:      autoSortSettings{Enabled: true},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
	}}
	r.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b", "c")
	setAutoSortState(r.autoSort, "b", codexAccount(0.9, 4*time.Hour, 0.9, 3*day))
	setAutoSortState(r.autoSort, "c", codexAccount(0.9, 10*time.Minute, 0.9, 3*day))
	resp, err := r.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"), Path: "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	var result quotaResourceResponse
	if err != nil || resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &result) != nil {
		t.Fatalf("status=%d err=%v body=%s", resp.StatusCode, err, resp.Body)
	}
	if result.NominalPrefix != "a/" || result.ActualPrefix != "c/" {
		t.Fatalf("prefixes = (%q, %q), want (a/, c/)", result.NominalPrefix, result.ActualPrefix)
	}
}

func TestRuntimeCloseBeforeStartPreventsBackgroundRefresh(t *testing.T) {
	r := &Runtime{host: &fakeHost{}, config: runtimeConfig{QuotaFallback: quotaFallbackConfig{Enabled: true}, AutoSort: autoSortSettings{Enabled: true, RefreshInterval: time.Minute}}}
	r.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b")
	r.Close()
	r.Start()
	if r.autoSort.cancel != nil {
		t.Fatal("Start() after Close() launched the refresher")
	}
	started := &Runtime{host: &fakeHost{}, config: r.config}
	started.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b")
	started.Start()
	started.Start()
	if started.autoSort.cancel == nil {
		t.Fatal("Start() did not launch the refresher")
	}
	started.Close()
}

func TestConfigExampleDecodesWithAutoSortEnabled(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	var root struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode config.example.yaml: %v", err)
	}
	node := root.Plugins.Configs[PluginIdentifier]
	var block map[string]any
	if err := node.Decode(&block); err != nil {
		t.Fatalf("decode plugin block: %v", err)
	}
	examplePath, err := filepath.Abs(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	block["cpa_config_path"] = examplePath
	block["quota_fallback"].(map[string]any)["enabled"] = true
	block["quota_fallback"].(map[string]any)["auto_sort"].(map[string]any)["enabled"] = true
	pluginRaw, err := yaml.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeRuntimeConfig(pluginRaw, ConfigureOptions{})
	if err != nil {
		t.Fatalf("decodeRuntimeConfig(example) error = %v", err)
	}
	if !cfg.AutoSort.Enabled || cfg.AutoSort.Strategy != autoSortStrategyBorrow || cfg.AutoSort.FiveHourReserve != 0.25 || len(cfg.AutoSort.Prefixes) != 3 {
		t.Fatalf("example AutoSort = %+v", cfg.AutoSort)
	}
}

func TestStartRefreshesInBackground(t *testing.T) {
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"b1": {prefix: "b", email: "b1@example.com", token: "token-b1", remaining: 0.9, hasSecondary: true, secondaryRemaining: 0.9},
	}}
	r := &Runtime{host: host, config: runtimeConfig{
		QuotaFallback: quotaFallbackConfig{Enabled: true},
		AutoSort:      autoSortSettings{Enabled: true, RefreshInterval: time.Minute},
	}}
	r.autoSort = testAutoSorter(autoSortStrategyBorrow, "a", "b")
	r.Start()
	defer r.Close()
	deadline := time.Now().Add(autoSortInitialDelay + 5*time.Second)
	for time.Now().Before(deadline) {
		r.autoSort.mu.RLock()
		state, ok := r.autoSort.states["b"]
		r.autoSort.mu.RUnlock()
		if ok && len(state.Accounts) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background refresh did not populate the snapshot")
}
