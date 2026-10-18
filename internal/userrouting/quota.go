package userrouting

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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
	quotaDirectResourcePath = "/quota/direct"
	quotaResetResourcePath  = "/quota/reset"
	quotaDirectResetPath    = "/quota/direct/reset"
	quotaCodexProvider      = "codex"
	quotaQueryRetryCount    = 3
	quotaQueryRetryDelay    = 100 * time.Millisecond
	quotaManagementTimeout  = 15 * time.Second
	quotaManagementBodyMax  = 2 << 20
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

type quotaHTTPDoer interface {
	Do(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
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

// DescribeQuota is exposed through quota.describe. The native QuotaProvider ABI
// advertises Codex only; the public resource can separately query Claude and
// Antigravity through CPA's authenticated Management API.
func (r *Runtime) DescribeQuota(context.Context) pluginapi.QuotaDescribeResponse {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled {
		return pluginapi.QuotaDescribeResponse{}
	}
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{quotaCodexProvider},
		DisplayName:        quotaProviderName,
		SupportsReset:      true,
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
	if strings.TrimSpace(req.AuthIndex) == "" || r.config.QuotaCacheTTL <= 0 {
		return fetchCodexQuota(ctx, req)
	}
	cacheKey, err := nativeQuotaCacheKey(req.AuthIndex, req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return fetchCodexQuota(ctx, req)
	}
	cached, err := r.quotaCache.get(ctx, cacheKey, r.config.QuotaCacheTTL, func() (quotaCacheValue, bool, error) {
		quota, fetchErr := fetchCodexQuota(ctx, req)
		return quotaCacheValue{native: quota}, fetchErr == nil, fetchErr
	})
	return cached.native, err
}

// ResetQuota consumes one Codex rate-limit reset credit through CPA's native
// QuotaProvider management flow. The host HTTP callback preserves CPA's
// outbound proxy and TLS policy.
func (r *Runtime) ResetQuota(ctx context.Context, req pluginapi.QuotaResetRequest, callbackID string) pluginapi.QuotaResetResponse {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled {
		return pluginapi.QuotaResetResponse{Message: "quota provider is disabled"}
	}
	if !strings.EqualFold(strings.TrimSpace(req.Provider), quotaCodexProvider) {
		return pluginapi.QuotaResetResponse{Message: "unsupported quota provider"}
	}
	if req.HTTPClient == nil {
		req.HTTPClient = &quotaHTTPClient{host: r.host, callbackID: callbackID}
	}
	accessToken, accountID, baseURL, err := codexRequestCredentials(req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return pluginapi.QuotaResetResponse{Message: "Codex credentials are unavailable"}
	}
	creditsURL, err := codexResetCreditsURL(baseURL)
	if err != nil {
		return pluginapi.QuotaResetResponse{Message: "invalid Codex base URL"}
	}
	headers := codexRequestHeaders(accessToken, accountID)
	creditsResponse, credits, err := readCodexResetCreditsWithRetry(ctx, req.HTTPClient, creditsURL, headers, nil)
	if err != nil {
		return pluginapi.QuotaResetResponse{Message: "unable to check Codex reset-credit availability"}
	}
	if credits.AvailableCount < 1 {
		return pluginapi.QuotaResetResponse{Message: "no Codex quota reset credits are available"}
	}
	creditID := firstAvailableCodexResetCreditID(creditsResponse.Body)
	if creditID == "" {
		return pluginapi.QuotaResetResponse{Message: "no selectable Codex quota reset credit details are available"}
	}
	requestID, err := newRedeemRequestID()
	if err != nil {
		return pluginapi.QuotaResetResponse{Message: "could not create a reset request identifier"}
	}
	consumeURL, err := codexResetConsumeURL(baseURL)
	if err != nil {
		return pluginapi.QuotaResetResponse{Message: "invalid Codex base URL"}
	}
	body, _ := json.Marshal(map[string]string{"redeem_request_id": requestID, "credit_id": creditID})
	consumeHeaders := codexRequestHeaders(accessToken, accountID)
	consumeHeaders.Set("Content-Type", "application/json")
	// The upstream may consume a credit even when the response is lost. Invalidate
	// before the one mutating request so subsequent reads cannot reuse old quota.
	r.quotaCache.invalidate(quotaCodexProvider, req.AuthIndex)
	consumeResponse, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     consumeURL,
		Headers: consumeHeaders,
		Body:    body,
	})
	if err != nil || consumeResponse.StatusCode < http.StatusOK || consumeResponse.StatusCode >= http.StatusMultipleChoices {
		return pluginapi.QuotaResetResponse{Message: "Codex reset-credit consumption failed; the final account state may be uncertain"}
	}
	var result struct {
		Code         string `json:"code"`
		WindowsReset int    `json:"windows_reset"`
	}
	if json.Unmarshal(consumeResponse.Body, &result) != nil || (result.Code != "reset" && result.WindowsReset < 1) {
		return pluginapi.QuotaResetResponse{Message: "Codex did not confirm reset-credit consumption"}
	}
	return pluginapi.QuotaResetResponse{Success: true, Message: "Codex quota reset credit consumed"}
}

type quotaResetCreditInfo struct {
	AvailableCount         int      `json:"available_count"`
	ExpiresAt              []string `json:"expires_at"`
	WithoutExpiry          int      `json:"without_expiry,omitempty"`
	ExpiryDetailsAvailable bool     `json:"expiry_details_available"`
	ExpiryDetailsComplete  bool     `json:"expiry_details_complete"`
}

