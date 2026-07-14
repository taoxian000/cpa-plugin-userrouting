package userrouting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const PluginIdentifier = "user-routing"

var supportedFormats = map[string]struct{}{
	"openai":          {},
	"openai-response": {},
	"claude":          {},
	"gemini":          {},
}

type HostCaller interface {
	Call(method string, payload any) (json.RawMessage, error)
}

type Runtime struct {
	host    HostCaller
	config  runtimeConfig
	catalog *modelCatalog
}

func NewRuntime(host HostCaller, rawConfig []byte, opts ConfigureOptions) (*Runtime, error) {
	if host == nil {
		return nil, errors.New("host caller is required")
	}
	cfg, err := decodeRuntimeConfig(rawConfig, opts)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		host:    host,
		config:  cfg,
		catalog: newModelCatalog(cfg),
	}, nil
}

func (r *Runtime) Route(req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	decline := pluginapi.ModelRouteResponse{Handled: false}
	if r == nil || !r.config.Enabled {
		return decline, nil
	}
	if _, ok := supportedFormats[strings.ToLower(strings.TrimSpace(req.SourceFormat))]; !ok {
		return decline, nil
	}
	if isCountTokensRequest(req.Metadata) {
		return decline, nil
	}
	if strings.TrimSpace(req.RequestedModel) == "" {
		return decline, nil
	}
	snapshot, err := r.config.CPAConfig.Snapshot()
	if err != nil {
		return decline, err
	}
	if r.config.StrictKeyValidation {
		if err := validateMappedKeys(r.config.PrefixMap, snapshot.APIKeys); err != nil {
			return decline, err
		}
	}
	apiKey := APIKeyFromRequest(req.Headers, req.Query, snapshot.APIKeys)
	if apiKey == "" {
		return decline, nil
	}
	specificPrefix, hasSpecific := r.config.PrefixMap[apiKey]
	defaultPrefix := r.config.PrefixMap["default"]
	if !hasSpecific && defaultPrefix == "" {
		return decline, nil
	}
	if hasSpecific && specificPrefix == "" && defaultPrefix == "" {
		return decline, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "downstream_api_key_model_prefix",
	}, nil
}

type Decision struct {
	RequestedModel string
	FinalModel     string
	UsedDefault    bool
	HadSpecific    bool
}

func (r *Runtime) Resolve(ctx context.Context, headers http.Header, query url.Values, requestedModel string) (Decision, error) {
	if r == nil {
		return Decision{}, errors.New("runtime is unavailable")
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return Decision{}, errors.New("requested model is empty")
	}
	snapshot, err := r.config.CPAConfig.Snapshot()
	if err != nil {
		return Decision{}, err
	}
	if r.config.StrictKeyValidation {
		if err := validateMappedKeys(r.config.PrefixMap, snapshot.APIKeys); err != nil {
			return Decision{}, err
		}
	}
	apiKey := APIKeyFromRequest(headers, query, snapshot.APIKeys)
	if apiKey == "" {
		return Decision{}, errors.New("request does not contain a valid CPA api-keys credential")
	}

	defaultModel := applyPrefix(r.config.PrefixMap["default"], requestedModel)
	specificPrefix, hasSpecific := r.config.PrefixMap[apiKey]
	decision := Decision{
		RequestedModel: requestedModel,
		FinalModel:     defaultModel,
		UsedDefault:    true,
		HadSpecific:    hasSpecific,
	}
	if !hasSpecific {
		return decision, nil
	}
	primaryModel := applyPrefix(specificPrefix, requestedModel)
	if primaryModel == defaultModel {
		decision.FinalModel = primaryModel
		decision.UsedDefault = false
		return decision, nil
	}
	exists, err := r.catalog.Exists(ctx, apiKey, primaryModel)
	if err != nil {
		return Decision{}, err
	}
	if exists {
		decision.FinalModel = primaryModel
		decision.UsedDefault = false
	}
	return decision, nil
}

