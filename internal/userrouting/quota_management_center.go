package userrouting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	managementAPICallPath        = "/v8/management/requests/api-call"
	claudeUsageURL               = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL             = "https://api.anthropic.com/api/oauth/profile"
	antigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	antigravityHubUserAgent      = "antigravity/hub/2.23.0 darwin/arm64"
)

var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

type managementAPICallRequest struct {
	AuthIndex string            `json:"authIndex"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header"`
	Data      string            `json:"data,omitempty"`
}

type managementAPICallEnvelope struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

type managementUpstreamResponse struct {
	StatusCode int
	Header     map[string][]string
	Body       []byte
}

// queryManagementCenterQuota mirrors the Management Center's provider-specific
// calls through CPA's authenticated api-call endpoint. Management Center keeps
// this API key server-side and CPA resolves $TOKEN$ using authIndex (including
// Antigravity token refresh and the credential's configured proxy).
func queryManagementCenterQuota(ctx context.Context, snapshot CPAConfigSnapshot, managementKey, authIndex, provider, projectID string) (cpaQuotaQueryResult, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return queryClaudeQuotaViaManagementCenter(ctx, snapshot, managementKey, authIndex)
	case "antigravity":
		return queryAntigravityQuotaViaManagementCenter(ctx, snapshot, managementKey, authIndex, projectID)
	default:
		return cpaQuotaQueryResult{}, fmt.Errorf("unsupported Management Center quota provider %q", provider)
	}
}

func queryClaudeQuotaViaManagementCenter(ctx context.Context, snapshot CPAConfigSnapshot, managementKey, authIndex string) (cpaQuotaQueryResult, error) {
	headers := map[string]string{
		"User-Agent":     "claude-cli/2.1.280 (external, cli)",
		"Authorization":  "Bearer $TOKEN$",
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
		"Accept":         "application/json",
	}
	usage, err := callManagementAPICall(ctx, snapshot, managementKey, managementAPICallRequest{
		AuthIndex: authIndex,
		Method:    http.MethodGet,
		URL:       claudeUsageURL,
		Header:    headers,
	})
	if err != nil {
		return cpaQuotaQueryResult{}, err
	}
	if usage.StatusCode < http.StatusOK || usage.StatusCode >= http.StatusMultipleChoices {
		return cpaQuotaQueryResult{status: usage.StatusCode}, nil
	}
	quota, err := parseClaudeManagementQuota(usage.Body, nil)
	if err != nil {
		return cpaQuotaQueryResult{}, err
	}

	// The Management Center treats the profile request as optional: quota data
	// remains useful if profile/plan lookup is unavailable.
	profile, errProfile := callManagementAPICall(ctx, snapshot, managementKey, managementAPICallRequest{
		AuthIndex: authIndex,
		Method:    http.MethodGet,
		URL:       claudeProfileURL,
		Header:    headers,
	})
	if errProfile == nil && profile.StatusCode >= http.StatusOK && profile.StatusCode < http.StatusMultipleChoices {
		quota.Subscription = parseClaudeSubscription(profile.Body)
	}
	return cpaQuotaQueryResult{quota: quota, status: http.StatusOK}, nil
}