type quotaResourceAccount struct {
	pluginapi.QuotaFetchResponse
	ResetCredits quotaResetCreditInfo `json:"reset_credits"`
}

func (a *quotaResourceAccount) UnmarshalJSON(raw []byte) error {
	var quota pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(raw, &quota); err != nil {
		return err
	}
	var extra struct {
		ResetCredits quotaResetCreditInfo `json:"reset_credits"`
	}
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	a.QuotaFetchResponse = quota
	a.ResetCredits = extra.ResetCredits
	return nil
}

type quotaResourceResponse struct {
	NominalPrefix   string                          `json:"nominal_prefix"`
	ActualPrefix    string                          `json:"actual_prefix,omitempty"`
	NominalAccounts map[string]quotaResourceAccount `json:"nominal_accounts,omitempty"`
	ActualAccounts  map[string]quotaResourceAccount `json:"actual_accounts,omitempty"`
	Errors          []string                        `json:"errors,omitempty"`
	Partial         bool                            `json:"partial"`
}

// An explicit provider/email query has no prefix selection, but uses the same
// account map and quota fields as the Codex resource response. SourceWindow
// preserves provider-specific names after normalizing the two main windows.
type providerQuotaResourceResponse struct {
	NominalAccounts map[string]providerQuotaAccount `json:"nominal_accounts"`
	Partial         bool                            `json:"partial"`
}

type providerQuotaAccount struct {
	Subscription       *pluginapi.QuotaSubscription `json:"subscription,omitempty"`
	Summary            []pluginapi.QuotaMetric      `json:"summary,omitempty"`
	ServerTimeOffsetMs int64                        `json:"serverTimeOffsetMs,omitempty"`
	Groups             []providerQuotaGroup         `json:"groups,omitempty"`
}

type providerQuotaGroup struct {
	DisplayName string                `json:"displayName,omitempty"`
	Buckets     []providerQuotaBucket `json:"buckets,omitempty"`
}

type providerQuotaBucket struct {
	pluginapi.QuotaBucket
	SourceWindow string `json:"sourceWindow,omitempty"`
}

func (b *providerQuotaBucket) UnmarshalJSON(raw []byte) error {
	var bucket pluginapi.QuotaBucket
	if err := json.Unmarshal(raw, &bucket); err != nil {
		return err
	}
	var extra struct {
		SourceWindow string `json:"sourceWindow"`
	}
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	b.QuotaBucket = bucket
	b.SourceWindow = extra.SourceWindow
	return nil
}

func normalizeProviderQuotaAccount(provider string, quota pluginapi.QuotaFetchResponse) providerQuotaAccount {
	account := providerQuotaAccount{
		Subscription:       quota.Subscription,
		Summary:            quota.Summary,
		ServerTimeOffsetMs: quota.ServerTimeOffsetMs,
		Groups:             make([]providerQuotaGroup, 0, len(quota.Groups)),
	}
	for _, group := range quota.Groups {
		normalized := providerQuotaGroup{
			DisplayName: group.DisplayName,
			Buckets:     make([]providerQuotaBucket, 0, len(group.Buckets)),
		}
		for _, bucket := range group.Buckets {
			originalWindow := bucket.Window
			bucket.Window = canonicalProviderQuotaWindow(provider, originalWindow)
			item := providerQuotaBucket{QuotaBucket: bucket}
			if bucket.Window != originalWindow {
				item.SourceWindow = originalWindow
			}
			normalized.Buckets = append(normalized.Buckets, item)
		}
		account.Groups = append(account.Groups, normalized)
	}
	return account
}

func canonicalProviderQuotaWindow(provider, window string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		switch strings.ToLower(strings.TrimSpace(window)) {
		case "five-hour", "five_hour", "5h":
			return "primary"
		case "seven-day", "seven_day", "weekly":
			return "secondary"
		}
	case "antigravity":
		switch strings.ToLower(strings.TrimSpace(window)) {
		case "5h", "five-hour", "five_hour":
			return "primary"
		case "weekly", "week", "seven-day", "seven_day":
			return "secondary"
		}
	}
	return window
}

// quotaDirectResourceResponse is returned by the token-forwarding resource.
// It intentionally has no prefix fields because the route performs no prefix
// lookup and accepts no downstream CPA API key.
type quotaDirectResourceResponse struct {
	Accounts map[string]quotaResourceAccount `json:"accounts,omitempty"`
	Errors   []string                        `json:"errors,omitempty"`
	Partial  bool                            `json:"partial"`
}

type quotaPrefixResult struct {
	Prefix   string
	Accounts map[string]quotaResourceAccount
	Errors   []string
}

type quotaResetResourceAccount struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

type quotaResetResourceResponse struct {
	NominalPrefix   string                               `json:"nominal_prefix"`
	NominalAccounts map[string]quotaResetResourceAccount `json:"nominal_accounts,omitempty"`
	Success         bool                                 `json:"success"`
	Partial         bool                                 `json:"partial"`
	Errors          []string                             `json:"errors,omitempty"`
}

type quotaDirectResetResourceResponse struct {
	Accounts map[string]quotaResetResourceAccount `json:"accounts,omitempty"`
	Success  bool                                 `json:"success"`
	Partial  bool                                 `json:"partial"`
	Errors   []string                             `json:"errors,omitempty"`
}

