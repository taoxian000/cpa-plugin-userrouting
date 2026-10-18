package userrouting

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseCodexQuotaResponse(t *testing.T) {
	response, err := parseCodexQuotaResponse([]byte(`{
      "plan_type":"pro",
      "rate_limit":{
		"primary_window":{"used_percent":25,"reset_at":1900000000},
		"secondary_window":{"used_percent":80,"reset_after_seconds":120}
		},
		"credits":{"balance":3},
		"rate_limit_reset_credits":{"available_count":2}
	}`))
	if err != nil {
		t.Fatalf("parseCodexQuotaResponse() error = %v", err)
	}
	if response.Subscription == nil || response.Subscription.Plan != "pro" {
		t.Fatalf("subscription = %#v, want pro", response.Subscription)
	}
	if len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %#v, want two buckets", response.Groups)
	}
	if got := response.Groups[0].Buckets[0].RemainingFraction; got != 0.75 {
		t.Fatalf("primary remaining = %v, want 0.75", got)
	}
	if got := response.Groups[0].Buckets[0].ResetTime; got != time.Unix(1900000000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("primary reset = %q", got)
	}
	if len(response.Summary) != 2 || response.Summary[0].Value != 3 || response.Summary[1].Key != "rate_limit_reset_credits_available" || response.Summary[1].Value != 2 {
		t.Fatalf("summary = %#v, want balance and reset-credit count", response.Summary)
	}
}

func TestParseCodexResetCredits(t *testing.T) {
	info, err := parseCodexResetCredits([]byte(`{
		"available_count":2,
		"credits":[
			{"id":"private-id-1","reset_type":"codex_rate_limits","status":"available","expires_at":"2030-07-17T12:30:00Z"},
			{"id":"private-id-2","reset_type":"codex_rate_limits","status":"available","expires_at":null},
			{"id":"private-id-3","reset_type":"other","status":"available","expires_at":"2030-07-18T00:00:00Z"},
			{"id":"private-id-4","reset_type":"codex_rate_limits","status":"redeemed","expires_at":"2030-07-19T00:00:00Z"}
		]
	}`), nil)
	if err != nil {
		t.Fatalf("parseCodexResetCredits() error = %v", err)
	}
	if info.AvailableCount != 2 || len(info.ExpiresAt) != 1 || info.ExpiresAt[0] != "2030-07-17T12:30:00Z" || info.WithoutExpiry != 1 || !info.ExpiryDetailsAvailable || !info.ExpiryDetailsComplete {
		t.Fatalf("reset-credit info = %#v", info)
	}
}

func TestFirstAvailableCodexResetCreditID(t *testing.T) {
	raw := []byte(`{"credits":[
		{"id":"wrong-type","reset_type":"unknown","status":"available"},
		{"id":"already-used","reset_type":"codex_rate_limits","status":"redeemed"},
		{"id":"selected","reset_type":"codex_rate_limits","status":"available"}
	]}`)
	if got := firstAvailableCodexResetCreditID(raw); got != "selected" {
		t.Fatalf("firstAvailableCodexResetCreditID() = %q, want selected", got)
	}
}

func TestCodexUsageURL(t *testing.T) {
	for _, test := range []struct {
		base string
		want string
	}{
		{base: "", want: "https://chatgpt.com/backend-api/wham/usage"},
		{base: "https://chatgpt.com/backend-api/codex", want: "https://chatgpt.com/backend-api/wham/usage"},
	} {
		got, err := codexUsageURL(test.base)
		if err != nil || got != test.want {
			t.Fatalf("codexUsageURL(%q) = (%q, %v), want (%q, nil)", test.base, got, err, test.want)
		}
	}
	if got, err := codexResetCreditsURL("https://chatgpt.com/backend-api/codex"); err != nil || got != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("codexResetCreditsURL() = (%q, %v)", got, err)
	}
	if got, err := codexResetConsumeURL(""); err != nil || got != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume" {
		t.Fatalf("codexResetConsumeURL() = (%q, %v)", got, err)
	}
}

func TestDescribeQuotaAdvertisesReset(t *testing.T) {
	runtime := &Runtime{config: runtimeConfig{Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true}}}
	if response := runtime.DescribeQuota(context.Background()); !response.SupportsReset {
		t.Fatalf("DescribeQuota() = %#v, want SupportsReset", response)
	}
}

