package userrouting

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	snapshots map[string]modelCatalogSnapshot
	inFlight  map[string]*modelCatalogFlight
}

type modelCatalogFlight struct {
	done   chan struct{}
	models map[string]struct{}
	err    error
}

var errCatalogLookupAborted = errors.New("CPA model catalog lookup aborted")

type modelCatalogSnapshot struct {
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
		Proxy:           nil,
		IdleConnTimeout: 90 * time.Second,
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
	return c.fetch(ctx, apiKey, false)
}

// ClaudeModels reads CPA's Anthropic-format catalog. It can contain models
// absent from the OpenAI-format catalog, and CPA may cloak their IDs.
func (c *modelCatalog) ClaudeModels(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	if c == nil {
		return nil, fmt.Errorf("model catalog is unavailable")
	}
	return c.fetch(ctx, apiKey, true)
}

func (c *modelCatalog) snapshot(ctx context.Context, apiKey string) (map[string]struct{}, error) {
	c.mu.Lock()
	if snapshot, exists := c.snapshots[apiKey]; exists && len(snapshot.models) > 0 && c.ttl > 0 && time.Now().Before(snapshot.expiresAt) {
		c.mu.Unlock()
		return snapshot.models, nil
	}
	// Concurrent misses for one key share a lookup so that cache expiry under
	// load does not fan out into one catalog download per request.
	if flight := c.inFlight[apiKey]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return flight.models, flight.err
		}
	}
	if c.inFlight == nil {
		c.inFlight = make(map[string]*modelCatalogFlight)
	}
	flight := &modelCatalogFlight{done: make(chan struct{}), err: errCatalogLookupAborted}
	c.inFlight[apiKey] = flight
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, apiKey)
		if flight.err == nil && c.ttl > 0 && len(flight.models) > 0 {
			now := time.Now()
			if c.snapshots == nil {
				c.snapshots = make(map[string]modelCatalogSnapshot)
			}
			for key, snapshot := range c.snapshots {
				if !now.Before(snapshot.expiresAt) {
					delete(c.snapshots, key)
				}
			}
			c.snapshots[apiKey] = modelCatalogSnapshot{expiresAt: now.Add(c.ttl), models: flight.models}
		}
		close(flight.done)
		c.mu.Unlock()
	}()
	// The lookup runs unlocked so one slow or timed-out CPA response cannot
	// serialize every routed request behind it.
	flight.models, flight.err = c.fetch(ctx, apiKey, false)
	return flight.models, flight.err
}

// close releases idle keep-alive connections held by a replaced runtime.
func (c *modelCatalog) close() {
	if c != nil && c.client != nil {
		c.client.CloseIdleConnections()
	}
}

func (c *modelCatalog) fetch(ctx context.Context, apiKey string, claude bool) (map[string]struct{}, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(lookupCtx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("create CPA model lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if claude {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
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
		if claude {
			id = decodeClaudeCatalogModelID(id)
		}
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
