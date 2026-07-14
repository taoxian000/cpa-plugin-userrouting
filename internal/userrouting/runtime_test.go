package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestResolveUsesSpecificPrefixWhenModelExists(t *testing.T) {
	runtime, catalogRequests := testRuntime(t, []string{"team-a/gpt-5", "fallback/gpt-5"})
	decision, err := runtime.Resolve(context.Background(), bearerHeader("key-1"), nil, "gpt-5")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decision.FinalModel != "team-a/gpt-5" || decision.UsedDefault {
		t.Fatalf("decision = %#v", decision)
	}
	if *catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", *catalogRequests)
	}
}

func TestResolveFallsBackToDefaultPrefix(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"fallback/gpt-5"})
	decision, err := runtime.Resolve(context.Background(), bearerHeader("key-1"), nil, "gpt-5")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decision.FinalModel != "fallback/gpt-5" || !decision.UsedDefault {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestResolvePreservesThinkingSuffixDuringCatalogLookup(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5"})
	decision, err := runtime.Resolve(context.Background(), bearerHeader("key-1"), nil, "gpt-5(high)")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if decision.FinalModel != "team-a/gpt-5(high)" {
		t.Fatalf("FinalModel = %q", decision.FinalModel)
	}
}

func TestRouteDeclinesCountTokensAndUnchangedDefault(t *testing.T) {
	path := writeCPAConfig(t, "key-1")
	host := &fakeHost{}
	runtime, err := NewRuntime(host, []byte("cpa_config_path: "+quotedYAML(path)+"\nprefix_map:\n  default: ''\n"), ConfigureOptions{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	base := pluginapi.ModelRouteRequest{
		SourceFormat:   "claude",
		RequestedModel: "claude-sonnet-4-5",
		Headers:        bearerHeader("key-1"),
	}
	response, err := runtime.Route(base)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if response.Handled {
		t.Fatal("Route() handled unchanged default request")
	}
	base.Metadata = map[string]any{"request_path": "/v1/messages/count_tokens"}
	response, err = runtime.Route(base)
	if err != nil {
		t.Fatalf("Route(count) error = %v", err)
	}
	if response.Handled {
		t.Fatal("Route() handled count_tokens request")
	}
}

func TestExecuteForwardsRewrittenModelAndLogsIt(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5"})
	response, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:           "gpt-5",
		Format:          "openai",
		SourceFormat:    "openai",
		Headers:         bearerHeader("key-1"),
		Query:           url.Values{},
		OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
	}, "callback-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(response.Payload) != `{"ok":true}` {
		t.Fatalf("response payload = %s", response.Payload)
	}

	host := runtime.host.(*fakeHost)
	modelCall := host.call(pluginabi.MethodHostModelExecute)
	var modelReq struct {
		Model string `json:"model"`
		Body  []byte `json:"body"`
	}
	if err := json.Unmarshal(modelCall, &modelReq); err != nil {
		t.Fatalf("decode model call: %v", err)
	}
	if modelReq.Model != "team-a/gpt-5" {
		t.Fatalf("host model = %q", modelReq.Model)
	}
	if got := gjson.GetBytes(modelReq.Body, "model").String(); got != "team-a/gpt-5" {
		t.Fatalf("rewritten body model = %q", got)
	}
	logCall := host.call(pluginabi.MethodHostLog)
	if got := gjson.GetBytes(logCall, "fields.model").String(); got != "team-a/gpt-5" {
		t.Fatalf("logged model = %q", got)
	}
	if got := gjson.GetBytes(logCall, "host_callback_id").String(); got != "callback-1" {
		t.Fatalf("log callback id = %q", got)
	}
}

func TestExecuteFallsBackAcrossPrefixesAfterCodexQuotaExhausted(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5", "team-b/gpt-5"})
	host := &quotaFallbackHost{quotaError: `host_call_failed: {"error":{"type":"usage_limit_reached"}}`}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{
		Enabled: true,
		Prefixes: map[string][]string{
			"team-a": {"team-b"},
		},
	}

	response, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:           "gpt-5",
		Format:          "openai",
		SourceFormat:    "openai",
		Headers:         bearerHeader("key-1"),
		OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
	}, "callback-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(response.Payload) != `{"ok":true}` {
		t.Fatalf("response payload = %s", response.Payload)
	}
	if got, want := host.models, []string{"team-a/gpt-5", "team-b/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
}

func TestExecuteDoesNotFallbackForNonQuotaError(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5", "team-b/gpt-5"})
	host := &quotaFallbackHost{quotaError: "host_call_failed: upstream connection reset"}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{
		Enabled: true,
		Prefixes: map[string][]string{
			"team-a": {"team-b"},
		},
	}

	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:        "gpt-5",
		Format:       "openai",
		SourceFormat: "openai",
		Headers:      bearerHeader("key-1"),
	}, "callback-1")
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream error")
	}
	if got, want := host.models, []string{"team-a/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
}