func applyPrefix(prefix, model string) string {
	prefix = normalizePrefix(prefix)
	model = strings.TrimSpace(model)
	if prefix == "" || strings.HasPrefix(model, prefix) {
		return model
	}
	return prefix + model
}

func isCountTokensRequest(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	path, _ := metadata["request_path"].(string)
	path = strings.ToLower(strings.TrimSpace(path))
	return strings.HasSuffix(path, "/count_tokens")
}

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
}

func (r *Runtime) Execute(ctx context.Context, req pluginapi.ExecutorRequest, hostCallbackID string) (pluginapi.ExecutorResponse, error) {
	decision, err := r.Resolve(ctx, req.Headers, req.Query, req.Model)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	targets := r.quotaFallbackTargets(decision.FinalModel)
	for index, model := range targets {
		response, errExecute := r.executeModel(ctx, req, hostCallbackID, decision, model, index > 0)
		if errExecute == nil {
			return response, nil
		}
		if index+1 < len(targets) && r.shouldRetryQuotaFallback(errExecute) {
			continue
		}
		return pluginapi.ExecutorResponse{}, errExecute
	}
	return pluginapi.ExecutorResponse{}, errors.New("no quota fallback target available")
}

func (r *Runtime) RunStream(ctx context.Context, req pluginapi.ExecutorRequest, hostCallbackID, pluginStreamID string) error {
	decision, err := r.Resolve(ctx, req.Headers, req.Query, req.Model)
	if err != nil {
		return err
	}
	targets := r.quotaFallbackTargets(decision.FinalModel)
	for index, model := range targets {
		retryFallback, errStream := r.streamModel(ctx, req, hostCallbackID, pluginStreamID, decision, model, index > 0)
		if errStream == nil {
			return nil
		}
		if retryFallback && index+1 < len(targets) {
			continue
		}
		return errStream
	}
	return errors.New("no quota fallback target available")
}

func (r *Runtime) executeModel(ctx context.Context, req pluginapi.ExecutorRequest, hostCallbackID string, decision Decision, model string, quotaFallback bool) (pluginapi.ExecutorResponse, error) {
	body, err := rewrittenRequestBody(req, model)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	r.logDecision(hostCallbackID, req.SourceFormat, decision, model, quotaFallback)
	raw, err := r.host.Call(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocolOrFallback(req.SourceFormat, req.Format),
			ExitProtocol:  protocolOrFallback(req.Format, req.SourceFormat),
			Model:         model,
			Stream:        false,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: hostCallbackID,
	})
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("decode host model response: %w", err)
	}
	if response.StatusCode >= http.StatusBadRequest {
		return pluginapi.ExecutorResponse{}, &StatusError{Status: response.StatusCode, Message: strings.TrimSpace(string(response.Body))}
	}
	return pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers}, nil
}

// streamModel may retry only before it forwards a payload to the downstream client.
func (r *Runtime) streamModel(ctx context.Context, req pluginapi.ExecutorRequest, hostCallbackID, pluginStreamID string, decision Decision, model string, quotaFallback bool) (bool, error) {
	body, err := rewrittenRequestBody(req, model)
	if err != nil {
		return false, err
	}
	r.logDecision(hostCallbackID, req.SourceFormat, decision, model, quotaFallback)
	raw, err := r.host.Call(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocolOrFallback(req.SourceFormat, req.Format),
			ExitProtocol:  protocolOrFallback(req.Format, req.SourceFormat),
			Model:         model,
			Stream:        true,
			Body:          body,
			Headers:       req.Headers,
			Query:         req.Query,
			Alt:           req.Alt,
		},
		HostCallbackID: hostCallbackID,
	})
	if err != nil {
		return r.shouldRetryQuotaFallback(err), err
	}
	var response pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, fmt.Errorf("decode host model stream response: %w", err)
	}
	if strings.TrimSpace(response.StreamID) == "" {
		return false, errors.New("host model stream returned an empty stream_id")
	}
	defer func() {
		_, _ = r.host.Call(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: response.StreamID})
	}()

	emitted := false
	for {
		chunkRaw, err := r.host.Call(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: response.StreamID})
		if err != nil {
			return !emitted && r.shouldRetryQuotaFallback(err), err
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			return false, fmt.Errorf("decode host model stream chunk: %w", err)
		}
		if chunk.Error != "" {
			errChunk := errors.New(chunk.Error)
			return !emitted && r.shouldRetryQuotaFallback(errChunk), errChunk
		}
		if len(chunk.Payload) > 0 {
			if _, err := r.host.Call(pluginabi.MethodHostStreamEmit, map[string]any{
				"stream_id": pluginStreamID,
				"payload":   bytes.Clone(chunk.Payload),
			}); err != nil {
				return false, err
			}
			emitted = true
		}
		if chunk.Done {
			return false, nil
		}
	}
}