func queryAntigravityQuotaViaManagementCenter(ctx context.Context, snapshot CPAConfigSnapshot, managementKey, authIndex, projectID string) (cpaQuotaQueryResult, error) {
	if strings.TrimSpace(projectID) == "" {
		return cpaQuotaQueryResult{}, errors.New("Antigravity project ID is unavailable")
	}
	body, errMarshal := json.Marshal(map[string]string{"project": projectID})
	if errMarshal != nil {
		return cpaQuotaQueryResult{}, errMarshal
	}
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    antigravityHubUserAgent,
	}

	var quotaResult managementUpstreamResponse
	var lastErr error
	hadSuccess := false
	for _, endpoint := range antigravityQuotaURLs {
		result, errCall := callManagementAPICall(ctx, snapshot, managementKey, managementAPICallRequest{
			AuthIndex: authIndex,
			Method:    http.MethodPost,
			URL:       endpoint,
			Header:    headers,
			Data:      string(body),
		})
		if errCall != nil {
			lastErr = errCall
			continue
		}
		if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
			quotaResult = result
			lastErr = fmt.Errorf("Antigravity quota endpoint returned HTTP %d", result.StatusCode)
			continue
		}
		hadSuccess = true
		quotaResult = result
		quota, errParse := parseAntigravityManagementQuota(result.Body)
		if errParse != nil {
			lastErr = errParse
			continue
		}
		if len(quota.Groups) > 0 {
			quota.Subscription = queryAntigravitySubscription(ctx, snapshot, managementKey, authIndex)
			quota.ServerTimeOffsetMs = managementCenterServerTimeOffset(result.Header)
			return cpaQuotaQueryResult{quota: quota, status: http.StatusOK}, nil
		}
	}

	if hadSuccess {
		quota, errParse := parseAntigravityManagementQuota(quotaResult.Body)
		if errParse == nil {
			quota.Subscription = queryAntigravitySubscription(ctx, snapshot, managementKey, authIndex)
			quota.ServerTimeOffsetMs = managementCenterServerTimeOffset(quotaResult.Header)
			return cpaQuotaQueryResult{quota: quota, status: http.StatusOK}, nil
		}
	}
	if quotaResult.StatusCode >= http.StatusOK && quotaResult.StatusCode < http.StatusMultipleChoices {
		return cpaQuotaQueryResult{}, lastErr
	}
	if quotaResult.StatusCode != 0 {
		return cpaQuotaQueryResult{status: quotaResult.StatusCode}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("Antigravity quota query failed")
	}
	return cpaQuotaQueryResult{}, lastErr
}

func queryAntigravitySubscription(ctx context.Context, snapshot CPAConfigSnapshot, managementKey, authIndex string) *pluginapi.QuotaSubscription {
	body := `{"metadata":{"ideType":"ANTIGRAVITY"}}`
	result, err := callManagementAPICall(ctx, snapshot, managementKey, managementAPICallRequest{
		AuthIndex: authIndex,
		Method:    http.MethodPost,
		URL:       antigravityLoadCodeAssistURL,
		Header: map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    antigravityHubUserAgent,
		},
		Data: body,
	})
	if err != nil || result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	return parseAntigravitySubscription(result.Body)
}

func callManagementAPICall(ctx context.Context, snapshot CPAConfigSnapshot, managementKey string, payload managementAPICallRequest) (managementUpstreamResponse, error) {
	return retryQuotaRead(ctx, func() (managementUpstreamResponse, error) {
		endpoint, errURL := deriveManagementAPICallURL(snapshot)
		if errURL != nil {
			return managementUpstreamResponse{}, errURL
		}
		body, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return managementUpstreamResponse{}, errMarshal
		}
		requestCtx, cancel := context.WithTimeout(ctx, quotaManagementTimeout)
		defer cancel()
		request, errRequest := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
		if errRequest != nil {
			return managementUpstreamResponse{}, errRequest
		}
		request.Header.Set("Authorization", "Bearer "+managementKey)
		request.Header.Set("Content-Type", "application/json")

		endpointURL, errParseURL := url.Parse(endpoint)
		if errParseURL != nil {
			return managementUpstreamResponse{}, errParseURL
		}
		expectedHost, expectedPort, errSplit := net.SplitHostPort(endpointURL.Host)
		if errSplit != nil {
			return managementUpstreamResponse{}, errSplit
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
			return managementUpstreamResponse{}, errDo
		}
		defer response.Body.Close()
		responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, quotaManagementBodyMax+1))
		if errRead != nil {
			return managementUpstreamResponse{}, errRead
		}
		if len(responseBody) > quotaManagementBodyMax {
			return managementUpstreamResponse{}, errors.New("CPA api-call response exceeded size limit")
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return managementUpstreamResponse{}, fmt.Errorf("CPA api-call endpoint returned HTTP %d", response.StatusCode)
		}
		var envelope managementAPICallEnvelope
		if err := json.Unmarshal(responseBody, &envelope); err != nil {
			return managementUpstreamResponse{}, fmt.Errorf("decode CPA api-call envelope: %w", err)
		}
		result := managementUpstreamResponse{
			StatusCode: envelope.StatusCode,
			Header:     envelope.Header,
			Body:       []byte(envelope.Body),
		}
		if result.StatusCode == 0 {
			return managementUpstreamResponse{}, errors.New("CPA api-call response omitted upstream status code")
		}
		if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
			return result, fmt.Errorf("upstream quota endpoint returned HTTP %d", result.StatusCode)
		}
		return result, nil
	})
}

