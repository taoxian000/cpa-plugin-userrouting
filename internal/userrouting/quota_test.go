package userrouting

import (
	"context"
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
      "credits":{"balance":3}
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
	if len(response.Summary) != 1 || response.Summary[0].Value != 3 {
		t.Fatalf("summary = %#v, want credits balance", response.Summary)
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
}

func TestQuotaResourceReturnsNominalAndActualPrefixes(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &quotaResourceHost{
		auths: map[string]quotaTestAuth{
			"auth-1": {prefix: "prefix1", token: "token-1", remaining: 0},
			"auth-2": {prefix: "prefix2", token: "token-2", remaining: 0.5},
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
	if len(decoded.Prefixes) != 2 {
		t.Fatalf("prefix results = %#v, want nominal plus fallback", decoded.Prefixes)
	}
	if decoded.Prefixes[0].NominalPrefix != "prefix1/" || decoded.Prefixes[0].ActualPrefix != "prefix1/" {
		t.Fatalf("nominal result prefixes = (%q, %q)", decoded.Prefixes[0].NominalPrefix, decoded.Prefixes[0].ActualPrefix)
	}
	if decoded.Prefixes[1].NominalPrefix != "prefix1/" || decoded.Prefixes[1].ActualPrefix != "prefix2/" {
		t.Fatalf("fallback result prefixes = (%q, %q)", decoded.Prefixes[1].NominalPrefix, decoded.Prefixes[1].ActualPrefix)
	}
	if len(decoded.Prefixes[0].Accounts) != 1 || len(decoded.Prefixes[1].Accounts) != 1 {
		t.Fatalf("accounts = %#v, want one account per prefix", decoded.Prefixes)
	}
	if strings.Contains(string(response.Body), "key-1") || strings.Contains(string(response.Body), "token-") {
		t.Fatalf("response contains credential material: %s", response.Body)
	}
}

type quotaTestAuth struct {
	prefix    string
	token     string
	remaining float64
}

type quotaResourceHost struct {
	auths map[string]quotaTestAuth
}

func (h *quotaResourceHost) Call(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		files := make([]pluginapi.HostAuthFileEntry, 0, len(h.auths))
		for index := range h.auths {
			files = append(files, pluginapi.HostAuthFileEntry{AuthIndex: index, ID: index, Provider: quotaCodexProvider})
		}
		return json.Marshal(hostAuthListResponse{Files: files})
	case pluginabi.MethodHostAuthGet:
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
			Headers http.Header `json:"headers"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
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
		}})
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: body})
	default:
		return json.RawMessage(`{}`), nil
	}
}