func (r *Runtime) RegisterManagement() pluginapi.ManagementRegistrationResponse {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled || !r.config.QuotaProvider.PublicEndpoint {
		return pluginapi.ManagementRegistrationResponse{}
	}
	return pluginapi.ManagementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        quotaResourcePath,
				Description: "Query Codex quota by downstream CPA API key, or query a CPA credential by provider and email.",
			},
			{
				Path:        quotaDirectResourcePath,
				Description: "Query Codex quota by forwarding a caller-supplied Codex access token.",
			},
			{
				Path:        quotaResetResourcePath,
				Description: "Synchronously consume one available Codex quota reset credit for every account under a downstream key's nominal prefix.",
			},
			{
				Path:        quotaDirectResetPath,
				Description: "Synchronously consume one available Codex quota reset credit using a caller-supplied Codex access token.",
			},
		},
	}
}

// HandleManagement handles the public ResourceRoute. The normal quota and
// reset routes require a downstream CPA API key; the direct route accepts a
// Codex access token and deliberately bypasses CPA key and prefix lookup.
func (r *Runtime) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest, callbackID string) (pluginapi.ManagementResponse, error) {
	if r == nil || !r.config.Enabled || !r.config.QuotaProvider.Enabled || !r.config.QuotaProvider.PublicEndpoint {
		return quotaJSONResponse(http.StatusNotFound, map[string]string{"error": "quota resource is disabled"}), nil
	}
	if req.Method != "" && !strings.EqualFold(req.Method, http.MethodGet) {
		return quotaJSONResponse(http.StatusMethodNotAllowed, map[string]string{"error": "GET is required"}), nil
	}
	if isQuotaDirectResetResourcePath(req.Path) {
		return r.handleQuotaDirectResetResource(ctx, req.Headers, callbackID), nil
	}
	if isQuotaDirectResourcePath(req.Path) {
		return r.handleQuotaDirectResource(ctx, req.Headers, callbackID), nil
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
	provider := strings.TrimSpace(req.Query.Get("provider"))
	email := strings.TrimSpace(req.Query.Get("email"))
	if provider != "" || email != "" {
		if isQuotaResetResourcePath(req.Path) {
			return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "provider and email are supported only by the quota query route"}), nil
		}
		if provider == "" || email == "" {
			return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "provider and email must be provided together"}), nil
		}
		return r.handleQuotaByProviderEmail(ctx, provider, email, snapshot), nil
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
	if isQuotaResetResourcePath(req.Path) {
		return r.handleQuotaResetResource(ctx, nominalName, listed.Files, callbackID), nil
	}

	nominalPrefix = normalizePrefix(nominalName)
	prefixResults := make([]quotaPrefixResult, 0, len(candidates))
	actualPrefix := nominalPrefix
	for _, candidate := range candidates {
		prefixResult := quotaPrefixResult{
			Prefix:   normalizePrefix(candidate),
			Accounts: make(map[string]quotaResourceAccount),
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
			account := strings.TrimSpace(entry.Email)
			if account == "" {
				account = material.email()
			}
			if account == "" {
				prefixResult.Errors = append(prefixResult.Errors, "one Codex credential has no email address")
				continue
			}
			fetchRequest := pluginapi.QuotaFetchRequest{
				AuthIndex:   entry.AuthIndex,
				AuthID:      entry.ID,
				Provider:    quotaCodexProvider,
				StorageJSON: rawAuth,
				Metadata:    material.Metadata,
				Attributes:  material.Attributes,
				HTTPClient:  &quotaHTTPClient{host: r.host, callbackID: callbackID},
			}
			cacheKey := newQuotaCacheKey(quotaCacheCodexResource, quotaCodexProvider, entry.AuthIndex, rawAuth, nil)
			cached, errQuota := r.quotaCache.get(ctx, cacheKey, r.config.QuotaCacheTTL, func() (quotaCacheValue, bool, error) {
				quota, err := r.FetchQuota(ctx, fetchRequest, callbackID)
				if err != nil {
					return quotaCacheValue{}, false, err
				}
				resetCredits, errCredits := fetchCodexResetCreditInfo(ctx, fetchRequest, quota)
				return quotaCacheValue{
					account:           quotaResourceAccount{QuotaFetchResponse: quota, ResetCredits: resetCredits},
					creditsIncomplete: errCredits != nil,
				}, errCredits == nil, nil
			})
			if errQuota != nil {
				prefixResult.Errors = append(prefixResult.Errors, "one Codex quota query failed")
				continue
			}
			if cached.creditsIncomplete {
				prefixResult.Errors = append(prefixResult.Errors, "one Codex reset-credit expiry query failed")
			}
			prefixResult.Accounts[account] = cached.account
		}
		selected := quotaPrefixHasRemaining(prefixResult)
		if selected {
			actualPrefix = prefixResult.Prefix
		}
		prefixResults = append(prefixResults, prefixResult)
		if selected {
			break
		}
	}
	if len(prefixResults) == 0 {
		return quotaJSONResponse(http.StatusNotFound, map[string]string{"error": "no Codex credential matches the configured prefix"}), nil
	}
	result := quotaResourceResponse{
		NominalPrefix:   nominalPrefix,
		ActualPrefix:    actualPrefix,
		NominalAccounts: make(map[string]quotaResourceAccount),
	}
	if actualPrefix != nominalPrefix {
		result.ActualAccounts = make(map[string]quotaResourceAccount)
	}
	matched := false
	for _, prefix := range prefixResults {
		if prefix.Prefix != nominalPrefix && prefix.Prefix != actualPrefix {
			continue
		}
		if len(prefix.Errors) > 0 {
			result.Partial = true
			result.Errors = append(result.Errors, prefix.Errors...)
		}
		if len(prefix.Accounts) > 0 {
			matched = true
		}
		if prefix.Prefix == nominalPrefix {
			for account, quota := range prefix.Accounts {
				result.NominalAccounts[account] = quota
			}
		}
		if actualPrefix != nominalPrefix && prefix.Prefix == actualPrefix {
			for account, quota := range prefix.Accounts {
				result.ActualAccounts[account] = quota
			}
		}
	}
	if !matched {
		return quotaJSONResponse(http.StatusNotFound, result), nil
	}
	return quotaJSONResponse(http.StatusOK, result), nil
}

