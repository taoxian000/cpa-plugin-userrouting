package userrouting

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleAllKiroQuota uses the Kiro plugin's native QuotaProvider through CPA's
// management API. Its quota.fetch implementation owns the AWS account routing,
// token refresh policy, and getUsageLimits response mapping.
func (r *Runtime) handleAllKiroQuota(ctx context.Context, snapshot CPAConfigSnapshot) pluginapi.ManagementResponse {
	listRaw, err := r.host.Call(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "CPA auth list is unavailable"})
	}
	var listed hostAuthListResponse
	if err := json.Unmarshal(listRaw, &listed); err != nil {
		return quotaJSONResponse(http.StatusBadGateway, map[string]string{"error": "invalid CPA auth list response"})
	}
	entries := make([]pluginapi.HostAuthFileEntry, 0)
	for _, entry := range listed.Files {
		provider := strings.TrimSpace(entry.Provider)
		if provider == "" {
			provider = strings.TrimSpace(entry.Type)
		}
		if strings.EqualFold(provider, "kiro") && !entry.Disabled && !entry.Unavailable && strings.TrimSpace(entry.AuthIndex) != "" {
			entries = append(entries, entry)
		}
	}
	result := providerQuotaResourceResponse{NominalAccounts: make(map[string]providerQuotaAccount)}
	if len(entries) == 0 {
		return quotaJSONResponse(http.StatusOK, result)
	}
	managementKey := strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD"))
	if managementKey == "" {
		return quotaJSONResponse(http.StatusServiceUnavailable, map[string]string{"error": "Kiro quota lookup requires MANAGEMENT_PASSWORD in the CPA process environment"})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].AuthIndex < entries[j].AuthIndex })
	failed := 0
	for _, entry := range entries {
		rawAuth, errGet := r.getAuthJSON(entry.AuthIndex)
		if errGet != nil {
			failed++
			continue
		}
		account := strings.TrimSpace(entry.Email)
		if account == "" {
			account = decodeAuthMaterial(rawAuth).email()
		}
		if account == "" {
			account = strings.TrimSpace(entry.Label)
		}
		if account == "" {
			account = strings.TrimSpace(entry.Name)
		}
		if account == "" {
			account = entry.AuthIndex
		}
		if _, exists := result.NominalAccounts[account]; exists {
			account += " (" + entry.AuthIndex + ")"
		}
		cacheKey := newQuotaCacheKey(quotaCacheProviderQuery, "kiro", entry.AuthIndex, rawAuth, nil)
		cached, errQuery := r.quotaCache.get(ctx, cacheKey, r.config.QuotaCacheTTL, func() (quotaCacheValue, bool, error) {
			query, queryErr := queryCPAManagementQuota(ctx, snapshot, managementKey, entry.AuthIndex, "kiro")
			_, hasMonthlyBucket := kiroMonthlyQuotaAccount(query.quota)
			return quotaCacheValue{query: query}, query.status == http.StatusOK && hasMonthlyBucket, queryErr
		})
		if errQuery != nil || cached.query.status != http.StatusOK {
			failed++
			continue
		}
		quota, ok := kiroMonthlyQuotaAccount(cached.query.quota)
		if !ok {
			failed++
			continue
		}
		result.NominalAccounts[account] = quota
	}
	if failed > 0 {
		result.Partial = true
		result.Errors = []string{"one or more Kiro credential quota queries failed"}
	}
	if len(result.NominalAccounts) == 0 && failed > 0 {
		return quotaJSONResponse(http.StatusBadGateway, result)
	}
	return quotaJSONResponse(http.StatusOK, result)
}

func kiroMonthlyQuotaAccount(quota pluginapi.QuotaFetchResponse) (providerQuotaAccount, bool) {
	account := normalizeProviderQuotaAccount("kiro", quota)
	for _, group := range account.Groups {
		for _, bucket := range group.Buckets {
			if strings.EqualFold(bucket.Window, "Plan") || strings.EqualFold(bucket.Window, "monthly") {
				bucket.SourceWindow = ""
				bucket.Window = "monthly"
				account.Groups = []providerQuotaGroup{{DisplayName: "Kiro", Buckets: []providerQuotaBucket{bucket}}}
				return account, true
			}
		}
	}
	return providerQuotaAccount{}, false
}