func deriveManagementAPICallURL(snapshot CPAConfigSnapshot) (string, error) {
	modelsURL, err := deriveModelsURL(snapshot)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(modelsURL)
	if err != nil {
		return "", err
	}
	endpoint.Path = managementAPICallPath
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String(), nil
}

func parseAntigravityManagementQuota(raw []byte) (pluginapi.QuotaFetchResponse, error) {
	var payload struct {
		Groups []pluginapi.QuotaGroup `json:"groups"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode Antigravity quota response: %w", err)
	}
	return pluginapi.QuotaFetchResponse{Groups: payload.Groups}, nil
}

func parseAntigravitySubscription(raw []byte) *pluginapi.QuotaSubscription {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	getTier := func(camel, snake string) map[string]any {
		if tier, ok := payload[camel].(map[string]any); ok {
			return tier
		}
		tier, _ := payload[snake].(map[string]any)
		return tier
	}
	paidTier := getTier("paidTier", "paid_tier")
	currentTier := getTier("currentTier", "current_tier")
	tier := paidTier
	if firstString(tier, "id", "name") == "" {
		tier = currentTier
	}
	if tier == nil {
		return nil
	}
	tierID := firstString(tier, "id")
	tierName := firstString(tier, "name")
	if tierID == "" && tierName == "" {
		return nil
	}
	plans := map[string]string{
		"free-tier":          "free",
		"g1-pro-tier":        "pro",
		"g1-ultra-tier":      "ultra",
		"g1-ultra-lite-tier": "ultra-lite",
	}
	plan := plans[tierID]
	if plan == "" {
		plan = "unknown"
	}
	return &pluginapi.QuotaSubscription{Plan: plan, TierName: tierName, TierID: tierID}
}

func parseClaudeManagementQuota(rawUsage, rawProfile []byte) (pluginapi.QuotaFetchResponse, error) {
	var usage map[string]any
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("decode Claude usage response: %w", err)
	}
	response := pluginapi.QuotaFetchResponse{}
	group := pluginapi.QuotaGroup{DisplayName: "Claude"}
	windowKeys := []struct {
		key   string
		label string
	}{
		{"five_hour", "five-hour"},
		{"seven_day", "seven-day"},
		{"seven_day_oauth_apps", "seven-day-oauth-apps"},
		{"seven_day_opus", "seven-day-opus"},
		{"seven_day_sonnet", "seven-day-sonnet"},
		{"seven_day_cowork", "seven-day-cowork"},
		{"iguana_necktie", "seven-day-fable"},
	}
	for _, item := range windowKeys {
		window, ok := usage[item.key].(map[string]any)
		if !ok {
			continue
		}
		utilization, ok := numberField(window, "utilization")
		if !ok {
			continue
		}
		remaining := 1 - utilization/100
		if remaining < 0 {
			remaining = 0
		} else if remaining > 1 {
			remaining = 1
		}
		reset := claudeResetTime(window["resets_at"])
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
			Window:            item.label,
			RemainingFraction: remaining,
			ResetTime:         reset,
			Description:       fmt.Sprintf("used %.2f%%", utilization),
		})
		if limit, hasLimit := numberField(window, "limit_dollars"); hasLimit {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: item.key + "_limit_dollars", Label: item.label + " limit", Value: limit, Unit: "USD", Format: "currency", Currency: "USD"})
		}
		if used, hasUsed := numberField(window, "used_dollars"); hasUsed {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: item.key + "_used_dollars", Label: item.label + " used", Value: used, Unit: "USD", Format: "currency", Currency: "USD"})
		}
		if remainingDollars, hasRemaining := numberField(window, "remaining_dollars"); hasRemaining {
			response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: item.key + "_remaining_dollars", Label: item.label + " remaining", Value: remainingDollars, Unit: "USD", Format: "currency", Currency: "USD"})
		}
	}
	if limits, ok := usage["limits"].([]any); ok {
		for _, rawLimit := range limits {
			limit, ok := rawLimit.(map[string]any)
			if !ok || !strings.EqualFold(firstString(limit, "kind"), "weekly_scoped") {
				continue
			}
			scope := nestedMap(limit, "scope")
			model := nestedMap(scope, "model")
			if !strings.EqualFold(firstString(model, "display_name"), "fable") && !strings.EqualFold(firstString(model, "display_name"), "fable 5") {
				continue
			}
			utilization, ok := numberField(limit, "percent")
			if !ok {
				continue
			}
			remaining := 1 - utilization/100
			if remaining < 0 {
				remaining = 0
			} else if remaining > 1 {
				remaining = 1
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
				Window:            "seven-day-fable",
				RemainingFraction: remaining,
				ResetTime:         claudeResetTime(limit["resets_at"]),
				Description:       fmt.Sprintf("used %.2f%%", utilization),
			})
			break
		}
	}
	if len(group.Buckets) > 0 {
		response.Groups = []pluginapi.QuotaGroup{group}
	}
	if extra, ok := usage["extra_usage"].(map[string]any); ok {
		for _, metric := range []struct {
			key   string
			label string
		}{
			{"monthly_limit", "Extra usage monthly limit"},
			{"used_credits", "Extra usage used credits"},
			{"utilization", "Extra usage utilization"},
		} {
			if value, exists := numberField(extra, metric.key); exists {
				response.Summary = append(response.Summary, pluginapi.QuotaMetric{Key: "extra_usage_" + metric.key, Label: metric.label, Value: value, Format: "number"})
			}
		}
	}
	if len(response.Groups) == 0 && len(response.Summary) == 0 {
		return pluginapi.QuotaFetchResponse{}, errors.New("Claude usage response contains no recognized quota data")
	}
	if len(rawProfile) > 0 {
		response.Subscription = parseClaudeSubscription(rawProfile)
	}
	return response, nil
}

func parseClaudeSubscription(raw []byte) *pluginapi.QuotaSubscription {
	var profile map[string]any
	if json.Unmarshal(raw, &profile) != nil {
		return nil
	}
	organization := nestedMap(profile, "organization")
	account := nestedMap(profile, "account")
	orgType := strings.ToLower(firstString(organization, "organization_type"))
	orgStatus := strings.ToLower(firstString(organization, "subscription_status"))
	plan := ""
	if orgType == "claude_team" && orgStatus == "active" {
		plan = "plan_team"
	} else if boolValue(account["has_claude_max"]) {
		plan = "plan_max"
	} else if boolValue(account["has_claude_pro"]) {
		plan = "plan_pro"
	} else if falseBoolValue(account["has_claude_max"]) && falseBoolValue(account["has_claude_pro"]) {
		plan = "plan_free"
	}
	if plan == "" {
		return nil
	}
	return &pluginapi.QuotaSubscription{Plan: plan}
}

func boolValue(value any) bool {
	parsed, _ := value.(bool)
	return parsed
}

func falseBoolValue(value any) bool {
	parsed, ok := value.(bool)
	return ok && !parsed
}

func claudeResetTime(value any) string {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	if err != nil {
		return strings.TrimSpace(text)
	}
	return parsed.UTC().Format(time.RFC3339)
}

func antigravityProjectID(entry pluginapi.HostAuthFileEntry, rawAuth []byte) string {
	if value := strings.TrimSpace(entry.ProjectID); value != "" {
		return value
	}
	var root map[string]any
	if json.Unmarshal(rawAuth, &root) != nil {
		return ""
	}
	paths := []string{
		"project_id", "projectId", "metadata.project_id", "metadata.projectId",
		"attributes.project_id", "attributes.projectId", "attributes.gemini_virtual_project",
		"installed.project_id", "installed.projectId", "web.project_id", "web.projectId",
	}
	return firstString(root, paths...)
}

func managementCenterServerTimeOffset(headers map[string][]string) int64 {
	for key, values := range headers {
		if !strings.EqualFold(key, "date") || len(values) == 0 {
			continue
		}
		serverTime, err := http.ParseTime(values[0])
		if err == nil {
			return serverTime.Sub(time.Now()).Milliseconds()
		}
	}
	return 0
}