func TestQuotaResourceReturnsNominalAndActualPrefixes(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{
		auths: map[string]quotaTestAuth{
			"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", remaining: 0, hasSecondary: true, secondaryRemaining: 0.82},
			"auth-2": {prefix: "prefix2", email: "two@example.com", token: "token-2", remaining: 0.5, hasSecondary: true, secondaryRemaining: 0.4},
		},
	}
	runtime := &Runtime{
		host: host,
		config: runtimeConfig{
			Enabled:             true,
			PrefixMap:           PrefixMap{"key-1": "prefix1/", "default": ""},
			QuotaFallback:       quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"prefix1": {"prefix2"}}},
			QuotaProvider:       quotaProviderConfig{Enabled: true, PublicEndpoint: true},
			StrictKeyValidation: true,
			CPAConfig:           NewCPAConfigReader(path),
		},
	}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: bearerHeader("key-1"),
		Path:    "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil {
		t.Fatalf("HandleManagement() error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	var decoded quotaResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.NominalPrefix != "prefix1/" || decoded.ActualPrefix != "prefix2/" {
		t.Fatalf("top-level prefixes = (%q, %q), want (prefix1/, prefix2/)", decoded.NominalPrefix, decoded.ActualPrefix)
	}
	if len(decoded.NominalAccounts) != 1 || len(decoded.ActualAccounts) != 1 {
		t.Fatalf("selected accounts = (%#v, %#v), want one account for nominal and actual", decoded.NominalAccounts, decoded.ActualAccounts)
	}
	nominalAccount, ok := decoded.NominalAccounts["one@example.com"]
	if !ok {
		t.Fatalf("nominal account = %#v, want one@example.com", decoded.NominalAccounts)
	}
	if _, ok := decoded.ActualAccounts["two@example.com"]; !ok {
		t.Fatalf("actual account = %#v, want two@example.com", decoded.ActualAccounts)
	}
	if nominalAccount.ResetCredits.AvailableCount != 1 || len(nominalAccount.ResetCredits.ExpiresAt) != 1 || !nominalAccount.ResetCredits.ExpiryDetailsAvailable || !nominalAccount.ResetCredits.ExpiryDetailsComplete {
		t.Fatalf("nominal reset credits = %#v, body = %s, partial=%v errors=%v", nominalAccount.ResetCredits, response.Body, decoded.Partial, decoded.Errors)
	}
	if strings.Contains(string(response.Body), `"prefixes"`) {
		t.Fatalf("response exposes internal prefix list: %s", response.Body)
	}
	if strings.Contains(string(response.Body), "key-1") || strings.Contains(string(response.Body), "token-") || strings.Contains(string(response.Body), "private-credit-id") {
		t.Fatalf("response contains credential material: %s", response.Body)
	}
}

func TestQuotaPrefixHasRemainingRequiresAllWindowsToBePositive(t *testing.T) {
	quotaPrefix := func(primary, secondary float64) quotaPrefixResult {
		return quotaPrefixResult{Accounts: map[string]quotaResourceAccount{
			"account@example.com": {QuotaFetchResponse: pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{
				{Window: "primary", RemainingFraction: primary},
				{Window: "secondary", RemainingFraction: secondary},
			}}}}},
		}}
	}
	for _, test := range []struct {
		name      string
		primary   float64
		secondary float64
		want      bool
	}{
		{name: "both windows have quota", primary: 0.1, secondary: 0.8, want: true},
		{name: "primary exhausted", primary: 0, secondary: 0.8, want: false},
		{name: "secondary exhausted", primary: 0.5, secondary: 0, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := quotaPrefixHasRemaining(quotaPrefix(test.primary, test.secondary)); got != test.want {
				t.Fatalf("quotaPrefixHasRemaining() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestQuotaResourceOmitsActualAccountsWhenNominalPrefixHasQuota(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{
		auths: map[string]quotaTestAuth{
			"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", remaining: 0.5},
			"auth-2": {prefix: "prefix2", email: "two@example.com", token: "token-2", remaining: 0.8},
		},
	}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		PrefixMap:     PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaFallback: quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"prefix1": {"prefix2"}}},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		CPAConfig:     NewCPAConfigReader(path),
	}}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: bearerHeader("key-1"),
		Path:    "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v), want 200", response, err)
	}
	var decoded quotaResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.NominalPrefix != "prefix1/" || decoded.ActualPrefix != "prefix1/" || decoded.ActualAccounts != nil {
		t.Fatalf("response = %#v, want unchanged prefix fields and no actual_accounts", decoded)
	}
	if host.usageCalls != 1 || host.resetCreditsCalls != 1 {
		t.Fatalf("query counts = usage %d, reset credits %d; fallback account should not be queried", host.usageCalls, host.resetCreditsCalls)
	}
	if strings.Contains(string(response.Body), `"actual_accounts"`) {
		t.Fatalf("response contains duplicate actual accounts: %s", response.Body)
	}
}

