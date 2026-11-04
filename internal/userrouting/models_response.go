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
	body, changed := removePrefixedModelEntries(req.Body, r.deduplicationPrefixes(), req.SourceFormat)
	if !changed {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	return pluginapi.ResponseInterceptResponse{Body: body}, nil
}

func removePrefixedModelEntries(body []byte, prefixes []string, sourceFormat string) ([]byte, bool) {
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
		visibleModels := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			if model := modelListEntryID(entry, sourceFormat); model != "" {
				visibleModels[model] = struct{}{}
			}
		}
		filtered := make([]json.RawMessage, 0, len(entries))
		fieldChanged := false
		for _, entry := range entries {
			if base, hasPrefix := stripModelPrefix(modelListEntryID(entry, sourceFormat), prefixes); hasPrefix {
				if _, hasReplacement := visibleModels[base]; hasReplacement {
					fieldChanged = true
					continue
				}
			}
			filtered = append(filtered, entry)
		}
		if !fieldChanged || len(filtered) == 0 {
			continue
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			continue
		}
		payload[field] = encoded
		if field == "data" {
			updateModelListBoundary(payload, "first_id", filtered[0])
			updateModelListBoundary(payload, "last_id", filtered[len(filtered)-1])
		}
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

func modelListEntryID(entry json.RawMessage, sourceFormat string) string {
	var model struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(entry, &model) != nil {
		return ""
	}
	id := strings.TrimSpace(model.ID)
	if id == "" {
		id = strings.TrimSpace(model.Name)
	}
	if strings.EqualFold(sourceFormat, "claude") {
		id = decodeClaudeCatalogModelID(id)
	}
	return strings.TrimPrefix(id, "models/")
}

func updateModelListBoundary(payload map[string]json.RawMessage, field string, entry json.RawMessage) {
	if _, exists := payload[field]; !exists {
		return
	}
	var model struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(entry, &model) != nil {
		return
	}
	if encoded, err := json.Marshal(model.ID); err == nil {
		payload[field] = encoded
	}
}
