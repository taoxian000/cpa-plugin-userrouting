//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/taoxian000/cpa-plugin-userrouting/internal/userrouting"
)

// pluginVersion can be overridden by the release workflow with -ldflags -X.
var pluginVersion = "0.2.0"

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

type hostCaller struct{}

type envelope struct {
	OK     bool             `json:"ok"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *pluginabi.Error `json:"error,omitempty"`
}

var currentRuntime atomic.Pointer[userrouting.Runtime]

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", http.StatusBadRequest))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelopeFor(err))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	currentRuntime.Store(nil)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get working directory: %w", err)
		}
		runtime, err := userrouting.NewRuntime(hostCaller{}, req.ConfigYAML, userrouting.ConfigureOptions{
			Args: os.Args,
			CWD:  cwd,
		})
		if err != nil {
			return nil, err
		}
		currentRuntime.Store(runtime)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		runtime, err := loadedRuntime()
		if err != nil {
			return errorEnvelopeFor(err), nil
		}
		var req rpcModelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelopeFor(err), nil
		}
		response, err := runtime.Route(req.ModelRouteRequest, req.HostCallbackID)
		if err != nil {
			return errorEnvelopeFor(err), nil
		}
		return okEnvelope(response)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": userrouting.PluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		runtime, err := loadedRuntime()
		if err != nil {
			return errorEnvelopeFor(err), nil
		}
		var req rpcExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelopeFor(err), nil
		}
		response, err := runtime.Execute(context.Background(), req.ExecutorRequest, req.HostCallbackID)
		if err != nil {
			return errorEnvelopeFor(err), nil
		}
		return okEnvelope(response)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return errorEnvelope("unsupported_count_tokens", "user-routing leaves count_tokens requests on CPA's native path", http.StatusNotImplemented), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return errorEnvelope("unsupported_http_request", "user-routing does not implement executor.http_request", http.StatusNotImplemented), nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

func executeStream(request []byte) ([]byte, error) {
	runtime, err := loadedRuntime()
	if err != nil {
		return errorEnvelopeFor(err), nil
	}
	var req rpcExecutorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelopeFor(err), nil
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required", http.StatusBadRequest), nil
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("stream panic: %v", recovered))
			}
		}()
		err := runtime.RunStream(context.Background(), req.ExecutorRequest, req.HostCallbackID, streamID)
		if err != nil {
			closePluginStream(streamID, err.Error())
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func closePluginStream(streamID, message string) {
	_, _ = (hostCaller{}).Call(pluginabi.MethodHostStreamClose, map[string]any{
		"stream_id": streamID,
		"error":     strings.TrimSpace(message),
	})
}

func loadedRuntime() (*userrouting.Runtime, error) {
	runtime := currentRuntime.Load()
	if runtime == nil {
		return nil, fmt.Errorf("plugin is not configured")
	}
	return runtime, nil
}

func pluginRegistration() registration {
	formats := []string{"openai", "openai-response", "claude", "gemini", "openai-video"}
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "CPA User Routing",
			Version:          pluginVersion,
			Author:           "CPA-Plugin-userrouting contributors",
			GitHubRepository: "https://github.com/taoxian000/CPA-Plugin-userrouting",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable downstream API-key model prefix routing."},
				{Name: "cpa_config_path", Type: pluginapi.ConfigFieldTypeString, Description: "CPA config.yaml path. Empty uses the CPA -config argument, CPA_CONFIG_PATH, or ./config.yaml."},
				{Name: "prefix_map", Type: pluginapi.ConfigFieldTypeObject, Description: "Map native CPA api-keys to model prefixes. The reserved default key is the fallback prefix."},
				{Name: "quota_fallback", Type: pluginapi.ConfigFieldTypeObject, Description: "Optional ordered cross-prefix fallback. Set fallback_on_other_errors to retry other upstream errors too."},
				{Name: "strict_key_validation", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Reject mapped keys that are not present in CPA api-keys."},
				{Name: "models_url", Type: pluginapi.ConfigFieldTypeString, Description: "Optional absolute CPA /v1/models URL; normally derived from config.yaml."},
				{Name: "model_cache_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long to cache the CPA model catalog, for example 5s."},
				{Name: "model_lookup_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Timeout for the local CPA model catalog lookup, for example 3s."},
				{Name: "models_tls_insecure_skip_verify", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Allow an untrusted certificate only for the local CPA models URL."},
				{Name: "log_routing", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Write requested_model, final model, and fallback status to the CPA main log."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  append([]string(nil), formats...),
			ExecutorOutputFormats: append([]string(nil), formats...),
		},
	}
}

func (hostCaller) Call(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelopeFor(err error) []byte {
	status := http.StatusBadGateway
	if statusErr, ok := err.(interface{ StatusCode() int }); ok {
		status = statusErr.StatusCode()
	}
	return errorEnvelope("plugin_error", err.Error(), status)
}

func errorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