// handleQuotaByProviderEmail resolves a credential through CPA's host auth ABI,
// then queries provider quota through CPA's authenticated API-call endpoint for
// providers implemented by Management Center, or CPA's native quota endpoint
// for other providers. The management key is intentionally read from CPA's
// process environment, never plugin config or the caller request. Unlike the
// default Codex quota query, this explicit provider/email lookup is not scoped
// to the downstream API key's model prefix.
func (r *Runtime) handleQuotaByProviderEmail(ctx context.Context, provider, email string, snapshot CPAConfigSnapshot) pluginapi.ManagementResponse {
	listRaw, err := r.host.Call(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA auth list is unavailable"})
	}
	var listed hostAuthListResponse
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "invalid CPA auth list response"})
	}

	type authMatch struct {
		index     string
		provider  string
		account   string
		projectID string
		cacheKey  quotaCacheKey
	}
	var match *authMatch
	for _, entry := range listed.Files {
		entryProvider := strings.TrimSpace(entry.Provider)
		if entryProvider == "" {
			entryProvider = strings.TrimSpace(entry.Type)
		}
		if entry.Disabled || entry.Unavailable || !strings.EqualFold(entryProvider, provider) || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		if entry.Email != "" && !strings.EqualFold(strings.TrimSpace(entry.Email), email) {
			continue
		}

		rawAuth, errGet := r.getAuthJSON(entry.AuthIndex)
		if errGet != nil {
			return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA credential could not be read"})
		}
		material := decodeAuthMaterial(rawAuth)
		accountEmail := strings.TrimSpace(entry.Email)
		if accountEmail == "" {
			accountEmail = material.email()
		}
		if !strings.EqualFold(accountEmail, email) {
			continue
		}
		if match != nil {
			return quotaJSONResponse(http.StatusConflict, map[string]string{"error": "provider and email match multiple credentials"})
		}
		projectID := antigravityProjectID(entry, rawAuth)
		match = &authMatch{
			index:     entry.AuthIndex,
			provider:  entryProvider,
			account:   accountEmail,
			projectID: projectID,
			cacheKey:  newQuotaCacheKey(quotaCacheProviderQuery, entryProvider, entry.AuthIndex, rawAuth, []byte(projectID)),
		}
	}
	if match == nil {
		return quotaJSONResponse(http.StatusNotFound, map[string]string{"error": "no available credential matches the requested provider and email"})
	}

	managementKey := strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD"))
	if managementKey == "" {
		return quotaJSONResponse(http.StatusServiceUnavailable, map[string]string{"error": "provider/email quota lookup requires MANAGEMENT_PASSWORD in the CPA process environment"})
	}

	cacheTTL := r.config.QuotaCacheTTL
	switch strings.ToLower(strings.TrimSpace(match.provider)) {
	case quotaCodexProvider, "claude", "antigravity":
	default:
		cacheTTL = 0
	}
	cached, errQuery := r.quotaCache.get(ctx, match.cacheKey, cacheTTL, func() (quotaCacheValue, bool, error) {
		var query cpaQuotaQueryResult
		var err error
		switch strings.ToLower(strings.TrimSpace(match.provider)) {
		case "antigravity", "claude":
			query, err = queryManagementCenterQuota(ctx, snapshot, managementKey, match.index, match.provider, match.projectID)
		default:
			query, err = queryCPAManagementQuota(ctx, snapshot, managementKey, match.index, match.provider)
		}
		return quotaCacheValue{query: query}, query.status >= http.StatusOK && query.status < http.StatusMultipleChoices, err
	})
	if errQuery != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA quota query failed"})
	}
	query := cached.query
	if query.status < http.StatusOK || query.status >= http.StatusMultipleChoices {
		if query.status == http.StatusNotImplemented {
			return quotaJSONResponse(http.StatusNotImplemented, map[string]string{"error": "CPA has no quota provider for this credential"})
		}
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA quota query failed", "status": strconv.Itoa(query.status)})
	}
	return quotaJSONResponse(http.StatusOK, providerQuotaResourceResponse{
		NominalAccounts: map[string]providerQuotaAccount{
			match.account: normalizeProviderQuotaAccount(match.provider, query.quota),
		},
	})
}

type cpaQuotaQueryResult struct {
	quota  pluginapi.QuotaFetchResponse
	status int
}