func TestQuotaResourceCachesCodexByAuthIndexAndInvalidatesAfterResetOrCredentialChange(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", remaining: 0.5},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		PrefixMap:     PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		QuotaCacheTTL: time.Minute,
		CPAConfig:     NewCPAConfigReader(path),
	}}
	query := func() float64 {
		t.Helper()
		response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodGet, Headers: bearerHeader("key-1"), Path: "/v0/resource/plugins/user-routing/quota",
		}, "callback-1")
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("quota query = (%#v, %v), want 200", response, err)
		}
		var decoded quotaResourceResponse
		if err := json.Unmarshal(response.Body, &decoded); err != nil {
			t.Fatalf("decode quota response: %v", err)
		}
		return decoded.NominalAccounts["one@example.com"].Groups[0].Buckets[0].RemainingFraction
	}
	if got := query(); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("first quota = %v, want 0.5", got)
	}
	auth := host.auths["auth-1"]
	auth.remaining = 0.3
	host.auths["auth-1"] = auth
	if got := query(); math.Abs(got-0.5) > 1e-9 || host.usageCalls != 1 || host.resetCreditsCalls != 1 {
		t.Fatalf("cached quota = %v, usage calls=%d, credit calls=%d; want old value and one upstream read", got, host.usageCalls, host.resetCreditsCalls)
	}
	storage, _ := json.Marshal(map[string]string{"access_token": "token-1", "account_id": "account-auth-1"})
	reset := runtime.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{
		AuthIndex: "auth-1", Provider: quotaCodexProvider, StorageJSON: storage,
	}, "callback-1")
	if !reset.Success || host.consumeCalls != 1 {
		t.Fatalf("reset = %#v, consume calls=%d", reset, host.consumeCalls)
	}
	if got := query(); math.Abs(got-0.3) > 1e-9 || host.usageCalls != 2 || host.resetCreditsCalls != 3 {
		t.Fatalf("post-reset quota = %v, usage calls=%d, credit calls=%d; want fresh reads", got, host.usageCalls, host.resetCreditsCalls)
	}
	auth.token = "token-2"
	auth.remaining = 0.9
	host.auths["auth-1"] = auth
	if got := query(); math.Abs(got-0.91) > 1e-9 || host.usageCalls != 3 || host.resetCreditsCalls != 4 {
		t.Fatalf("changed-credential quota = %v, usage calls=%d, credit calls=%d; want fresh reads", got, host.usageCalls, host.resetCreditsCalls)
	}
	if host.authListCalls != 4 || host.authGetCalls != 4 {
		t.Fatalf("auth lookups = list %d, get %d; each request must recheck CPA auth", host.authListCalls, host.authGetCalls)
	}
}

func TestQuotaResourceQueriesAntigravityUsingManagementCenterAPI(t *testing.T) {
	const managementKey = "test-management-key"
	var apiCallRequests []managementAPICallRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != managementAPICallPath {
			t.Errorf("management request = %s %s, want POST %s", req.Method, req.URL.Path, managementAPICallPath)
		}
		if req.Header.Get("Authorization") != "Bearer "+managementKey {
			t.Errorf("management Authorization header = %q, want configured secret", req.Header.Get("Authorization"))
		}
		var body managementAPICallRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode management request: %v", err)
		}
		apiCallRequests = append(apiCallRequests, body)
		if body.AuthIndex != "auth-antigravity" {
			t.Errorf("authIndex = %q, want auth-antigravity", body.AuthIndex)
		}
		if body.Header["Authorization"] != "Bearer $TOKEN$" || body.Header["User-Agent"] != antigravityHubUserAgent {
			t.Errorf("Antigravity headers = %#v", body.Header)
		}
		var responseBody string
		switch body.URL {
		case antigravityQuotaURLs[0]:
			if body.Method != http.MethodPost || body.Data != `{"project":"project-123"}` {
				t.Errorf("Antigravity quota call = %#v, want project quota POST", body)
			}
			responseBody = `{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"weekly","remainingFraction":0.91,"resetTime":"2030-01-08T03:04:05Z"},{"window":"5h","remainingFraction":0.82,"resetTime":"2030-01-02T03:04:05Z"}]},{"displayName":"Claude and GPT models","buckets":[{"window":"weekly","remainingFraction":0.73,"resetTime":"2030-01-09T03:04:05Z"},{"window":"5h","remainingFraction":0.64,"resetTime":"2030-01-03T03:04:05Z"}]}]}`
		case antigravityLoadCodeAssistURL:
			if body.Method != http.MethodPost || body.Data != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
				t.Errorf("Antigravity subscription call = %#v, want loadCodeAssist POST", body)
			}
			responseBody = `{"paidTier":{"id":"g1-pro-tier","name":"Google One Pro"}}`
		default:
			t.Errorf("unexpected Antigravity API URL %q", body.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(managementAPICallEnvelope{
			StatusCode: http.StatusOK,
			Header:     map[string][]string{"date": {time.Now().UTC().Format(http.TimeFormat)}},
			Body:       responseBody,
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv("MANAGEMENT_PASSWORD", managementKey)

	path := writeCPAConfigForServer(t, server.URL, "key-1")
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-antigravity": {provider: "antigravity", prefix: "prefix2", email: "alice@example.com", projectID: "project-123"},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		PrefixMap:     PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		QuotaCacheTTL: time.Minute,
		CPAConfig:     NewCPAConfigReader(path),
	}}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: bearerHeader("key-1"),
		Query:   url.Values{"provider": {"Antigravity"}, "email": {"ALICE@example.com"}},
		Path:    "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v), want 200", response, err)
	}
	var decoded providerQuotaResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	account, ok := decoded.NominalAccounts["alice@example.com"]
	if !ok || account.Subscription == nil || account.Subscription.Plan != "pro" || account.Subscription.TierID != "g1-pro-tier" || len(account.Groups) != 2 {
		t.Fatalf("quota response = %#v, body=%s", decoded, response.Body)
	}
	for groupIndex, want := range []struct {
		name      string
		fractions [2]float64
	}{
		{"Gemini Models", [2]float64{0.91, 0.82}},
		{"Claude and GPT models", [2]float64{0.73, 0.64}},
	} {
		group := account.Groups[groupIndex]
		if group.DisplayName != want.name || len(group.Buckets) != 2 || group.Buckets[0].Window != "secondary" || group.Buckets[0].SourceWindow != "weekly" || group.Buckets[0].RemainingFraction != want.fractions[0] || group.Buckets[1].Window != "primary" || group.Buckets[1].SourceWindow != "5h" || group.Buckets[1].RemainingFraction != want.fractions[1] {
			t.Fatalf("group %d lost or changed one of four quota values: %#v, body=%s", groupIndex, group, response.Body)
		}
	}
	if strings.Contains(string(response.Body), `"nominal_prefix"`) || strings.Contains(string(response.Body), `"actual_prefix"`) || strings.Contains(string(response.Body), `"actual_accounts"`) || strings.Contains(string(response.Body), `"QuotaBucket"`) {
		t.Fatalf("explicit provider lookup should have flattened buckets and no prefix fields: %s", response.Body)
	}
	if len(apiCallRequests) != 2 || apiCallRequests[0].AuthIndex != "auth-antigravity" || apiCallRequests[1].AuthIndex != "auth-antigravity" {
		t.Fatalf("api-call requests = %#v, want quota and subscription calls for selected auth", apiCallRequests)
	}
	if strings.Contains(string(response.Body), managementKey) || strings.Contains(string(response.Body), "project-123") || host.authListCalls != 1 || host.authGetCalls != 1 {
		t.Fatalf("secret leaked or auth lookup mismatch: body=%s auth-list=%d auth-get=%d", response.Body, host.authListCalls, host.authGetCalls)
	}
	second, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"antigravity"}, "email": {"alice@example.com"}},
		Path:  "/v0/resource/plugins/user-routing/quota",
	}, "callback-2")
	if err != nil || second.StatusCode != http.StatusOK || len(apiCallRequests) != 2 || host.authListCalls != 2 || host.authGetCalls != 2 {
		t.Fatalf("cached Antigravity query = (%#v, %v), upstream calls=%d auth-list=%d auth-get=%d", second, err, len(apiCallRequests), host.authListCalls, host.authGetCalls)
	}
}

