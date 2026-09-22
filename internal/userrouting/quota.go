package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	quotaProviderIdentifier = "user-routing-quota"
	quotaProviderName       = "Codex quota"
	quotaResourcePath       = "/quota"
	quotaCodexProvider      = "codex"
)

// QuotaProviderIdentifier returns the stable plugin quota capability ID used
// by the CPA plugin host.
func QuotaProviderIdentifier() string { return quotaProviderIdentifier }

func (r *Runtime) QuotaProviderEnabled() bool {
	return r != nil && r.config.Enabled && r.config.QuotaProvider.Enabled
}

func (r *Runtime) QuotaResourceEnabled() bool {
	return r != nil && r.config.Enabled && r.config.QuotaProvider.Enabled && r.config.QuotaProvider.PublicEndpoint
}

// rpcHostHTTPRequest mirrors CPA's host.http.do wire request. The host HTTP
// callback supplies CPA's proxy/TLS policy; credentials are added by the
// provider adapter from the auth storage JSON.
type rpcHostHTTPRequest struct {
	HostCallbackID string                     `json:"host_callback_id,omitempty"`
	Method         string                     `json:"method,omitempty"`
	URL            string                     `json:"url,omitempty"`
	Headers        http.Header                `json:"headers,omitempty"`
	Body           []byte                     `json:"body,omitempty"`
	WireProfile    *pluginapi.HTTPWireProfile `json:"wire_profile,omitempty"`
}

type quotaHTTPClient struct {
	host       HostCaller
	callbackID string
}

func (c *quotaHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if c == nil || c.host == nil {
		return pluginapi.HTTPResponse{}, errors.New("host HTTP client is unavailable")
	}
	raw, err := c.host.Call(pluginabi.MethodHostHTTPDo, rpcHostHTTPRequest{
		HostCallbackID: c.callbackID,
		Method:         req.Method,
		URL:            req.URL,
		Headers:        req.Headers,
		Body:           req.Body,
		WireProfile:    req.WireProfile,
	})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var response pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", err)
	}
	return response, nil
}

func (c *quotaHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("quota provider does not use streaming HTTP")
}

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// QuotaIdentifier is exposed through quota.identifier.
func (r *Runtime) QuotaIdentifier() string {
	return quotaProviderIdentifier
}

// DescribeQuota is exposed through quota.describe. This plugin intentionally
// advertises Codex only; other CPA providers are not routed by this plugin.
func (r *Runtime) DescribeQuota(context.Context) pluginapi.QuotaDescribeResponse {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled {
		return pluginapi.QuotaDescribeResponse{}
	}
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{quotaCodexProvider},
		DisplayName:        quotaProviderName,
		SupportsReset:      false,
	}
}

// FetchQuota is exposed through quota.fetch for CPA's native Management API.
func (r *Runtime) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest, callbackID string) (pluginapi.QuotaFetchResponse, error) {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled {
		return pluginapi.QuotaFetchResponse{}, errors.New("quota provider is disabled")
	}
	if !strings.EqualFold(strings.TrimSpace(req.Provider), quotaCodexProvider) {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("unsupported quota provider %q", req.Provider)
	}
	if req.HTTPClient == nil {
		req.HTTPClient = &quotaHTTPClient{host: r.host, callbackID: callbackID}
	}
	return fetchCodexQuota(ctx, req)
}

// ResetQuota is intentionally read-only. CPA still requires the method for
// the QuotaProvider ABI, so report a structured unsupported result.
func (r *Runtime) ResetQuota(context.Context, pluginapi.QuotaResetRequest, string) pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{
		Success: false,
		Message: "Codex quota reset is not supported by user-routing",
	}
}

type quotaResourceResponse struct {
	NominalPrefix string                      `json:"nominal_prefix"`
	ActualPrefix  string                      `json:"actual_prefix,omitempty"`
	Prefixes      []quotaResourcePrefixResult `json:"prefixes"`
	Partial       bool                        `json:"partial"`
}

type quotaResourcePrefixResult struct {
	NominalPrefix string                 `json:"nominal_prefix"`
	ActualPrefix  string                 `json:"actual_prefix"`
	Provider      string                 `json:"provider"`
	Accounts      []quotaResourceAccount `json:"accounts,omitempty"`
	Errors        []string               `json:"errors,omitempty"`
}

