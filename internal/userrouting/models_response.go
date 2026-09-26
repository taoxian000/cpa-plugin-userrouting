package userrouting

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// InterceptResponse removes configured prefixed model IDs from model-list
// responses only. The models remain registered with CPA, so direct requests
// that name them continue to route normally.
func (r *Runtime) InterceptResponse(_ context.Context, req pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	if r == nil || !r.HidePrefixedModels() || !r.deduplicatedModelsLoaded.Load() || req.StatusCode != 200 || req.Stream || req.Model != "" || req.RequestedModel != "" {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	if r.catalog.isInternalRequest(req.RequestHeaders) {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	body, changed := removePrefixedModelEntries(req.Body, r.deduplicationPrefixes())
	if !changed {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	return pluginapi.ResponseInterceptResponse{Body: body}, nil
}

func removePrefixedModelEntries(body []byte, prefixes []string) ([]byte, bool) {
	if len(body) == 0 || len(prefixes) == 0 {
		return body, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, false
	}

	changed := false
	for _, field := range []string{"data", "models"} {
		raw, exists := payload[field]
		if !exists {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			continue
		}
		filtered := make([]json.RawMessage, 0, len(entries))
		fieldChanged := false
		for _, entry := range entries {
			if isPrefixedModelEntry(entry, prefixes) {
				fieldChanged = true
				continue
			}
			filtered = append(filtered, entry)
		}
		if !fieldChanged {
			continue
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			continue
		}
		payload[field] = encoded
		changed = true
	}
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func isPrefixedModelEntry(entry json.RawMessage, prefixes []string) bool {
	var model map[string]json.RawMessage
	if err := json.Unmarshal(entry, &model); err != nil || model == nil {
		return false
	}
	for _, field := range []string{"id", "name"} {
		var value string
		if err := json.Unmarshal(model[field], &value); err != nil {
			continue
		}
		value = strings.TrimPrefix(strings.TrimSpace(value), "models/")
		for _, prefix := range prefixes {
			if prefix != "" && strings.HasPrefix(value, prefix) {
				return true
			}
		}
	}
	return false
}