func TestQuotaResourceQueriesClaudeUsingManagementCenterAPI(t *testing.T) {
	const managementKey = "test-management-key"
	var apiCallRequests []managementAPICallRequest
	usageAttempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != managementAPICallPath {
			t.Errorf("management request = %s %s, want POST %s", req.Method, req.URL.Path, managementAPICallPath)
		}
		if req.Header.Get("Authorization") != "Bearer "+managementKey {
			t.Errorf("management Authorization header = %q, want configured secret", req.Header.Get("Authorization"))
		}
		var body managementAPICallRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode management request: %v", err)
		}
		apiCallRequests = append(apiCallRequests, body)
		if body.AuthIndex != "auth-claude" || body.Method != http.MethodGet || body.Header["Authorization"] != "Bearer $TOKEN$" || body.Header["User-Agent"] != "claude-cli/2.1.280 (external, cli)" || body.Header["anthropic-beta"] != "oauth-2025-04-20" {
			t.Errorf("Claude api-call request = %#v", body)
		}
		var responseBody string
		statusCode := http.StatusOK
		switch body.URL {
		case claudeUsageURL:
			usageAttempts++
			if usageAttempts <= quotaQueryRetryCount {
				statusCode = http.StatusServiceUnavailable
				responseBody = `{"error":"temporary upstream failure"}`
				break
			}
			responseBody = `{"five_hour":{"utilization":42.5,"resets_at":"2030-01-02T03:04:05Z"},"seven_day":{"utilization":18,"resets_at":"2030-01-08T03:04:05Z"},"extra_usage":{"monthly_limit":100,"used_credits":12,"utilization":12}}`
		case claudeProfileURL:
			responseBody = `{"account":{"has_claude_pro":true,"has_claude_max":false}}`
		default:
			t.Errorf("unexpected Claude API URL %q", body.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(managementAPICallEnvelope{StatusCode: statusCode, Body: responseBody})
	}))
	t.Cleanup(server.Close)
	t.Setenv("MANAGEMENT_PASSWORD", managementKey)

	path := writeCPAConfigForServer(t, server.URL, "key-1")
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-claude": {provider: "claude", prefix: "prefix1", email: "alice@example.com"},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		PrefixMap:     PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		QuotaCacheTTL: time.Minute,
		CPAConfig:     NewCPAConfigReader(path),
	}}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: bearerHeader("key-1"),
		Query:   url.Values{"provider": {"claude"}, "email": {"alice@example.com"}},
		Path:    "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v), want 200", response, err)
	}
	var decoded providerQuotaResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode quota response: %v", err)
	}
	account, ok := decoded.NominalAccounts["alice@example.com"]
	if !ok || account.Subscription == nil || account.Subscription.Plan != "plan_pro" || len(account.Groups) != 1 || len(account.Groups[0].Buckets) != 2 || account.Groups[0].Buckets[0].Window != "primary" || account.Groups[0].Buckets[0].SourceWindow != "five-hour" || math.Abs(account.Groups[0].Buckets[0].RemainingFraction-0.575) > 1e-9 || account.Groups[0].Buckets[1].Window != "secondary" || account.Groups[0].Buckets[1].SourceWindow != "seven-day" || math.Abs(account.Groups[0].Buckets[1].RemainingFraction-0.82) > 1e-9 {
		t.Fatalf("quota response = %#v, body=%s", decoded, response.Body)
	}
	if len(apiCallRequests) != quotaQueryRetryCount+2 || usageAttempts != quotaQueryRetryCount+1 || apiCallRequests[len(apiCallRequests)-1].URL != claudeProfileURL {
		t.Fatalf("api-call requests = %#v, usage attempts=%d; want usage retried 3 times then profile", apiCallRequests, usageAttempts)
	}
	for _, apiCallRequest := range apiCallRequests[:len(apiCallRequests)-1] {
		if apiCallRequest.URL != claudeUsageURL {
			t.Fatalf("unexpected API call before profile query: %#v", apiCallRequest)
		}
	}
	if strings.Contains(string(response.Body), managementKey) || strings.Contains(string(response.Body), "Bearer $TOKEN$") || host.authListCalls != 1 || host.authGetCalls != 1 {
		t.Fatalf("secret leaked or auth lookup mismatch: body=%s auth-list=%d auth-get=%d", response.Body, host.authListCalls, host.authGetCalls)
	}
	second, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"CLAUDE"}, "email": {"ALICE@example.com"}},
		Path:  "/v0/resource/plugins/user-routing/quota",
	}, "callback-2")
	if err != nil || second.StatusCode != http.StatusOK || len(apiCallRequests) != quotaQueryRetryCount+2 || host.authListCalls != 2 || host.authGetCalls != 2 {
		t.Fatalf("cached Claude query = (%#v, %v), upstream calls=%d auth-list=%d auth-get=%d", second, err, len(apiCallRequests), host.authListCalls, host.authGetCalls)
	}
}

