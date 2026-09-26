package userrouting

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
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
			"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", remaining: 0},
			"auth-2": {prefix: "prefix2", email: "two@example.com", token: "token-2", remaining: 0.5},
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
	prefix    string
	email     string
	token     string
	remaining float64
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
			files = append(files, pluginapi.HostAuthFileEntry{AuthIndex: index, ID: index, Provider: quotaCodexProvider, Email: auth.email})
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
		body, _ := json.Marshal(map[string]any{"plan_type": "pro", "rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": used, "reset_at": 1900000000},
		}, "rate_limit_reset_credits": map[string]any{"available_count": 1}})
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
	default:
		return json.RawMessage(`{}`), nil
	}
}
