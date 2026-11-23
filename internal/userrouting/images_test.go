package userrouting

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

type capturedHostImageCall struct {
	EntryProtocol string      `json:"entry_protocol"`
	ExitProtocol  string      `json:"exit_protocol"`
	Model         string      `json:"model"`
	Body          []byte      `json:"body"`
	Headers       http.Header `json:"headers"`
	Path          *string     `json:"path"`
}

func decodeHostImageCall(t *testing.T, raw []byte) capturedHostImageCall {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("host model call was not made")
	}
	var call capturedHostImageCall
	if err := json.Unmarshal(raw, &call); err != nil {
		t.Fatalf("decode host call: %v", err)
	}
	return call
}

func TestRouteHandlesImageFormat(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-image-2"})
	response, err := runtime.Route(pluginapi.ModelRouteRequest{
		SourceFormat:   "openai-image",
		RequestedModel: "gpt-image-2",
		Headers:        bearerHeader("key-1"),
		Metadata:       map[string]any{"request_path": "/v1/images/generations"},
	}, "")
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if !response.Handled || response.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("Route() response = %#v", response)
	}
}

func TestExecuteImageGenerationRewritesModelAndKeepsPath(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-image-2", "fallback/gpt-image-2"})
	body := []byte(`{"model":"gpt-image-2","prompt":"a red apple","stream":false}`)
	response, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:           "gpt-image-2",
		Format:          "openai-image",
		SourceFormat:    "openai-image",
		Headers:         bearerHeader("key-1"),
		Payload:         body,
		OriginalRequest: body,
		Metadata:        map[string]any{"request_path": "/v1/images/generations"},
	}, "callback-1")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(response.Payload) != `{"ok":true}` {
		t.Fatalf("response payload = %s", response.Payload)
	}
	call := decodeHostImageCall(t, runtime.host.(*fakeHost).call(pluginabi.MethodHostModelExecute))
	if call.Model != "team-a/gpt-image-2" || call.EntryProtocol != "openai-image" || call.ExitProtocol != "openai-image" {
		t.Fatalf("host call = %#v", call)
	}
	if call.Path == nil || *call.Path != "/v1/images/generations" {
		t.Fatalf("host path = %v, want /v1/images/generations", call.Path)
	}
	if got := gjson.GetBytes(call.Body, "model").String(); got != "team-a/gpt-image-2" {
		t.Fatalf("body model = %q", got)
	}
	if got := gjson.GetBytes(call.Body, "prompt").String(); got != "a red apple" {
		t.Fatalf("body prompt = %q", got)
	}
}

func TestExecuteImageGenerationUsesDefaultPrefixWhenSpecificModelIsMissing(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"fallback/gpt-image-2"})
	body := []byte(`{"model":"gpt-image-2","prompt":"cat"}`)
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-image-2", Format: "openai-image", SourceFormat: "openai-image",
		Headers: bearerHeader("key-1"), Payload: body, OriginalRequest: body,
		Metadata: map[string]any{"request_path": "/v1/images/generations"},
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	call := decodeHostImageCall(t, runtime.host.(*fakeHost).call(pluginabi.MethodHostModelExecute))
	if call.Model != "fallback/gpt-image-2" {
		t.Fatalf("host model = %q", call.Model)
	}
}

func TestExecuteMultipartImageEditForwardsBodyUnchanged(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-image-2"})
	body := []byte("--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt-image-2\r\n--b\r\nContent-Disposition: form-data; name=\"image\"; filename=\"a.png\"\r\nContent-Type: image/png\r\n\r\n\x89PNG\x00\x01\r\n--b--\r\n")
	headers := bearerHeader("key-1")
	headers.Set("Content-Type", "multipart/form-data; boundary=b")
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-image-2", Format: "openai-image", SourceFormat: "openai-image",
		Headers: headers, Payload: body, OriginalRequest: body,
		Metadata: map[string]any{"request_path": "/v1/images/edits"},
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	call := decodeHostImageCall(t, runtime.host.(*fakeHost).call(pluginabi.MethodHostModelExecute))
	if call.Model != "team-a/gpt-image-2" {
		t.Fatalf("host model = %q", call.Model)
	}
	if !bytes.Equal(call.Body, body) {
		t.Fatalf("multipart body changed: %q", call.Body)
	}
	if got := call.Headers.Get("Content-Type"); got != "multipart/form-data; boundary=b" {
		t.Fatalf("Content-Type = %q", got)
	}
	if call.Path == nil || *call.Path != "/v1/images/edits" {
		t.Fatalf("host path = %v, want /v1/images/edits", call.Path)
	}
}

