package userrouting

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type modelCatalog struct {
	url                  string
	ttl                  time.Duration
	timeout              time.Duration
	client               *http.Client
	internalBypassHeader string
	internalBypassToken  string

	mu        sync.Mutex
	expiresAt time.Time
	models    map[string]struct{}
}

const internalCatalogBypassHeader = "X-CPA-User-Routing-Internal-Catalog"

func newModelCatalog(cfg runtimeConfig) (*modelCatalog, error) {
	randomToken := make([]byte, 32)
	if _, err := rand.Read(randomToken); err != nil {
		return nil, fmt.Errorf("create internal catalog bypass token: %w", err)
	}
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{ // #nosec G402 -- explicitly controlled for a loopback CPA endpoint.
			InsecureSkipVerify: cfg.ModelsTLSInsecureSkipVerify,
		},
	}
	return &modelCatalog{
		url:                  cfg.ModelsURL,
		ttl:                  cfg.ModelCacheTTL,
		timeout:              cfg.ModelLookupTimeout,
		client:               &http.Client{Transport: transport},
		internalBypassHeader: internalCatalogBypassHeader,
		internalBypassToken:  base64.RawURLEncoding.EncodeToString(randomToken),
	}, nil
}

func (c *modelCatalog) isInternalRequest(headers http.Header) bool {
	return c != nil && c.internalBypassToken != "" && headers.Get(c.internalBypassHeader) == c.internalBypassToken
}

func (c *modelCatalog) Exists(ctx context.Context, apiKey, model string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("model catalog is unavailable")
	}
	models, err := c.snapshot(ctx, apiKey)
	if err != nil {
		return false, err
	}
	_, ok := models[modelLookupName(model)]
	return ok, nil
}

// Models fetches a fresh model snapshot for the supplied CPA API key. It is
// intentionally uncached because different downstream keys may expose
// different prefixed model catalogs.
func (c *modelCatalog) Models(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	if c == nil {
		return nil, fmt.Errorf("model catalog is unavailable")
	}
	return c.fetch(ctx, apiKey)
}

func (c *modelCatalog) snapshot(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) > 0 && c.ttl > 0 && time.Now().Before(c.expiresAt) {
		return c.models, nil
	}

	models, err := c.fetch(ctx, apiKey)
	if err != nil {
		return nil, err
	}
	c.models = models
	c.expiresAt = time.Now().Add(c.ttl)
	return models, nil
}

func (c *modelCatalog) fetch(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create CPA model lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set(c.internalBypassHeader, c.internalBypassToken)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query CPA model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("query CPA model catalog: status %d: %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode CPA model catalog: %w", err)
	}
	models := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" {
			models[id] = struct{}{}
		}
	}
	return models, nil
}

func modelLookupName(model string) string {
	model = strings.TrimSpace(model)
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen >= 0 && strings.HasSuffix(model, ")") {
		return strings.TrimSpace(model[:lastOpen])
	}
	return model
}