func TestQuotaProviderEmailRequiresBothParametersAndManagementPassword(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-claude": {provider: "claude", prefix: "prefix1", email: "alice@example.com"},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		PrefixMap:     PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		CPAConfig:     NewCPAConfigReader(path),
	}}

	t.Setenv("MANAGEMENT_PASSWORD", "")
	resetQuery, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"claude"}, "email": {"alice@example.com"}}, Path: "/v0/resource/plugins/user-routing/quota/reset",
	}, "callback-1")
	if err != nil || resetQuery.StatusCode != http.StatusBadRequest || host.consumeCalls != 0 {
		t.Fatalf("provider/email reset route = (%#v, %v), consume calls=%d; want 400 and no reset", resetQuery, err, host.consumeCalls)
	}

	partial, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"claude"}}, Path: "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil || partial.StatusCode != http.StatusBadRequest {
		t.Fatalf("partial query = (%#v, %v), want 400", partial, err)
	}

	missingSecret, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"claude"}, "email": {"alice@example.com"}}, Path: "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	if err != nil || missingSecret.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing Management Key = (%#v, %v), want 503", missingSecret, err)
	}
}

func TestDirectQuotaResourceForwardsTokenWithoutCPAKeyOrPrefixLookup(t *testing.T) {
	token := testCodexAccessToken("direct@example.com", "account-direct")
	host := &quotaResourceHost{}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
	}}
	registered := runtime.RegisterManagement()
	if len(registered.Resources) != 4 || registered.Resources[1].Path != quotaDirectResourcePath {
		t.Fatalf("registered resources = %#v, want direct quota route", registered.Resources)
	}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/user-routing/quota/direct",
		Headers: http.Header{
			"Authorization":      []string{"Bearer " + token},
			"ChatGPT-Account-Id": []string{"account-direct"},
		},
	}, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v), want 200", response, err)
	}
	var decoded quotaDirectResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := decoded.Accounts["direct@example.com"]; !ok || decoded.Partial {
		t.Fatalf("direct response = %#v, body=%s", decoded, response.Body)
	}
	if strings.Contains(string(response.Body), "prefix") || strings.Contains(string(response.Body), "actual_accounts") || strings.Contains(string(response.Body), token) {
		t.Fatalf("direct response contains prefix/account routing data or credential: %s", response.Body)
	}
	if host.authListCalls != 0 || host.authGetCalls != 0 || host.usageCalls != 1 || host.resetCreditsCalls != 1 {
		t.Fatalf("host calls = auth-list %d, auth-get %d, usage %d, reset credits %d", host.authListCalls, host.authGetCalls, host.usageCalls, host.resetCreditsCalls)
	}
}

