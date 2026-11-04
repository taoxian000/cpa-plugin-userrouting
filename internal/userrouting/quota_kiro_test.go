package userrouting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestQuotaResourceQueriesAllKiroAccountsAndCachesByAuthIndex(t *testing.T) {
	const managementKey = "test-management-password"
	requests := make(map[string]int)
	failB := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v0/management/quota/fetch" || req.Header.Get("Authorization") != "Bearer "+managementKey {
			t.Errorf("unexpected management request: %s %s", req.Method, req.URL.Path)
		}
		var body struct {
			AuthIndex string `json:"auth_index"`
			Provider  string `json:"provider"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode quota fetch request: %v", err)
		}
		if body.Provider != "kiro" {
			t.Errorf("quota provider = %q", body.Provider)
		}
		requests[body.AuthIndex]++
		if body.AuthIndex == "auth-b" && failB {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"temporary failure"}`))
			return
		}
		_, _ = w.Write([]byte(`{"subscription":{"plan":"KIRO PRO","tierId":"Q_DEVELOPER_STANDALONE_PRO"},"summary":[{"key":"limit_0","label":"Credits limit","value":1000,"format":"number"}],"groups":[{"displayName":"Credits","buckets":[{"window":"Plan","remainingFraction":0.75,"resetTime":"2030-10-01T00:00:00Z","description":"250 / 1000"},{"window":"Free trial","remainingFraction":0.9}]}]}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("MANAGEMENT_PASSWORD", managementKey)
	path := writeCPAConfigForServer(t, server.URL, "key-1")
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-a": {provider: "kiro", email: "shared@example.test", token: "token-a"},
		"auth-b": {provider: "kiro", email: "shared@example.test", token: "token-b"},
		"auth-c": {provider: "codex", email: "other@example.test", token: "token-c"},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, PrefixMap: PrefixMap{"key-1": "prefix1/"},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		QuotaCacheTTL: time.Minute, CPAConfig: NewCPAConfigReader(path),
	}}
	request := pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"KIRO"}}, Path: "/v0/resource/plugins/user-routing/quota",
	}
	response, err := runtime.HandleManagement(context.Background(), request, "callback-1")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HandleManagement() = (%#v, %v)", response, err)
	}
	var result providerQuotaResourceResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Partial || len(result.Errors) != 1 || len(result.NominalAccounts) != 1 {
		t.Fatalf("partial Kiro result = %#v", result)
	}
	account, ok := result.NominalAccounts["shared@example.test"]
	if !ok || account.Subscription == nil || account.Subscription.Plan != "KIRO PRO" || len(account.Summary) != 1 || len(account.Groups) != 1 || len(account.Groups[0].Buckets) != 1 {
		t.Fatalf("Kiro account = %#v", account)
	}
	bucket := account.Groups[0].Buckets[0]
	if bucket.Window != "monthly" || bucket.RemainingFraction != 0.75 || bucket.ResetTime != "2030-10-01T00:00:00Z" || bucket.Description != "250 / 1000" {
		t.Fatalf("monthly bucket = %#v", bucket)
	}
	if requests["auth-a"] != 1 || requests["auth-b"] != quotaQueryRetryCount+1 || requests["auth-c"] != 0 || strings.Contains(string(response.Body), managementKey) || strings.Contains(string(response.Body), "token-a") {
		t.Fatalf("unexpected requests or secret disclosure: requests=%#v, body=%s", requests, response.Body)
	}
	second, err := runtime.HandleManagement(context.Background(), request, "callback-2")
	if err != nil || second.StatusCode != http.StatusOK || requests["auth-a"] != 1 || requests["auth-b"] != 2*(quotaQueryRetryCount+1) {
		t.Fatalf("cache behavior: response=%#v, err=%v, requests=%#v", second, err, requests)
	}
	failB = false
	third, err := runtime.HandleManagement(context.Background(), request, "callback-3")
	if err != nil || third.StatusCode != http.StatusOK || requests["auth-a"] != 1 || requests["auth-b"] != 2*(quotaQueryRetryCount+1)+1 {
		t.Fatalf("recovered Kiro account: response=%#v, err=%v, requests=%#v", third, err, requests)
	}
	result = providerQuotaResourceResponse{}
	if err := json.Unmarshal(third.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.NominalAccounts) != 2 || len(result.NominalAccounts["shared@example.test (auth-b)"].Groups) != 1 {
		t.Fatalf("all Kiro accounts = %#v", result)
	}
}

func TestKiroMonthlyQuotaAccountRequiresPlanBucket(t *testing.T) {
	if _, ok := kiroMonthlyQuotaAccount(pluginapi.QuotaFetchResponse{}); ok {
		t.Fatal("empty quota has no monthly meter")
	}
	if _, ok := kiroMonthlyQuotaAccount(pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{DisplayName: "Credits", Buckets: []pluginapi.QuotaBucket{{Window: "Free trial", RemainingFraction: 1}}}}}); ok {
		t.Fatal("trial-only quota must not masquerade as monthly plan")
	}
}

func TestAllKiroQuotaRequiresManagementSecretAndNativeProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"no quota provider available for credential"}`))
	}))
	t.Cleanup(server.Close)
	path := writeCPAConfigForServer(t, server.URL, "key-1")
	runtime := &Runtime{host: &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-kiro": {provider: "kiro", email: "kiro@example.test"},
	}}, config: runtimeConfig{
		Enabled: true, QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		CPAConfig: NewCPAConfigReader(path),
	}}
	request := pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"),
		Query: url.Values{"provider": {"kiro"}}, Path: "/v0/resource/plugins/user-routing/quota",
	}
	t.Setenv("MANAGEMENT_PASSWORD", "")
	missingSecret, err := runtime.HandleManagement(context.Background(), request, "")
	if err != nil || missingSecret.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing Management Key = (%#v, %v)", missingSecret, err)
	}
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-password")
	noProvider, err := runtime.HandleManagement(context.Background(), request, "")
	if err != nil || noProvider.StatusCode != http.StatusBadGateway {
		t.Fatalf("missing Kiro QuotaProvider = (%#v, %v)", noProvider, err)
	}
}
