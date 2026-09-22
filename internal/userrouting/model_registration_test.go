package userrouting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegisterModelsDeduplicatesAndIncludesDefaultPrefix(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a":    {status: http.StatusOK, models: []string{"a/gpt-5.5", "a/claude", "fallback/default-only", "other/x"}},
		"key-b":    {status: http.StatusOK, models: []string{"b/gpt-5.5", "b/claude", "b/unique"}},
		"key-fail": {status: http.StatusInternalServerError},
	})

	response, err := runtime.RegisterModels(context.Background())
	if err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	if response.Provider != modelRegistrationProvider {
		t.Fatalf("provider = %q, want %q", response.Provider, modelRegistrationProvider)
	}
	if got, want := registeredModelIDs(response.Models), []string{"claude", "default-only", "gpt-5.5", "unique"}; !equalStrings(got, want) {
		t.Fatalf("registered models = %#v, want %#v", got, want)
	}
}

func TestRegisterModelsCanExcludeDefaultPrefix(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"a/gpt-5.5", "fallback/default-only"}},
	})
	runtime.config.IncludeDefaultPrefix = false

	response, err := runtime.RegisterModels(context.Background())
	if err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	if got, want := registeredModelIDs(response.Models), []string{"gpt-5.5"}; !equalStrings(got, want) {
		t.Fatalf("registered models = %#v, want %#v", got, want)
	}
}

func TestRegisterModelsDisabledReturnsEmptyModels(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"a/gpt-5.5"}},
	})
	runtime.config.RegisterDeduplicatedModels = false

	response, err := runtime.RegisterModels(context.Background())
	if err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	if response.Provider != modelRegistrationProvider || len(response.Models) != 0 {
		t.Fatalf("response = %#v, want provider with no models", response)
	}
}

func TestRegisterModelsUsesLongestPrefix(t *testing.T) {
	if got, ok := stripModelPrefix("team/codex/gpt-5", []string{"team/codex/", "team/"}); !ok || got != "gpt-5" {
		t.Fatalf("stripModelPrefix() = (%q, %v), want (gpt-5, true)", got, ok)
	}
}

func modelRegistrationRuntime(t *testing.T, responses map[string]struct {
	status int
	models []string
}) *Runtime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("Authorization")
		if len(key) > len("Bearer ") {
			key = key[len("Bearer "):]
		}
		response, ok := responses[key]
		if !ok {
			http.Error(w, "unknown key", http.StatusUnauthorized)
			return
		}
		if response.status != http.StatusOK {
			http.Error(w, "catalog failed", response.status)
			return
		}
		data := make([]map[string]string, 0, len(response.models))
		for _, model := range response.models {
			data = append(data, map[string]string{"id": model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)

	path := writeCPAConfig(t, "key-a", "key-b", "key-fail")
	raw := "cpa_config_path: " + quotedYAML(path) + "\n" +
		"models_url: " + quotedYAML(server.URL+"/v1/models") + "\n" +
		"register_deduplicated_models: true\n" +
		"include_default_prefix: true\n" +
		"prefix_map:\n" +
		"  key-a: a\n" +
		"  key-b: b\n" +
		"  key-fail: fail\n" +
		"  default: fallback\n"
	runtime, err := NewRuntime(&fakeHost{}, []byte(raw), ConfigureOptions{})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return runtime
}

func registeredModelIDs(models []pluginapi.ModelInfo) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}