func TestDirectQuotaResetUsesAccessTokenAndAccountClaimWithoutCPAKey(t *testing.T) {
	token := testCodexAccessToken("direct@example.com", "account-direct")
	host := &quotaResourceHost{}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:       true,
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
	}}
	registered := runtime.RegisterManagement()
	if len(registered.Resources) != 4 || registered.Resources[3].Path != quotaDirectResetPath {
		t.Fatalf("registered resources = %#v, want direct reset resource", registered.Resources)
	}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/user-routing/quota/direct/reset",
		Headers: http.Header{
			"Authorization": []string{"Bearer " + token},
		},
	}, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v), want 200", response, err)
	}
	var decoded quotaDirectResetResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !decoded.Success || decoded.Partial || !decoded.Accounts["direct@example.com"].Success {
		t.Fatalf("direct reset response = %#v, body=%s", decoded, response.Body)
	}
	if host.authListCalls != 0 || host.authGetCalls != 0 || host.consumeCalls != 1 {
		t.Fatalf("host calls = auth-list %d, auth-get %d, consume %d; want no auth lookup and one consume", host.authListCalls, host.authGetCalls, host.consumeCalls)
	}
	if host.consumeTokens[0] != token {
		t.Fatal("reset request did not use the supplied Codex access token")
	}
	if strings.Contains(string(response.Body), token) || strings.Contains(string(response.Body), "private-credit-id") {
		t.Fatal("direct reset response contains credential or private reset-credit data")
	}

	postResponse, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/user-routing/quota/direct/reset",
		Headers: http.Header{
			"Authorization": []string{"Bearer " + token},
		},
	}, "callback-1")
	if err != nil || postResponse.StatusCode != http.StatusMethodNotAllowed || host.consumeCalls != 1 {
		t.Fatalf("POST response = (%#v, %v), consume calls=%d; want 405 and no additional consume", postResponse, err, host.consumeCalls)
	}
}

func TestQuotaReadsRetryThreeTimes(t *testing.T) {
	storage, _ := json.Marshal(map[string]string{"access_token": "retry-token", "account_id": "retry-account"})
	host := &quotaResourceHost{usageFailures: 3, resetCreditsFailures: 3}
	client := &quotaHTTPClient{host: host, callbackID: "callback-1"}
	quota, err := fetchCodexQuota(context.Background(), pluginapi.QuotaFetchRequest{StorageJSON: storage, HTTPClient: client})
	if err != nil {
		t.Fatalf("fetchCodexQuota() error = %v", err)
	}
	if host.usageCalls != 4 {
		t.Fatalf("usage attempts = %d, want initial attempt plus 3 retries", host.usageCalls)
	}
	_, err = fetchCodexResetCreditInfo(context.Background(), pluginapi.QuotaFetchRequest{StorageJSON: storage, HTTPClient: client}, quota)
	if err != nil {
		t.Fatalf("fetchCodexResetCreditInfo() error = %v", err)
	}
	if host.resetCreditsCalls != 4 {
		t.Fatalf("reset-credit attempts = %d, want initial attempt plus 3 retries", host.resetCreditsCalls)
	}
}

func TestNativeCodexQuotaFetchCachesOnlyCPAAuthIndex(t *testing.T) {
	storage, _ := json.Marshal(map[string]string{"access_token": "token-1", "account_id": "account-auth-1"})
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-1": {token: "token-1", remaining: 0.5},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true}, QuotaCacheTTL: time.Minute,
	}}
	request := pluginapi.QuotaFetchRequest{AuthIndex: "auth-1", Provider: quotaCodexProvider, StorageJSON: storage}
	for i := 0; i < 2; i++ {
		if _, err := runtime.FetchQuota(context.Background(), request, "callback-1"); err != nil {
			t.Fatal(err)
		}
	}
	if host.usageCalls != 1 {
		t.Fatalf("native quota upstream reads = %d, want one", host.usageCalls)
	}
	request.AuthIndex = ""
	for i := 0; i < 2; i++ {
		if _, err := runtime.FetchQuota(context.Background(), request, "callback-1"); err != nil {
			t.Fatal(err)
		}
	}
	if host.usageCalls != 3 {
		t.Fatalf("direct/no-auth_index upstream reads = %d, want three total", host.usageCalls)
	}
}

