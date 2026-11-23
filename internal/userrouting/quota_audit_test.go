package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestCallManagementAPICallDoesNotRetryUpstreamClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(managementAPICallEnvelope{StatusCode: http.StatusForbidden, Body: `{"error":"forbidden"}`})
	}))
	t.Cleanup(server.Close)
	snapshot, err := NewCPAConfigReader(writeCPAConfigForServer(t, server.URL, "key-1")).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	result, err := callManagementAPICall(context.Background(), snapshot, "management-key", managementAPICallRequest{Method: http.MethodGet, URL: "https://example.invalid"})
	if err != nil || result.StatusCode != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("callManagementAPICall() = (%#v, %v), calls=%d; want upstream 403 returned once", result, err, calls.Load())
	}
}

func TestCallManagementAPICallStopsOnEndpointClientError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	snapshot, err := NewCPAConfigReader(writeCPAConfigForServer(t, server.URL, "key-1")).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, err = callManagementAPICall(context.Background(), snapshot, "wrong-key", managementAPICallRequest{Method: http.MethodGet, URL: "https://example.invalid"})
	var permanent *permanentQuotaError
	if !errors.As(err, &permanent) || calls.Load() != 1 {
		t.Fatalf("error = %v, calls = %d; want one permanent failure", err, calls.Load())
	}
}

type statusQuotaHost struct {
	status int
	calls  int
}

func (h *statusQuotaHost) Call(method string, payload any) (json.RawMessage, error) {
	if method != pluginabi.MethodHostHTTPDo {
		return json.RawMessage(`{}`), nil
	}
	h.calls++
	return json.Marshal(pluginapi.HTTPResponse{StatusCode: h.status})
}

func TestCodexQuotaReadRetriesOnlyTransientStatuses(t *testing.T) {
	storage, _ := json.Marshal(map[string]string{"access_token": "token", "account_id": "account"})
	for _, tc := range []struct {
		status int
		want   int
	}{
		{http.StatusUnauthorized, 1},
		{http.StatusForbidden, 1},
		{http.StatusServiceUnavailable, quotaQueryRetryCount + 1},
	} {
		host := &statusQuotaHost{status: tc.status}
		_, err := fetchCodexQuota(context.Background(), pluginapi.QuotaFetchRequest{StorageJSON: storage, HTTPClient: &quotaHTTPClient{host: host}})
		if err == nil || host.calls != tc.want {
			t.Errorf("HTTP %d: err=%v calls=%d, want %d calls", tc.status, err, host.calls, tc.want)
		}
	}
}

func TestQuotaResetIncludesCoolingAccounts(t *testing.T) {
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-1": {prefix: "prefix1", email: "one@example.com", token: "token-1", unavailable: true, retryAt: time.Now().Add(time.Hour)},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, PrefixMap: PrefixMap{"key-1": "prefix1/"},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		CPAConfig:     NewCPAConfigReader(writeCPAConfig(t, "key-1")),
	}}
	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"), Path: "/v0/resource/plugins/user-routing/quota/reset",
	}, "callback-1")
	var decoded quotaResetResourceResponse
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &decoded) != nil || !decoded.Success || host.consumeCalls != 1 {
		t.Fatalf("reset = (%d, %v) body=%s consume=%d; want cooling account reset", response.StatusCode, err, response.Body, host.consumeCalls)
	}
}

func TestQuotaResourcesKeepAccountsSharingAnEmail(t *testing.T) {
	host := &quotaResourceHost{auths: map[string]quotaTestAuth{
		"auth-1": {prefix: "prefix1", email: "same@example.com", token: "token-1", remaining: 0.5},
		"auth-2": {prefix: "prefix1", email: "same@example.com", token: "token-2", remaining: 0.4},
		"auth-3": {prefix: "prefix1", email: "same@example.com", token: "token-3", remaining: 0.3},
	}}
	runtime := &Runtime{host: host, config: runtimeConfig{
		Enabled: true, PrefixMap: PrefixMap{"key-1": "prefix1/"},
		QuotaProvider: quotaProviderConfig{Enabled: true, PublicEndpoint: true},
		CPAConfig:     NewCPAConfigReader(writeCPAConfig(t, "key-1")),
	}}
	wantKeys := []string{"same@example.com", "same@example.com#2", "same@example.com#3"}

	response, err := runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"), Path: "/v0/resource/plugins/user-routing/quota",
	}, "callback-1")
	var quota quotaResourceResponse
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &quota) != nil {
		t.Fatalf("quota = (%d, %v) body=%s", response.StatusCode, err, response.Body)
	}
	for _, key := range wantKeys {
		if _, ok := quota.NominalAccounts[key]; !ok {
			t.Fatalf("quota accounts = %v, missing %q", quota.NominalAccounts, key)
		}
	}
	if !quota.Partial || !strings.Contains(strings.Join(quota.Errors, ";"), "share an email") {
		t.Fatalf("quota response should flag duplicate emails: %s", response.Body)
	}

	response, err = runtime.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Headers: bearerHeader("key-1"), Path: "/v0/resource/plugins/user-routing/quota/reset",
	}, "callback-1")
	var reset quotaResetResourceResponse
	if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &reset) != nil {
		t.Fatalf("reset = (%d, %v) body=%s", response.StatusCode, err, response.Body)
	}
	for _, key := range wantKeys {
		if _, ok := reset.NominalAccounts[key]; !ok {
			t.Fatalf("reset accounts = %v, missing %q", reset.NominalAccounts, key)
		}
	}
	if len(reset.Errors) != 1 || host.consumeCalls != 3 {
		t.Fatalf("reset errors = %v, consume = %d; want one duplicate note and three resets", reset.Errors, host.consumeCalls)
	}
}

func TestNormalizeCodexExpirationAcceptsNumericStrings(t *testing.T) {
	want := time.Unix(1900000000, 0).UTC().Format(time.RFC3339)
	for _, raw := range []string{`1900000000`, `"1900000000"`, `"1900000000000"`, `1900000000000`, `"2030-03-17T17:46:40Z"`} {
		if got, ok := normalizeCodexExpiration(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("normalizeCodexExpiration(%s) = (%q, %v), want %q", raw, got, ok, want)
		}
	}
	if _, ok := normalizeCodexExpiration(json.RawMessage(`"0"`)); ok {
		t.Errorf("zero expiry should be rejected")
	}
}