func queryCPAManagementQuota(ctx context.Context, snapshot CPAConfigSnapshot, managementKey, authIndex, provider string) (cpaQuotaQueryResult, error) {
	return retryQuotaRead(ctx, func() (cpaQuotaQueryResult, error) {
		endpoint, errURL := deriveManagementQuotaURL(snapshot)
		if errURL != nil {
			return cpaQuotaQueryResult{}, errURL
		}
		body, errMarshal := json.Marshal(map[string]string{"auth_index": authIndex, "provider": provider})
		if errMarshal != nil {
			return cpaQuotaQueryResult{}, errMarshal
		}
		requestCtx, cancel := context.WithTimeout(ctx, quotaManagementTimeout)
		defer cancel()
		request, errRequest := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
		if errRequest != nil {
			return cpaQuotaQueryResult{}, errRequest
		}
		request.Header.Set("Authorization", "Bearer "+managementKey)
		request.Header.Set("Content-Type", "application/json")

		// Keep the configured host for HTTP Host/TLS verification, but force the
		// TCP connection to loopback. This prevents a CPA config hostname from
		// sending the Management Key to a remote host or an outbound proxy.
		endpointURL, errParseURL := url.Parse(endpoint)
		if errParseURL != nil {
			return cpaQuotaQueryResult{}, errParseURL
		}
		expectedHost, expectedPort, errSplit := net.SplitHostPort(endpointURL.Host)
		if errSplit != nil {
			return cpaQuotaQueryResult{}, errSplit
		}
		loopbackHost := "127.0.0.1"
		if ip := net.ParseIP(expectedHost); ip != nil && ip.IsLoopback() && strings.Contains(expectedHost, ":") {
			loopbackHost = "::1"
		}
		dialer := &net.Dialer{Timeout: quotaManagementTimeout}
		transport := &http.Transport{
			Proxy: nil,
			DialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil || !strings.EqualFold(host, expectedHost) || port != expectedPort {
					return nil, errors.New("refusing non-local CPA Management API redirect")
				}
				return dialer.DialContext(dialCtx, network, net.JoinHostPort(loopbackHost, expectedPort))
			},
		}
		client := &http.Client{
			Timeout:   quotaManagementTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		defer transport.CloseIdleConnections()
		response, errDo := client.Do(request)
		if errDo != nil {
			return cpaQuotaQueryResult{}, errDo
		}
		defer response.Body.Close()
		responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, quotaManagementBodyMax+1))
		if errRead != nil {
			return cpaQuotaQueryResult{}, errRead
		}
		if len(responseBody) > quotaManagementBodyMax {
			return cpaQuotaQueryResult{}, errors.New("CPA quota response exceeded size limit")
		}
		result := cpaQuotaQueryResult{status: response.StatusCode}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
			return result, fmt.Errorf("CPA quota endpoint returned HTTP %d", response.StatusCode)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return result, nil
		}
		if err := json.Unmarshal(responseBody, &result.quota); err != nil {
			return cpaQuotaQueryResult{}, fmt.Errorf("decode CPA quota response: %w", err)
		}
		return result, nil
	})
}

func deriveManagementQuotaURL(snapshot CPAConfigSnapshot) (string, error) {
	modelsURL, err := deriveModelsURL(snapshot)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(modelsURL)
	if err != nil {
		return "", err
	}
	endpoint.Path = "/v0/management/quota/fetch"
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String(), nil
}

func isQuotaDirectResourcePath(path string) bool {
	path = "/" + strings.Trim(strings.TrimSpace(path), "/")
	return path == quotaDirectResourcePath || strings.HasSuffix(path, "/plugins/user-routing"+quotaDirectResourcePath)
}

func isQuotaDirectResetResourcePath(path string) bool {
	path = "/" + strings.Trim(strings.TrimSpace(path), "/")
	return path == quotaDirectResetPath || strings.HasSuffix(path, "/plugins/user-routing"+quotaDirectResetPath)
}

// handleQuotaDirectResetResource consumes one reset credit for the caller's
// Codex account. The request uses the same access token and account ID inputs
// as /quota/direct and does not require a CPA API key or auth-file lookup.
func (r *Runtime) handleQuotaDirectResetResource(ctx context.Context, headers http.Header, callbackID string) pluginapi.ManagementResponse {
	accessToken := extractBearerToken(headers.Get("Authorization"))
	if accessToken == "" {
		accessToken = extractBearerToken(headers.Get("X-Codex-Access-Token"))
	}
	if accessToken == "" {
		return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "a Codex access token is required"})
	}
	email, accountIDFromToken := codexTokenDisplayClaims(accessToken)
	accountID := strings.TrimSpace(headers.Get("ChatGPT-Account-ID"))
	if accountID == "" {
		accountID = strings.TrimSpace(headers.Get("X-Codex-Account-ID"))
	}
	if accountID == "" {
		accountID = accountIDFromToken
	}
	if accountID == "" {
		return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "a Codex account ID is required"})
	}
	storageJSON, err := json.Marshal(map[string]string{"access_token": accessToken, "account_id": accountID})
	if err != nil {
		return quotaJSONResponse(http.StatusInternalServerError, map[string]string{"error": "could not prepare Codex credentials"})
	}
	reset := r.ResetQuota(ctx, pluginapi.QuotaResetRequest{
		Provider:    quotaCodexProvider,
		StorageJSON: storageJSON,
		HTTPClient:  &quotaHTTPClient{host: r.host, callbackID: callbackID},
	}, callbackID)
	account := email
	if account == "" {
		account = accountID
	}
	result := quotaDirectResetResourceResponse{
		Accounts: map[string]quotaResetResourceAccount{
			account: {Success: reset.Success, Message: reset.Message},
		},
		Success: reset.Success,
	}
	return quotaJSONResponse(http.StatusOK, result)
}