func TestExecuteImageUsesHandlerPayloadInsteadOfOriginalRequest(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/grok-imagine-image"})
	payload := []byte(`{"model":"grok-imagine-image","prompt":"cat","aspect_ratio":"1:1"}`)
	original := []byte(`{"model":"grok-imagine-image","prompt":"cat","size":"1024x1024"}`)
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "grok-imagine-image", Format: "openai-image", SourceFormat: "openai-image",
		Headers: bearerHeader("key-1"), Payload: payload, OriginalRequest: original,
		Metadata: map[string]any{"request_path": "/v1/images/generations"},
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	call := decodeHostImageCall(t, runtime.host.(*fakeHost).call(pluginabi.MethodHostModelExecute))
	if got := gjson.GetBytes(call.Body, "aspect_ratio").String(); got != "1:1" {
		t.Fatalf("body = %s, want handler payload", call.Body)
	}
	if got := gjson.GetBytes(call.Body, "model").String(); got != "team-a/grok-imagine-image" {
		t.Fatalf("body model = %q", got)
	}
}

func TestExecuteNonImageRequestOmitsPath(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-5"})
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-5", Format: "openai", SourceFormat: "openai",
		Headers: bearerHeader("key-1"), OriginalRequest: []byte(`{"model":"gpt-5","messages":[]}`),
		Metadata: map[string]any{"request_path": "/v1/chat/completions"},
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	call := decodeHostImageCall(t, runtime.host.(*fakeHost).call(pluginabi.MethodHostModelExecute))
	if call.Path != nil {
		t.Fatalf("host path = %q, want omitted", *call.Path)
	}
}

func TestImageExecuteFallsBackAcrossPrefixesAfterCodexQuotaExhausted(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-image-2", "team-b/gpt-image-2"})
	host := &quotaFallbackHost{quotaError: `host_call_failed: {"error":{"type":"usage_limit_reached"}}`}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"team-a": {"team-b"}}}
	body := []byte(`{"model":"gpt-image-2","prompt":"cat"}`)
	_, err := runtime.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-image-2", Format: "openai-image", SourceFormat: "openai-image",
		Headers: bearerHeader("key-1"), Payload: body, OriginalRequest: body,
		Metadata: map[string]any{"request_path": "/v1/images/generations"},
	}, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := host.models, []string{"team-a/gpt-image-2", "team-b/gpt-image-2"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
}

func TestImageRunStreamKeepsPathAndFallsBackBeforeFirstPayload(t *testing.T) {
	runtime, _ := testRuntime(t, []string{"team-a/gpt-image-2", "team-b/gpt-image-2"})
	host := &streamQuotaFallbackHost{}
	runtime.host = host
	runtime.config.QuotaFallback = quotaFallbackConfig{Enabled: true, Prefixes: map[string][]string{"team-a": {"team-b"}}}
	body := []byte(`{"model":"gpt-image-2","prompt":"cat","stream":true}`)
	err := runtime.RunStream(context.Background(), pluginapi.ExecutorRequest{
		Model: "gpt-image-2", Format: "openai-image", SourceFormat: "openai-image", Stream: true,
		Headers: bearerHeader("key-1"), Payload: body, OriginalRequest: body,
		Metadata: map[string]any{"request_path": "/v1/images/edits"},
	}, "callback-1", "plugin-stream")
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if got, want := host.models, []string{"team-a/gpt-image-2", "team-b/gpt-image-2"}; !equalStrings(got, want) {
		t.Fatalf("host models = %#v, want %#v", got, want)
	}
	if got := string(host.emitted); got != "data: done\n\n" {
		t.Fatalf("emitted payload = %q", got)
	}
	for _, path := range host.paths {
		if path != "/v1/images/edits" {
			t.Fatalf("stream paths = %#v", host.paths)
		}
	}
	if len(host.paths) != 2 {
		t.Fatalf("stream paths = %#v", host.paths)
	}
}