type quotaResourceAccount struct {
	Provider string                       `json:"provider"`
	Quota    pluginapi.QuotaFetchResponse `json:"quota"`
}

func (r *Runtime) RegisterManagement() pluginapi.ManagementRegistrationResponse {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled || !r.config.QuotaProvider.PublicEndpoint {
		return pluginapi.ManagementRegistrationResponse{}
	}
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{{
			Path:        quotaResourcePath,
			Description: "Query Codex quota using a downstream CPA API key.",
		}},
	}
}

// HandleManagement handles the public ResourceRoute. Resource routes are not
// protected by CPA's Management Key, so the downstream CPA API key is checked
// against the native api-keys list before any auth material is read.
func (r *Runtime) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest, callbackID string) (pluginapi.ManagementResponse, error) {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled || !r.config.QuotaProvider.PublicEndpoint {
		return quotaJSONResponse(http.StatusNotFound, map[string]string{"error": "quota resource is disabled"}), nil
	}
	if req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet) {
		return quotaJSONResponse(http.StatusMethodNotAllowed, map[string]string{"error": "GET is required"}), nil
	}
	snapshot, err := r.config.CPAConfig.Snapshot()
	if err != nil {
		return quotaJSONResponse(http.StatusInternalServerError, map[string]string{"error": "CPA configuration unavailable"}), nil
	}
	if r.config.StrictKeyValidation {
		if err := validateMappedKeys(r.config.PrefixMap, snapshot.APIKeys); err != nil {
			return quotaJSONResponse(http.StatusInternalServerError, map[string]string{"error": "plugin prefix configuration is invalid"}), nil
		}
	}
	apiKey := APIKeyFromRequest(req.Headers, req.Query, snapshot.APIKeys)
	if apiKey == "" {
		return quotaJSONResponse(http.StatusUnauthorized, map[string]string{"error": "a valid CPA API key is required"}), nil
	}
	nominalPrefix, hasSpecific := r.config.PrefixMap[apiKey]
	if !hasSpecific {
		nominalPrefix = r.config.PrefixMap["default"]
	}
	nominalName := normalizePrefixName(nominalPrefix)
	candidates := r.quotaFallbackPrefixes(nominalName)
	if len(candidates) == 0 {
		candidates = []string{""}
	}

	listRaw, err := r.host.Call(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA auth list is unavailable"}), nil
	}
	var listed hostAuthListResponse
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "invalid CPA auth list response"}), nil
	}

	result := quotaResourceResponse{NominalPrefix: normalizePrefix(nominalName), Prefixes: make([]quotaResourcePrefixResult, 0, len(candidates))}
	actualSet := false
	for _, candidate := range candidates {
		prefixResult := quotaResourcePrefixResult{
			NominalPrefix: normalizePrefix(nominalName),
			ActualPrefix:  normalizePrefix(candidate),
			Provider:      quotaCodexProvider,
		}
		for _, entry := range listed.Files {
			if entry.Disabled || entry.Unavailable || !strings.EqualFold(strings.TrimSpace(entry.Provider), quotaCodexProvider) {
				continue
			}
			rawAuth, errGet := r.getAuthJSON(entry.AuthIndex)
			if errGet != nil {
				prefixResult.Errors = append(prefixResult.Errors, "one Codex credential could not be read")
				continue
			}
			material := decodeAuthMaterial(rawAuth)
			if material.prefix() != candidate {
				continue
			}
			quota, errQuota := r.FetchQuota(ctx, pluginapi.QuotaFetchRequest{
				AuthIndex:   entry.AuthIndex,
				AuthID:      entry.ID,
				Provider:    quotaCodexProvider,
				StorageJSON: rawAuth,
				Metadata:    material.Metadata,
				Attributes:  material.Attributes,
				HTTPClient:  &quotaHTTPClient{host: r.host, callbackID: callbackID},
			}, callbackID)
			if errQuota != nil {
				prefixResult.Errors = append(prefixResult.Errors, "one Codex quota query failed")
				continue
			}
			prefixResult.Accounts = append(prefixResult.Accounts, quotaResourceAccount{Provider: quotaCodexProvider, Quota: quota})
		}
		if len(prefixResult.Errors) > 0 {
			result.Partial = true
		}
		if !actualSet && quotaPrefixHasRemaining(prefixResult) {
			result.ActualPrefix = prefixResult.ActualPrefix
			actualSet = true
		}
		result.Prefixes = append(result.Prefixes, prefixResult)
	}
	if len(result.Prefixes) == 0 {
		return quotaJSONResponse(http.StatusNotFound, map[string]string{"error": "no Codex credential matches the configured prefix"}), nil
	}
	if result.ActualPrefix == "" {
		result.ActualPrefix = result.NominalPrefix
	}
	if len(result.Prefixes) > 0 {
		matched := false
		for _, prefix := range result.Prefixes {
			if len(prefix.Accounts) > 0 {
				matched = true
				break
			}
		}
		if !matched {
			return quotaJSONResponse(http.StatusNotFound, result), nil
		}
	}
	return quotaJSONResponse(http.StatusOK, result), nil
}

