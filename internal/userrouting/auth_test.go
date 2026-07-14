package userrouting

import (
	"net/http"
	"net/url"
	"testing"
)

func TestAPIKeyFromRequestMatchesCPAProviderPriority(t *testing.T) {
	headers := http.Header{
		"Authorization":  []string{"Bearer invalid"},
		"X-Goog-Api-Key": []string{"valid-google"},
		"X-Api-Key":      []string{"valid-anthropic"},
	}
	keys := map[string]struct{}{
		"valid-google":    {},
		"valid-anthropic": {},
	}
	got := APIKeyFromRequest(headers, url.Values{"key": []string{"valid-query"}}, keys)
	if got != "valid-google" {
		t.Fatalf("APIKeyFromRequest() = %q, want valid-google", got)
	}
}

func TestExtractBearerTokenAcceptsRawKey(t *testing.T) {
	if got := extractBearerToken("raw-key"); got != "raw-key" {
		t.Fatalf("extractBearerToken() = %q", got)
	}
}