func TestExecuteFallsBackForNonQuotaErrorWhenEnabled(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5", "team-b/gpt-5"})
	host := &quotaFallbackHost{quotaError: "host_call_failed: upstream connection reset"}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{
		Enabled:               true,
		FallbackOnOtherErrors: true,
		Prefixes: map[string][]string{
			"team-a": {"team-b"},
		},
	}

	response, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:        "gpt-5",
		Format:       "openai",
		SourceFormat: "openai",
		Headers:      bearerHeader("key-1"),
	}, "callback-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(response.Payload) != `{"ok":true}` {
		t.Fatalf("response payload = %s", response.Payload)
	}
	if got, want := host.models, []string{"team-a/gpt-5", "team-b/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
}

func TestRunStreamFallsBackBeforeFirstPayloadAfterCodexQuotaExhausted(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5", "team-b/gpt-5"})
	host := &streamQuotaFallbackHost{}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{
		Enabled: true,
		Prefixes: map[string][]string{
			"team-a": {"team-b"},
		},
	}

	err := runtime.RunStream(context.Background(), pluginapi.ExecutorRequest{
		Model:        "gpt-5",
		Format:       "openai",
		SourceFormat: "openai",
		Headers:      bearerHeader("key-1"),
	}, "callback-1", "plugin-stream")
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if got, want := host.models, []string{"team-a/gpt-5", "team-b/gpt-5"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
	if got := string(host.emitted); got != "data: done\n\n" {
		t.Fatalf("emitted payload = %q", got)
	}
}

func TestQuotaFallbackTargetsMatchLongestConfiguredPrefix(t *testing.T) {
	runtime := &Runtime{config: runtimeConfig{QuotaFallback: quotaFallbackConfig{
		Enabled: true,
		Prefixes: map[string][]string{
			"team":       {"other"},
			"team/codex": {"backup"},
		},
	}}}
	if got, want := runtime.quotaFallbackTargets("team/codex/gpt-5(high)"), []string{"team/codex/gpt-5(high)", "backup/gpt-5(high)"}; !equalStrings(got, want) {
		t.Fatalf("quotaFallbackTargets() = %#v, want %#v", got, want)
	}
}

func testRuntime(t *testing.T, models []string) (*Runtime, *int) {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if got := req.Header.Get("Authorization"); got != "Bearer key-1" {
			t.Errorf("Authorization = %q", got)
		}
		data := make([]map[string]string, 0, len(models))
		for _, model := range models {
			data = append(data, map[string]string{"id": model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	path := writeCPAConfig(t, "key-1", "key-2")
	raw := "cpa_config_path: " + quotedYAML(path) + "\n" +
		"models_url: " + quotedYAML(server.URL+"/v1/models") + "\n" +
		"model_cache_ttl: 1m\n" +
		"prefix_map:\n" +
		"  key-1: team-a/\n" +
		"  default: fallback/\n"
	host := &fakeHost{}
	runtime, err := NewRuntime(host, []byte(raw), ConfigureOptions{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return runtime, &requests
}

func bearerHeader(key string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + key}}
}

type fakeHost struct {
	mu    sync.Mutex
	calls map[string][][]byte
}

type quotaFallbackHost struct {
	mu         sync.Mutex
	models     []string
	quotaError string
}

func (h *quotaFallbackHost) Call(method string, payload any) (json.RawMessage, error) {
	if method != pluginabi.MethodHostModelExecute {
		return json.RawMessage(`{}`), nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.models = append(h.models, request.Model)
	attempt := len(h.models)
	h.mu.Unlock()
	if attempt == 1 {
		return nil, errors.New(h.quotaError)
	}
	return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)})
}

type streamQuotaFallbackHost struct {
	mu      sync.Mutex
	models  []string
	emitted []byte
}

func (h *streamQuotaFallbackHost) Call(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostModelExecuteStream:
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.models = append(h.models, request.Model)
		attempt := len(h.models)
		h.mu.Unlock()
		if attempt == 1 {
			return nil, errors.New(`host_call_failed: {"error":{"type":"usage_limit_reached"}}`)
		}
		return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "host-stream"})
	case pluginabi.MethodHostModelStreamRead:
		return json.Marshal(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: done\n\n"), Done: true})
	case pluginabi.MethodHostStreamEmit:
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var request struct {
			Payload []byte `json:"payload"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.emitted = append(h.emitted, request.Payload...)
		h.mu.Unlock()
		return json.RawMessage(`{}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (h *fakeHost) Call(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.calls == nil {
		h.calls = make(map[string][][]byte)
	}
	h.calls[method] = append(h.calls[method], raw)
	h.mu.Unlock()
	switch method {
	case pluginabi.MethodHostModelExecute:
		return json.Marshal(pluginapi.HostModelExecutionResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(`{"ok":true}`),
		})
	default:
		return json.RawMessage(`{}`), nil
	}
}

func (h *fakeHost) call(method string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	values := h.calls[method]
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}