func (r *Runtime) handleQuotaDirectResource(ctx context.Context, headers http.Header, callbackID string) pluginapi.ManagementResponse {
	accessToken := extractBearerToken(headers.Get("Authorization"))
	if accessToken == "" {
		accessToken = extractBearerToken(headers.Get("X-Codex-Access-Token"))
	}
	if accessToken == "" {
		return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "a Codex access token is required"})
	}

	emailFromToken, accountIDFromToken := codexTokenDisplayClaims(accessToken)
	accountID := strings.TrimSpace(headers.Get("ChatGPT-Account-ID"))
	if accountID == "" {
		accountID = strings.TrimSpace(headers.Get("X-Codex-Account-ID"))
	}
	if accountID == "" {
		accountID = accountIDFromToken
	}
	if accountID == "" {
		return quotaJSONResponse(http.StatusBadRequest, map[string]string{"error": "a Codex account ID is required"})
	}

	storageJSON, err := json.Marshal(map[string]string{"access_token": accessToken, "account_id": accountID})
	if err != nil {
		return quotaJSONResponse(http.StatusInternalServerError, map[string]string{"error": "could not prepare Codex credentials"})
	}
	quotaRequest := pluginapi.QuotaFetchRequest{
		Provider:    quotaCodexProvider,
		StorageJSON: storageJSON,
		HTTPClient:  &quotaHTTPClient{host: r.host, callbackID: callbackID},
	}
	quota, err := r.FetchQuota(ctx, quotaRequest, callbackID)
	if err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "Codex quota query failed after 3 retries"})
	}
	resetCredits, err := fetchCodexResetCreditInfo(ctx, quotaRequest, quota)
	result := quotaDirectResourceResponse{Accounts: make(map[string]quotaResourceAccount, 1)}
	if err != nil {
		result.Partial = true
		result.Errors = append(result.Errors, "Codex reset-credit details query failed after 3 retries")
		resetCredits = quotaResetCreditInfoFromQuota(quota)
	}
	account := emailFromToken
	if account == "" {
		account = accountID
	}
	result.Accounts[account] = quotaResourceAccount{QuotaFetchResponse: quota, ResetCredits: resetCredits}
	return quotaJSONResponse(http.StatusOK, result)
}

