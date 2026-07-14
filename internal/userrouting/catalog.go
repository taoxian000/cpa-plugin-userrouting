package userrouting

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type modelCatalog struct {
	url     string
	ttl     time.Duration
	timeout time.Duration
	client  *http.Client

	mu        sync.Mutex
	expiresAt time.Time
	models    map[string]struct{}
}

func newModelCatalog(cfg runtimeConfig) *modelCatalog {
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{ // #nosec G402 -- explicitly controlled for a loopback CPA endpoint.
			InsecureSkipVerify: cfg.ModelsTLSInsecureSkipVerify,
		},
	}
	return &modelCatalog{
		url:     cfg.ModelsURL,
		ttl:     cfg.ModelCacheTTL,
		timeout: cfg.ModelLookupTimeout,
		client:  &http.Client{Transport: transport},
	}
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

func (c *modelCatalog) snapshot(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) > 0 && c.ttl > 0 && time.Now().Before(c.expiresAt) {
		return c.models, nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create CPA model lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
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
	c.models = models
	c.expiresAt = time.Now().Add(c.ttl)
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