func TestQuotaReadStopsAfterThreeRetriesAndResetConsumptionIsNotRetried(t *testing.T) {
	storage, _ := json.Marshal(map[string]string{"access_token": "retry-token", "account_id": "retry-account"})
	host := &quotaResourceHost{usageFailures: quotaQueryRetryCount + 1}
	_, err := fetchCodexQuota(context.Background(), pluginapi.QuotaFetchRequest{StorageJSON: storage, HTTPClient: &quotaHTTPClient{host: host}})
	if err == nil || host.usageCalls != quotaQueryRetryCount+1 {
		t.Fatalf("fetchCodexQuota() error=%v attempts=%d, want failure after 4 attempts", err, host.usageCalls)
	}

	host = &quotaResourceHost{consumeFailures: 1}
	runtime := &Runtime{host: host, config: runtimeConfig{Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true}}}
	reset := runtime.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{Provider: quotaCodexProvider, StorageJSON: storage}, "callback-1")
	if reset.Success || host.consumeCalls != 1 {
		t.Fatalf("ResetQuota() = %#v, consume attempts=%d, want one non-retried consume", reset, host.consumeCalls)
	}
}

func testCodexAccessToken(email, accountID string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": email},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": accountID},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestQuotaResetResourceSynchronouslyResetsOnlyNominalAccounts(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{
		auths: map[string]quotaTestAuth{
			"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1"},
			"auth-2": {prefix: "prefix1", email: "two@example.com", token: "token-2"},
			"auth-3": {prefix: "prefix2", email: "fallback@example.com", token: "token-3"},
		},
	}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled:             true,
		PrefixMap:           PrefixMap{"key-1": "prefix1/", "default": ""},
		QuotaFallback:       quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"prefix1": {"prefix2"}}},
		QuotaProvider:       quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		StrictKeyValidation: true,
		CPAConfig:           NewCPAConfigReader(path),
	}}

	registered := runtime.RegisterManagement()
	if len(registered.Resources) != 4 || registered.Resources[2].Path != quotaResetResourcePath || registered.Resources[3].Path != quotaDirectResetPath {
		t.Fatalf("registered resources = %#v, want quota, direct quota, reset, and direct reset routes", registered.Resources)
	}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Headers: bearerHeader("key-1"),
		Path:    "/v0/resource/plugins/user-routing/quota/reset",
	}, "callback-1")
	if err != nil {
		t.Fatalf("HandleManagement() error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	var decoded quotaResetResourceResponse
	if err := json.Unmarshal(response.Body, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.NominalPrefix != "prefix1/" || !decoded.Success || decoded.Partial || len(decoded.NominalAccounts) != 2 {
		t.Fatalf("reset response = %#v, body = %s", decoded, response.Body)
	}
	if !decoded.NominalAccounts["one@example.com"].Success || !decoded.NominalAccounts["two@example.com"].Success {
		t.Fatalf("nominal account reset results = %#v", decoded.NominalAccounts)
	}
	if host.consumeCalls != 2 {
		t.Fatalf("consume calls = %d, want exactly one call per nominal account", host.consumeCalls)
	}
	consumed := make(map[string]int)
	for _, token := range host.consumeTokens {
		consumed[token]++
	}
	if consumed["token-1"] != 1 || consumed["token-2"] != 1 || consumed["token-3"] != 0 {
		t.Fatalf("consumed account tokens = %#v, want only the two nominal accounts", consumed)
	}
	if strings.Contains(string(response.Body), "token-") || strings.Contains(string(response.Body), "key-1") || strings.Contains(string(response.Body), "private-credit-id") {
		t.Fatalf("response contains credential material: %s", response.Body)
	}

	postResponse, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Headers: bearerHeader("key-1"),
		Path:    "/v0/resource/plugins/user-routing/quota/reset",
	}, "callback-1")
	if err != nil {
		t.Fatalf("POST HandleManagement() error = %v", err)
	}
	if postResponse.StatusCode != http.StatusMethodNotAllowed || host.consumeCalls != 2 {
		t.Fatalf("POST response status = %d, consume calls = %d; want 405 and no additional attempt", postResponse.StatusCode, host.consumeCalls)
	}
}

func TestResetQuotaConsumesAvailableCodexCredit(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{
		auths: map[string]quotaTestAuth{
			"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", remaining: 0},
		},
	}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true}, CPAConfig: NewCPAConfigReader(path),
	}}
	storage, _ := json.Marshal(map[string]any{"access_token": "token-1", "account_id": "account-auth-1"})
	response := runtime.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{
		Provider: quotaCodexProvider, StorageJSON: storage,
	}, "callback-1")
	if !response.Success {
		t.Fatalf("ResetQuota() = %#v, want success", response)
	}
	if host.consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want exactly one", host.consumeCalls)
	}
	var payload map[string]string
	if err := json.Unmarshal(host.consumeBody, &payload); err != nil {
		t.Fatalf("decode consume request: %v", err)
	}
	if payload["redeem_request_id"] == "" || payload["credit_id"] != "private-credit-id" || len(payload) != 2 {
		t.Fatalf("consume request payload = %#v, want a request ID and the selected Codex credit ID", payload)
	}
}