// codexTokenDisplayClaims decodes claims only to label the returned account.
// It does not authorize the request; the upstream quota API validates the
// supplied bearer token when the query is made.
func codexTokenDisplayClaims(accessToken string) (email, accountID string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Profile map[string]any `json:"https://api.openai.com/profile"`
		Auth    map[string]any `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	email, _ = claims.Profile["email"].(string)
	accountID, _ = claims.Auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(email), strings.TrimSpace(accountID)
}

func isQuotaResetResourcePath(path string) bool {
	path = "/" + strings.Trim(strings.TrimSpace(path), "/")
	return path == quotaResetResourcePath || strings.HasSuffix(path, "/plugins/user-routing"+quotaResetResourcePath)
}

// handleQuotaResetResource runs synchronously and only targets Codex auth files
// whose nominal prefix matches the downstream API key. The read-only quota
// checks may retry; each mutating reset-consumption request is attempted once.
func (r *Runtime) handleQuotaResetResource(ctx context.Context, nominalName string, files []pluginapi.HostAuthFileEntry, callbackID string) pluginapi.ManagementResponse {
	nominalPrefix := normalizePrefix(nominalName)
	result := quotaResetResourceResponse{
		NominalPrefix:   nominalPrefix,
		NominalAccounts: make(map[string]quotaResetResourceAccount),
	}
	matched := 0
	succeeded := 0
	failed := 0
	unresolved := 0
	for _, entry := range files {
		if entry.Disabled || entry.Unavailable || !strings.EqualFold(strings.TrimSpace(entry.Provider), quotaCodexProvider) {
			continue
		}
		rawAuth, err := r.getAuthJSON(entry.AuthIndex)
		if err != nil {
			// Without the auth payload the credential's prefix cannot be safely
			// identified, so do not report or operate on an unrelated account.
			unresolved++
			continue
		}
		material := decodeAuthMaterial(rawAuth)
		if material.prefix() != normalizePrefixName(nominalName) {
			continue
		}
		matched++
		account := strings.TrimSpace(entry.Email)
		if account == "" {
			account = material.email()
		}
		if account == "" {
			failed++
			result.Errors = append(result.Errors, "one matching Codex credential has no email address")
			continue
		}
		accountKey := account
		if _, exists := result.NominalAccounts[accountKey]; exists {
			accountKey = fmt.Sprintf("%s#%d", account, matched)
			result.Errors = append(result.Errors, "multiple matching Codex credentials share an email address")
		}
		reset := r.ResetQuota(ctx, pluginapi.QuotaResetRequest{
			AuthIndex:   entry.AuthIndex,
			AuthID:      entry.ID,
			Provider:    quotaCodexProvider,
			StorageJSON: rawAuth,
			Metadata:    material.Metadata,
			Attributes:  material.Attributes,
			HTTPClient:  &quotaHTTPClient{host: r.host, callbackID: callbackID},
		}, callbackID)
		result.NominalAccounts[accountKey] = quotaResetResourceAccount{Success: reset.Success, Message: reset.Message}
		if reset.Success {
			succeeded++
		} else {
			failed++
		}
	}
	if matched == 0 {
		if unresolved > 0 {
			return quotaJSONResponse(http.StatusBadGateway, map[string]any{
				"nominal_prefix": nominalPrefix,
				"success":        false,
				"error":          "unable to inspect all Codex credentials to resolve the nominal prefix",
			})
		}
		return quotaJSONResponse(http.StatusNotFound, map[string]any{
			"nominal_prefix": nominalPrefix,
			"success":        false,
			"error":          "no available Codex credential matches the downstream key's nominal prefix",
		})
	}
	if unresolved > 0 {
		result.Errors = append(result.Errors, "one or more Codex credentials could not be inspected for prefix matching")
	}
	result.Success = failed == 0 && unresolved == 0 && len(result.NominalAccounts) == matched
	result.Partial = succeeded > 0 && (failed > 0 || unresolved > 0)
	return quotaJSONResponse(http.StatusOK, result)
}

func quotaPrefixHasRemaining(prefix quotaPrefixResult) bool {
	for _, account := range prefix.Accounts {
		hasWindow := false
		allWindowsHaveRemaining := true
		for _, group := range account.QuotaFetchResponse.Groups {
			for _, bucket := range group.Buckets {
				hasWindow = true
				if bucket.RemainingFraction <= 0 {
					allWindowsHaveRemaining = false
					break
				}
			}
			if !allWindowsHaveRemaining {
				break
			}
		}
		if hasWindow && allWindowsHaveRemaining {
			return true
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
	Email      string
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
	material := authMaterial{
		Metadata:   metadata,
		Attributes: stringMapValue(root, "attributes"),
		Email:      firstString(root, "email", "metadata.email", "attributes.email"),
	}
	material.Prefix = firstString(root, "prefix", "metadata.prefix", "attributes.prefix")
	return material
}

func (m authMaterial) prefix() string {
	return normalizePrefixName(m.Prefix)
}

func (m authMaterial) email() string {
	return strings.TrimSpace(m.Email)
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
	return retryQuotaRead(ctx, func() (pluginapi.QuotaFetchResponse, error) {
		return fetchCodexQuotaOnce(ctx, req)
	})
}

func fetchCodexQuotaOnce(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.HTTPClient == nil {
		return pluginapi.QuotaFetchResponse{}, errors.New("quota HTTP client is unavailable")
	}
	accessToken, accountID, baseURL, err := codexRequestCredentials(req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	usageURL, err := codexUsageURL(baseURL)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	response, err := req.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: usageURL, Headers: codexRequestHeaders(accessToken, accountID)})
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("Codex quota endpoint returned HTTP %d", response.StatusCode)
	}
	return parseCodexQuotaResponse(response.Body)
}

func retryQuotaRead[T any](ctx context.Context, query func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= quotaQueryRetryCount; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		result, err := query()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt == quotaQueryRetryCount {
			break
		}
		timer := time.NewTimer(quotaQueryRetryDelay * time.Duration(1<<attempt))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, lastErr
}

func codexRequestCredentials(storageJSON []byte, metadata map[string]any, attributes map[string]string) (accessToken, accountID, baseURL string, err error) {
	material := decodeAuthMaterial(storageJSON)
	if material.Metadata == nil {
		material.Metadata = make(map[string]any)
	}
	for key, value := range metadata {
		if _, exists := material.Metadata[key]; !exists {
			material.Metadata[key] = value
		}
	}
	if material.Attributes == nil {
		material.Attributes = make(map[string]string)
	}
	for key, value := range attributes {
		if _, exists := material.Attributes[key]; !exists {
			material.Attributes[key] = value
		}
	}
	accessToken = firstStringFromMaps(material.Metadata, material.Attributes, "access_token", "api_key")
	if accessToken == "" {
		return "", "", "", errors.New("Codex access token is unavailable")
	}
	accountID = firstStringFromMaps(material.Metadata, material.Attributes, "account_id")
	baseURL = firstStringFromMaps(material.Metadata, material.Attributes, "base_url")
	return accessToken, accountID, baseURL, nil
}

func codexRequestHeaders(accessToken, accountID string) http.Header {
	headers := http.Header{
		"Authorization":   []string{"Bearer " + accessToken},
		"Accept":          []string{"application/json"},
		"OAI-Product-Sku": []string{"CODEX"},
	}
	if accountID != "" {
		headers.Set("ChatGPT-Account-ID", accountID)
	}
	return headers
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
	return codexWHAMURL(baseURL, "usage")
}

func codexResetCreditsURL(baseURL string) (string, error) {
	return codexWHAMURL(baseURL, "rate-limit-reset-credits")
}

func codexResetConsumeURL(baseURL string) (string, error) {
	return codexWHAMURL(baseURL, "rate-limit-reset-credits/consume")
}

func codexWHAMURL(baseURL, endpoint string) (string, error) {
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
	u.Path = path + "/wham/" + strings.TrimLeft(endpoint, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

type codexResetCredit struct {
	ID        string          `json:"id"`
	ResetType string          `json:"reset_type"`
	Status    string          `json:"status"`
	ExpiresAt json.RawMessage `json:"expires_at"`
}

type codexResetCreditsResponse struct {
	AvailableCount *int               `json:"available_count"`
	Credits        []codexResetCredit `json:"credits"`
}

func fetchCodexResetCreditInfo(ctx context.Context, req pluginapi.QuotaFetchRequest, quota pluginapi.QuotaFetchResponse) (quotaResetCreditInfo, error) {
	fallback := quotaResetCreditInfoFromQuota(quota)
	if req.HTTPClient == nil {
		return fallback, errors.New("quota HTTP client is unavailable")
	}
	accessToken, accountID, baseURL, err := codexRequestCredentials(req.StorageJSON, req.Metadata, req.Attributes)
	if err != nil {
		return fallback, err
	}
	creditsURL, err := codexResetCreditsURL(baseURL)
	if err != nil {
		return fallback, err
	}
	_, info, err := readCodexResetCreditsWithRetry(ctx, req.HTTPClient, creditsURL, codexRequestHeaders(accessToken, accountID), quotaResetCreditsCount(quota))
	if err != nil {
		return fallback, err
	}
	return info, nil
}

func readCodexResetCreditsWithRetry(ctx context.Context, client quotaHTTPDoer, creditsURL string, headers http.Header, fallbackCount *int) (pluginapi.HTTPResponse, quotaResetCreditInfo, error) {
	type readResult struct {
		response pluginapi.HTTPResponse
		credits  quotaResetCreditInfo
	}
	result, err := retryQuotaRead(ctx, func() (readResult, error) {
		if client == nil {
			return readResult{}, errors.New("quota HTTP client is unavailable")
		}
		response, err := client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: creditsURL, Headers: headers})
		if err != nil {
			return readResult{}, err
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return readResult{}, fmt.Errorf("Codex reset-credit endpoint returned HTTP %d", response.StatusCode)
		}
		credits, err := parseCodexResetCredits(response.Body, fallbackCount)
		if err != nil {
			return readResult{}, err
		}
		return readResult{response: response, credits: credits}, nil
	})
	if err != nil {
		return pluginapi.HTTPResponse{}, quotaResetCreditInfo{}, err
	}
	return result.response, result.credits, nil
}

func parseCodexResetCredits(raw []byte, fallbackCount *int) (quotaResetCreditInfo, error) {
	var payload codexResetCreditsResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return quotaResetCreditInfo{}, fmt.Errorf("decode Codex reset-credit response: %w", err)
	}
	info := quotaResetCreditInfo{ExpiryDetailsAvailable: payload.Credits != nil, ExpiresAt: make([]string, 0)}
	availableRows := 0
	allAvailableRowsHaveExpiry := true
	for _, credit := range payload.Credits {
		if !strings.EqualFold(strings.TrimSpace(credit.ResetType), "codex_rate_limits") || !strings.EqualFold(strings.TrimSpace(credit.Status), "available") {
			continue
		}
		availableRows++
		if len(credit.ExpiresAt) == 0 {
			allAvailableRowsHaveExpiry = false
			continue
		}
		if string(credit.ExpiresAt) == "null" {
			info.WithoutExpiry++
			continue
		}
		if expiry, ok := normalizeCodexExpiration(credit.ExpiresAt); ok {
			info.ExpiresAt = append(info.ExpiresAt, expiry)
		} else {
			allAvailableRowsHaveExpiry = false
		}
	}
	switch {
	case payload.AvailableCount != nil:
		info.AvailableCount = *payload.AvailableCount
	case fallbackCount != nil:
		info.AvailableCount = *fallbackCount
	default:
		info.AvailableCount = availableRows
	}
	info.ExpiryDetailsComplete = info.ExpiryDetailsAvailable && allAvailableRowsHaveExpiry && availableRows >= info.AvailableCount
	return info, nil
}

func firstAvailableCodexResetCreditID(raw []byte) string {
	var payload codexResetCreditsResponse
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	for _, credit := range payload.Credits {
		if strings.EqualFold(strings.TrimSpace(credit.ResetType), "codex_rate_limits") &&
			strings.EqualFold(strings.TrimSpace(credit.Status), "available") && strings.TrimSpace(credit.ID) != "" {
			return strings.TrimSpace(credit.ID)
		}
	}
	return ""
}

func normalizeCodexExpiration(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil {
			return parsed.UTC().Format(time.RFC3339), true
		}
		return strings.TrimSpace(text), strings.TrimSpace(text) != ""
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		seconds, ok := 0.0, false
		switch number := value.(type) {
		case float64:
			seconds, ok = number, true
		case json.Number:
			parsed, parseErr := number.Float64()
			seconds, ok = parsed, parseErr == nil
		case string:
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(number), 64)
			seconds, ok = parsed, parseErr == nil
		}
		if ok && seconds > 0 {
			if seconds > 1_000_000_000_000 {
				seconds /= 1000
			}
			return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339), true
		}
	}
	return "", false
}

func quotaResetCreditInfoFromQuota(quota pluginapi.QuotaFetchResponse) quotaResetCreditInfo {
	info := quotaResetCreditInfo{ExpiresAt: make([]string, 0)}
	if count := quotaResetCreditsCount(quota); count != nil {
		info.AvailableCount = *count
	}
	return info
}

func quotaResetCreditsCount(quota pluginapi.QuotaFetchResponse) *int {
	for _, metric := range quota.Summary {
		if metric.Key == "rate_limit_reset_credits_available" {
			count := int(metric.Value)
			return &count
		}
	}
	return nil
}

func newRedeemRequestID() (string, error) {
	var id [16]byte
	if _, err := cryptorand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
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
	if credits := nestedMap(root, "rate_limit_reset_credits"); len(credits) > 0 {
		if count, ok := numberField(credits, "available_count", "availableCount"); ok {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{
				Key: "rate_limit_reset_credits_available", Label: "Quota reset credits available", Value: count, Format: "number",
			})
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