func quotaPrefixHasRemaining(prefix quotaResourcePrefixResult) bool {
	for _, account := range prefix.Accounts {
		for _, group := range account.Quota.Groups {
			for _, bucket := range group.Buckets {
				if bucket.RemainingFraction > 0 {
					return true
				}
			}
		}
	}
	return false
}

func quotaJSONResponse(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError}
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}
}

func (r *Runtime) quotaFallbackPrefixes(nominal string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, 1)
	add := func(prefix string) {
		prefix = normalizePrefixName(prefix)
		if _, ok := seen[prefix]; ok {
			return
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	add(nominal)
	if r != nil && r.config.QuotaFallback.Enabled {
		for _, target := range r.config.QuotaFallback.Prefixes[normalizePrefixName(nominal)] {
			add(target)
		}
	}
	return result
}

func (r *Runtime) getAuthJSON(authIndex string) (json.RawMessage, error) {
	if strings.TrimSpace(authIndex) == "" {
		return nil, errors.New("auth index is empty")
	}
	raw, err := r.host.Call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var response hostAuthGetResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.JSON) == 0 {
		return nil, errors.New("auth JSON is empty")
	}
	return response.JSON, nil
}

type authMaterial struct {
	Metadata   map[string]any
	Attributes map[string]string
	Prefix     string
}

func decodeAuthMaterial(raw []byte) authMaterial {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return authMaterial{}
	}
	metadata := mapValue(root, "metadata")
	if metadata == nil {
		metadata = make(map[string]any)
	}
	// CPA auth files may store provider credentials directly at the root or
	// under metadata, depending on the auth parser that produced the file.
	for _, key := range []string{"access_token", "refresh_token", "id_token", "account_id", "base_url", "api_key", "type"} {
		if _, exists := metadata[key]; !exists {
			if value, exists := root[key]; exists {
				metadata[key] = value
			}
		}
	}
	material := authMaterial{Metadata: metadata, Attributes: stringMapValue(root, "attributes")}
	material.Prefix = firstString(root, "prefix", "metadata.prefix", "attributes.prefix")
	return material
}

func (m authMaterial) prefix() string {
	return normalizePrefixName(m.Prefix)
}