func TestResetQuotaDoesNotConsumeWithoutCredits(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{noCredits: true}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true}, CPAConfig: NewCPAConfigReader(path),
	}}
	storage, _ := json.Marshal(map[string]any{"access_token": "token-1"})
	response := runtime.ResetQuota(context.Background(), pluginapi.QuotaResetRequest{
		Provider: quotaCodexProvider, StorageJSON: storage,
	}, "callback-1")
	if response.Success || host.consumeCalls != 0 {
		t.Fatalf("ResetQuota() = %#v, consume calls = %d; want no consume", response, host.consumeCalls)
	}
}

type quotaTestAuth struct {
	provider           string
	prefix             string
	email              string
	projectID          string
	token              string
	remaining          float64
	hasSecondary       bool
	secondaryRemaining float64
}

type quotaResourceHost struct {
	auths                map[string]quotaTestAuth
	authListCalls        int
	authGetCalls         int
	usageCalls           int
	usageFailures        int
	resetCreditsCalls    int
	resetCreditsFailures int
	consumeCalls         int
	consumeFailures      int
	consumeBody          []byte
	consumeTokens        []string
	noCredits            bool
}

func (h *quotaResourceHost) Call(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		h.authListCalls++
		files := make([]pluginapi.HostAuthFileEntry, 0, len(h.auths))
		for index, auth := range h.auths {
			provider := auth.provider
			if provider == "" {
				provider = quotaCodexProvider
			}
			files = append(files, pluginapi.HostAuthFileEntry{AuthIndex: index, ID: index, Provider: provider, Email: auth.email, ProjectID: auth.projectID})
		}
		return json.Marshal(hostAuthListResponse{Files: files})
	case pluginabi.MethodHostAuthGet:
		h.authGetCalls++
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var request pluginapi.HostAuthGetRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		auth, ok := h.auths[request.AuthIndex]
		if !ok {
			return nil, context.Canceled
		}
		storage, _ := json.Marshal(map[string]any{
			"prefix":       auth.prefix,
			"email":        auth.email,
			"project_id":   auth.projectID,
			"access_token": auth.token,
			"account_id":   "account-" + request.AuthIndex,
		})
		return json.Marshal(hostAuthGetResponse{AuthIndex: request.AuthIndex, JSON: storage})
	case pluginabi.MethodHostHTTPDo:
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var request struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
			Body    []byte      `json:"body"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		if strings.Contains(request.URL, "/rate-limit-reset-credits/consume") {
			h.consumeCalls++
			h.consumeBody = append([]byte(nil), request.Body...)
			h.consumeTokens = append(h.consumeTokens, strings.TrimPrefix(request.Headers.Get("Authorization"), "Bearer "))
			if h.consumeFailures > 0 {
				h.consumeFailures--
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable})
			}
			body, _ := json.Marshal(map[string]any{"code": "reset", "windows_reset": 2})
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
		}
		if strings.HasSuffix(request.URL, "/rate-limit-reset-credits") {
			h.resetCreditsCalls++
			if h.resetCreditsFailures > 0 {
				h.resetCreditsFailures--
				return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable})
			}
			count := 1
			credits := []map[string]any{{"id": "private-credit-id", "reset_type": "codex_rate_limits", "status": "available", "expires_at": "2030-07-17T12:30:00Z"}}
			if h.noCredits {
				count = 0
				credits = []map[string]any{}
			}
			body, _ := json.Marshal(map[string]any{"available_count": count, "credits": credits})
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
		}
		h.usageCalls++
		if h.usageFailures > 0 {
			h.usageFailures--
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusServiceUnavailable})
		}
		token := strings.TrimPrefix(request.Headers.Get("Authorization"), "Bearer ")
		remaining := 0.0
		for _, auth := range h.auths {
			if auth.token == token {
				remaining = auth.remaining
				break
			}
		}
		used := int((1 - remaining) * 100)
		rateLimit := map[string]any{
			"primary_window": map[string]any{"used_percent": used, "reset_at": 1900000000},
		}
		for _, auth := range h.auths {
			if auth.token == token && auth.hasSecondary {
				secondaryUsed := int((1 - auth.secondaryRemaining) * 100)
				rateLimit["secondary_window"] = map[string]any{"used_percent": secondaryUsed, "reset_at": 1900003600}
				break
			}
		}
		body, _ := json.Marshal(map[string]any{"plan_type": "pro", "rate_limit": rateLimit, "rate_limit_reset_credits": map[string]any{"available_count": 1}})
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
	default:
		return json.RawMessage(`{}`), nil
	}
}

func writeCPAConfigForServer(t *testing.T, serverURL string, keys ...string) string {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse CPA test server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split CPA test server address: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := fmt.Sprintf("host: %s\nport: %s\napi-keys:\n", host, port)
	for _, key := range keys {
		content += fmt.Sprintf("  - %q\n", key)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write CPA test config: %v", err)
	}
	return path
}
