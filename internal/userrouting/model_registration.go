package userrouting

import (
	"context"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const modelRegistrationProvider = PluginIdentifier

// RegisterModels contributes additive, unprefixed aliases to CPA's model
// registry. The original prefixed models remain available in CPA.
func (r *Runtime) RegisterModels(ctx context.Context) (pluginapi.ModelRegistrationResponse, error) {
	response := pluginapi.ModelRegistrationResponse{Provider: modelRegistrationProvider}
	if r == nil || !r.config.RegisterDeduplicatedModels {
		return response, nil
	}

	prefixes := r.deduplicationPrefixes()
	if len(prefixes) == 0 || r.catalog == nil {
		return response, nil
	}

	keys := make([]string, 0)
	seenKeys := make(map[string]struct{})
	for key, prefix := range r.config.PrefixMap {
		if key == "default" || prefix == "" {
			continue
		}
		if _, seen := seenKeys[key]; seen {
			continue
		}
		seenKeys[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	models := make(map[string]pluginapi.ModelInfo)
	for _, key := range keys {
		catalog, err := r.catalog.Models(ctx, key)
		if err != nil {
			continue
		}
		modelIDs := make([]string, 0, len(catalog))
		for modelID := range catalog {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			base, ok := stripModelPrefix(modelID, prefixes)
			if !ok || base == "" {
				continue
			}
			if _, exists := models[base]; exists {
				continue
			}
			models[base] = pluginapi.ModelInfo{
				ID:          base,
				Object:      "model",
				OwnedBy:     modelRegistrationProvider,
				Name:        base,
				DisplayName: base,
			}
		}
	}

	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	response.Models = make([]pluginapi.ModelInfo, 0, len(ids))
	for _, id := range ids {
		response.Models = append(response.Models, models[id])
	}
	return response, nil
}

func (r *Runtime) deduplicationPrefixes() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	prefixes := make([]string, 0, len(r.config.PrefixMap))
	for key, prefix := range r.config.PrefixMap {
		if key == "default" || prefix == "" {
			continue
		}
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	if r.config.IncludeDefaultPrefix {
		if prefix := r.config.PrefixMap["default"]; prefix != "" {
			if _, exists := seen[prefix]; !exists {
				prefixes = append(prefixes, prefix)
			}
		}
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) != len(prefixes[j]) {
			return len(prefixes[i]) > len(prefixes[j])
		}
		return prefixes[i] < prefixes[j]
	})
	return prefixes
}

func stripModelPrefix(model string, prefixes []string) (string, bool) {
	model = strings.TrimSpace(model)
	for _, prefix := range prefixes {
		if strings.HasPrefix(model, prefix) {
			return strings.TrimPrefix(model, prefix), true
		}
	}
	return "", false
}
