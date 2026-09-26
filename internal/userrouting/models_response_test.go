package userrouting

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestInterceptResponseHidesPrefixedModelsButKeepsDeduplicatedModels(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"a/gpt-5.5"}},
	})
	runtime.config.RegisterDeduplicatedModels = true
	runtime.config.HidePrefixedModels = true
	if response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{StatusCode: http.StatusOK, Body: []byte(`{"data":[{"id":"a/gpt-5.5"}]}`)}); err != nil || len(response.Body) != 0 {
		t.Fatalf("model-list response before discovery = (%q, %v), want no filtering", response.Body, err)
	}
	if _, err := runtime.RegisterModels(context.Background()); err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	internalHeaders := make(http.Header)
	internalHeaders.Set(internalCatalogBypassHeader, runtime.catalog.internalBypassToken)
	internalResponse, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		StatusCode:     http.StatusOK,
		RequestHeaders: internalHeaders,
		Body:           []byte(`{"data":[{"id":"a/gpt-5.5"}]}`),
	})
	if err != nil || len(internalResponse.Body) != 0 {
		t.Fatalf("internal catalog response = (%q, %v), want original response", internalResponse.Body, err)
	}

	body := []byte(`{"object":"list","data":[{"id":"a/gpt-5.5","object":"model"},{"id":"b/gpt-5.5","object":"model"},{"id":"fallback/gpt-5.5","object":"model"},{"id":"gpt-5.5","object":"model"},{"id":"other/model","object":"model"}]}`)
	response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{StatusCode: http.StatusOK, Body: body})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if len(response.Body) == 0 {
		t.Fatal("expected rewritten response body")
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	if got, want := len(payload.Data), 2; got != want {
		t.Fatalf("remaining model count = %d, want %d: %s", got, want, response.Body)
	}
	if payload.Data[0].ID != "gpt-5.5" || payload.Data[1].ID != "other/model" {
		t.Fatalf("remaining models = %#v, want unprefixed models", payload.Data)
	}
}

func TestInterceptResponseHandlesGeminiModelsField(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"a/gemini-3"}},
	})
	runtime.config.RegisterDeduplicatedModels = true
	runtime.config.HidePrefixedModels = true
	if _, err := runtime.RegisterModels(context.Background()); err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}

	response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"models":[{"name":"models/a/gemini-3"},{"name":"models/gemini-3"}]}`),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	var payload struct {
		Models []map[string]string `json:"models"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	if len(payload.Models) != 1 || payload.Models[0]["name"] != "models/gemini-3" {
		t.Fatalf("remaining models = %#v, want only unprefixed Gemini model", payload.Models)
	}
}

func TestInterceptResponseIsNoopUnlessEnabledForModelLists(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"a/gpt-5.5"}},
	})
	body := []byte(`{"data":[{"id":"a/gpt-5.5"}]}`)

	response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{StatusCode: http.StatusOK, Body: body})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if len(response.Body) != 0 {
		t.Fatalf("disabled interceptor returned body %q, want no rewrite", response.Body)
	}

	runtime.config.HidePrefixedModels = true
	runtime.config.RegisterDeduplicatedModels = true
	if response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{StatusCode: http.StatusOK, Body: body}); err != nil || len(response.Body) != 0 {
		t.Fatalf("model-list response before discovery = (%q, %v), want no filtering", response.Body, err)
	}
	if _, err := runtime.RegisterModels(context.Background()); err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	response, err = runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		StatusCode: http.StatusOK,
		Model:      "gpt-5.5",
		Body:       body,
	})
	if err != nil {
		t.Fatalf("InterceptResponse() execution response error = %v", err)
	}
	if len(response.Body) != 0 {
		t.Fatalf("execution response was unexpectedly rewritten: %q", response.Body)
	}
}

func TestInterceptResponseFailsOpenWhenNoDeduplicatedModelsWereDiscovered(t *testing.T) {
	runtime := modelRegistrationRuntime(t, map[string]struct {
		status int
		models []string
	}{
		"key-a": {status: http.StatusOK, models: []string{"unrelated/model"}},
	})
	runtime.config.RegisterDeduplicatedModels = true
	runtime.config.HidePrefixedModels = true
	if _, err := runtime.RegisterModels(context.Background()); err != nil {
		t.Fatalf("RegisterModels() error = %v", err)
	}
	body := []byte(`{"data":[{"id":"a/gpt-5.5"}]}`)
	response, err := runtime.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		StatusCode: http.StatusOK,
		Body:       body,
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if len(response.Body) != 0 {
		t.Fatalf("response = %q, want no filtering when registration discovered no replacements", response.Body)
	}
}