func (r *Runtime) quotaFallbackTargets(primaryModel string) []string {
	targets := []string{primaryModel}
	if r == nil || !r.config.QuotaFallback.Enabled {
		return targets
	}
	sourcePrefix, baseModel := r.quotaFallbackSource(primaryModel)
	if sourcePrefix == "" || baseModel == "" {
		return targets
	}
	for _, targetPrefix := range r.config.QuotaFallback.Prefixes[sourcePrefix] {
		target := targetPrefix + "/" + baseModel
		if target != primaryModel {
			targets = append(targets, target)
		}
	}
	return targets
}

func (r *Runtime) quotaFallbackSource(model string) (string, string) {
	if r == nil {
		return "", ""
	}
	model = strings.TrimSpace(model)
	bestPrefix := ""
	for sourcePrefix := range r.config.QuotaFallback.Prefixes {
		needle := sourcePrefix + "/"
		if strings.HasPrefix(model, needle) && len(sourcePrefix) > len(bestPrefix) {
			bestPrefix = sourcePrefix
		}
	}
	if bestPrefix == "" {
		return "", ""
	}
	return bestPrefix, strings.TrimPrefix(model, bestPrefix+"/")
}

func isCodexQuotaExhausted(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "usage_limit_reached")
}

func (r *Runtime) shouldRetryQuotaFallback(err error) bool {
	if isCodexQuotaExhausted(err) {
		return true
	}
	return r != nil && r.config.QuotaFallback.FallbackOnOtherErrors
}

func (r *Runtime) logDecision(hostCallbackID, sourceFormat string, decision Decision, model string, quotaFallback bool) {
	if r == nil || !r.config.LogRouting {
		return
	}
	_, _ = r.host.Call(pluginabi.MethodHostLog, hostLogRequest{
		HostCallbackID: hostCallbackID,
		Level:          "info",
		Message:        "user-routing selected execution model",
		Fields: map[string]any{
			"plugin":          PluginIdentifier,
			"source_format":   sourceFormat,
			"requested_model": decision.RequestedModel,
			"model":           model,
			"used_default":    decision.UsedDefault,
			"quota_fallback":  quotaFallback,
		},
	})
}

func rewrittenRequestBody(req pluginapi.ExecutorRequest, model string) ([]byte, error) {
	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	body = bytes.Clone(body)
	if len(body) == 0 || !gjson.ValidBytes(body) || !gjson.GetBytes(body, "model").Exists() {
		return body, nil
	}
	rewritten, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, fmt.Errorf("rewrite request model: %w", err)
	}
	return rewritten, nil
}

func protocolOrFallback(primary, fallback string) string {
	if value := strings.TrimSpace(primary); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}

type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	if e == nil {
		return "request failed"
	}
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("request failed with status %d", e.Status)
}

func (e *StatusError) StatusCode() int {
	if e == nil || e.Status == 0 {
		return http.StatusBadGateway
	}
	return e.Status
}
