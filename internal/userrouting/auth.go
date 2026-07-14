package userrouting

import (
	"net/http"
	"net/url"
	"strings"
)

func APIKeyFromRequest(headers http.Header, query url.Values, validKeys map[string]struct{}) string {
	candidates := []string{
		extractBearerToken(headers.Get("Authorization")),
		strings.TrimSpace(headers.Get("X-Goog-Api-Key")),
		strings.TrimSpace(headers.Get("X-Api-Key")),
		strings.TrimSpace(query.Get("key")),
		strings.TrimSpace(query.Get("auth_token")),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := validKeys[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func extractBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return value
	}
	return strings.TrimSpace(parts[1])
}
