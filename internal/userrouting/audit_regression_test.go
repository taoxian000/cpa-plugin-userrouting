package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const testModelNotFoundError = `plugin_error: {"error":{"message":"unknown provider for model team-b/gpt-5","type":"invalid_request_error","code":"model_not_found","param":"model"}}`
const testQuotaError = `host_call_failed: {"error":{"type":"usage_limit_reached"}}`

// scriptedModelHost fails host model calls for the models listed in failures
// and succeeds for every other model.
type scriptedModelHost struct {
	mu       sync.Mutex
	failures map[string]string
	models   []string
}

func (h *scriptedModelHost) Call(method string, payload any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostModelExecute, pluginabi.MethodHostModelExecuteStream:
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
		h.mu.Unlock()
		if message, failed := h.failures[request.Model]; failed {
			return nil, errors.New(message)
		}
		if method == pluginabi.MethodHostModelExecuteStream {
			return json.Marshal(pluginapi.HostModelStreamResponse{StatusCode: http.StatusOK, StreamID: "host-stream"})
		}
		return json.Marshal(pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)})
	case pluginabi.MethodHostModelStreamRead:
		return json.Marshal(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: done\n\n"), Done: true})
	default:
		return json.RawMessage(`{}`), nil
	}
}

func fallbackChainRuntime(t *testing.T, failures map[string]string, targets ...string) (*Runtime, *scriptedModelHost) {
	t.Helper()
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5"})
	host := &scriptedModelHost{failures: failures}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"team-a": targets}}
	return runtime, host
}

func TestExecuteSkipsFallbackPrefixWithoutModel(t *testing.T) {
	runtime, host := fallbackChainRuntime(t, map[string]string{
		"team-a/gpt-5": testQuotaError,
		"team-b/gpt-5": testModelNotFoundError,
	}, "team-b", "team-c")
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
		OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := []string{"team-a/gpt-5", "team-b/gpt-5", "team-c/gpt-5"}; !equalStrings(host.models, want) {
		t.Fatalf("host models = %#v, want %#v", host.models, want)
	}
}

func TestExecuteReportsOriginalErrorWhenLastFallbackLacksModel(t *testing.T) {
	runtime, _ := fallbackChainRuntime(t, map[string]string{
		"team-a/gpt-5": testQuotaError,
		"team-b/gpt-5": testModelNotFoundError,
	}, "team-b")
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
		OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
	}, "")
	if err == nil || !strings.Contains(err.Error(), "usage_limit_reached") {
		t.Fatalf("Execute() error = %v, want original quota error", err)
	}
}

func TestRunStreamSkipsFallbackPrefixWithoutModel(t *testing.T) {
	runtime, host := fallbackChainRuntime(t, map[string]string{
		"team-a/gpt-5": testQuotaError,
		"team-b/gpt-5": testModelNotFoundError,
	}, "team-b", "team-c")
	err := runtime.RunStream(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
	}, "", "plugin-stream")
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if want := []string{"team-a/gpt-5", "team-b/gpt-5", "team-c/gpt-5"}; !equalStrings(host.models, want) {
		t.Fatalf("host models = %#v, want %#v", host.models, want)
	}

	runtime, _ = fallbackChainRuntime(t, map[string]string{
		"team-a/gpt-5": testQuotaError,
		"team-b/gpt-5": testModelNotFoundError,
	}, "team-b")
	err = runtime.RunStream(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
	}, "", "plugin-stream")
	if err == nil || !strings.Contains(err.Error(), "usage_limit_reached") {
		t.Fatalf("RunStream() error = %v, want original quota error", err)
	}
}

func TestPrimaryModelNotFoundIsNotRetried(t *testing.T) {
	runtime, host := fallbackChainRuntime(t, map[string]string{"team-a/gpt-5": testModelNotFoundError}, "team-b")
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai", Headers: bearerHeader("key-1"),
		OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
	}, "")
	if err == nil || !isModelNotFound(err) {
		t.Fatalf("Execute() error = %v, want model_not_found", err)
	}
	if want := []string{"team-a/gpt-5"}; !equalStrings(host.models, want) {
		t.Fatalf("host models = %#v, want %#v", host.models, want)
	}
}

func TestMarkDemotesWeeklyOnlyAccounts(t *testing.T) {
	sorter := testAutoSorter(autoSortStrategyBorrow, "nominal", "weekly-only", "other")
	weeklyOnly := autoSortAccount{Secondary: autoSortWindow{Present: true, Remaining: 0.9, ResetAt: autoSortTestNow.Add(3 * day)}}
	setAutoSortState(sorter, "weekly-only", weeklyOnly)
	setAutoSortState(sorter, "other", codexAccount(0.1, time.Hour, 0.5, 3*day))
	if got := sorter.order("nominal"); !equalStrings(got, []string{"weekly-only", "other"}) {
		t.Fatalf("order() before mark = %#v", got)
	}
	sorter.markExhausted("weekly-only")
	if got := sorter.order("nominal"); !equalStrings(got, []string{"other", "weekly-only"}) {
		t.Fatalf("order() after mark = %#v, want weekly-only demoted", got)
	}
}

func TestDeriveModelsURLAcceptsBracketedIPv6Host(t *testing.T) {
	for host, want := range map[string]string{
		"[::1]": "http://[::1]:8317/v1/models",
		"::1":   "http://[::1]:8317/v1/models",
		"[::]":  "http://127.0.0.1:8317/v1/models",
	} {
		got, err := deriveModelsURL(CPAConfigSnapshot{Host: host})
		if err != nil || got != want {
			t.Fatalf("deriveModelsURL(%q) = (%q, %v), want %q", host, got, err, want)
		}
	}
}