func firstString(root map[string]any, paths ...string) string {
	for _, path := range paths {
		var current any = root
		valid := true
		for _, part := range strings.Split(path, ".") {
			object, ok := current.(map[string]any)
			if !ok {
				valid = false
				break
			}
			current, ok = object[part]
			if !ok {
				valid = false
				break
			}
		}
		if valid {
			if value, ok := current.(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func mapValue(root map[string]any, key string) map[string]any {
	value, _ := root[key].(map[string]any)
	return value
}

func stringMapValue(root map[string]any, key string) map[string]string {
	value, _ := root[key].(map[string]any)
	if len(value) == 0 {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		if text, ok := item.(string); ok {
			result[key] = text
		}
	}
	return result
}

func fetchCodexQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.QuotaFetchResponse{}, errors.New("quota HTTP client is unavailable")
	}
	material := decodeAuthMaterial(req.StorageJSON)
	if material.Metadata == nil {
		material.Metadata = make(map[string]any)
	}
	for key, value := range req.Metadata {
		if _, exists := material.Metadata[key]; !exists {
			material.Metadata[key] = value
		}
	}
	if material.Attributes == nil {
		material.Attributes = make(map[string]string)
	}
	for key, value := range req.Attributes {
		if _, exists := material.Attributes[key]; !exists {
			material.Attributes[key] = value
		}
	}
	accessToken := firstStringFromMaps(material.Metadata, material.Attributes, "access_token", "api_key")
	if accessToken == "" {
		return pluginapi.QuotaFetchResponse{}, errors.New("Codex access token is unavailable")
	}
	baseURL := firstStringFromMaps(material.Metadata, material.Attributes, "base_url")
	usageURL, err := codexUsageURL(baseURL)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	headers := http.Header{
		"Authorization":   []string{"Bearer " + accessToken},
		"Accept":          []string{"application/json"},
		"OAI-Product-Sku": []string{"CODEX"},
	}
	accountID := firstStringFromMaps(material.Metadata, material.Attributes, "account_id")
	if accountID != "" {
		headers.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: usageURL, Headers: headers})
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("Codex quota endpoint returned HTTP %d", response.StatusCode)
	}
	return parseCodexQuotaResponse(response.Body)
}

func firstStringFromMaps(metadata map[string]any, attributes map[string]string, keys ...string) string {
	for _, key := range keys {
		if metadata != nil {
			if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		if attributes != nil {
			if value := strings.TrimSpace(attributes[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func codexUsageURL(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid Codex base URL")
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/codex") {
		path = strings.TrimSuffix(path, "/codex")
	}
	u.Path = path + "/wham/usage"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func parseCodexQuotaResponse(raw []byte) (pluginapi.QuotaFetchResponse, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode Codex quota response: %w", err)
	}
	rateLimit := nestedMap(root, "rate_limit", "rateLimit")
	if len(rateLimit) == 0 {
		return pluginapi.QuotaFetchResponse{}, errors.New("Codex quota response has no rate_limit")
	}
	response := pluginapi.QuotaFetchResponse{}
	plan := firstString(root, "plan_type", "planType")
	if plan != "" {
		response.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
	}
	group := pluginapi.QuotaGroup{DisplayName: "Codex"}
	for _, window := range []string{"primary", "secondary"} {
		windowMap := nestedMap(rateLimit, window+"_window", window+"Window")
		if len(windowMap) == 0 {
			continue
		}
		bucket, ok := codexQuotaBucket(window, windowMap)
		if ok {
			group.Buckets = append(group.Buckets, bucket)
		}
	}
	if len(group.Buckets) == 0 {
		return pluginapi.QuotaFetchResponse{}, errors.New("Codex quota response has no rate-limit windows")
	}
	response.Groups = []pluginapi.QuotaGroup{group}
	if credits := nestedMap(root, "credits"); len(credits) > 0 {
		if balance, ok := numberField(credits, "balance"); ok {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: "credits_balance", Label: "Credits balance", Value: balance, Format: "number"})
		}
	}
	return response, nil
}

func codexQuotaBucket(name string, window map[string]any) (pluginapi.QuotaBucket, bool) {
	used, hasUsed := numberField(window, "used_percent", "usedPercent")
	remaining, hasRemaining := numberField(window, "remaining_fraction", "remainingFraction")
	if !hasRemaining && hasUsed {
		remaining = 1 - used/100
		hasRemaining = true
	}
	if !hasRemaining {
		return pluginapi.QuotaBucket{}, false
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	reset := resetTimeString(window)
	description := ""
	if hasUsed {
		description = fmt.Sprintf("used %.2f%%", used)
	}
	return pluginapi.QuotaBucket{Window: name, RemainingFraction: remaining, ResetTime: reset, Description: description}, true
}

func nestedMap(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := root[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func numberField(root map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		switch number := value.(type) {
		case float64:
			return number, true
		case json.Number:
			parsed, err := number.Float64()
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func resetTimeString(window map[string]any) string {
	if value, ok := numberField(window, "reset_at", "resetAt"); ok && value > 0 {
		if value > 1_000_000_000_000 {
			value /= 1000
		}
		return time.Unix(int64(value), 0).UTC().Format(time.RFC3339)
	}
	for _, key := range []string{"reset_at", "resetAt"} {
		if value, ok := window[key].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				return parsed.UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}
