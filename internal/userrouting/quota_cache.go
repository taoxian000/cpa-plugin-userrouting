package userrouting

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	quotaCacheCodexResource = "codex-resource"
	quotaCacheCodexNative   = "codex-native"
	quotaCacheProviderQuery = "provider-query"
	quotaCacheMaxEntries    = 256
)

// quotaCacheKey is scoped to a CPA auth_index and the current credential
// contents. The hash prevents an updated/reused auth_index from receiving a
// previous credential's quota without retaining tokens in the cache key.
type quotaCacheKey struct {
	kind        string
	provider    string
	authIndex   string
	fingerprint [sha256.Size]byte
}

func newQuotaCacheKey(kind, provider, authIndex string, rawAuth, extra []byte) quotaCacheKey {
	hash := sha256.New()
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(rawAuth)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(rawAuth)
	binary.LittleEndian.PutUint64(length[:], uint64(len(extra)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(extra)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return quotaCacheKey{
		kind:        kind,
		provider:    strings.ToLower(strings.TrimSpace(provider)),
		authIndex:   strings.TrimSpace(authIndex),
		fingerprint: fingerprint,
	}
}

type quotaCacheValue struct {
	account           quotaResourceAccount
	creditsIncomplete bool
	query             cpaQuotaQueryResult
	native            pluginapi.QuotaFetchResponse
}

func nativeQuotaCacheKey(authIndex string, storageJSON []byte, metadata map[string]any, attributes map[string]string) (quotaCacheKey, error) {
	extra, err := json.Marshal(struct {
		Metadata   map[string]any    `json:"metadata"`
		Attributes map[string]string `json:"attributes"`
	}{Metadata: metadata, Attributes: attributes})
	if err != nil {
		return quotaCacheKey{}, err
	}
	return newQuotaCacheKey(quotaCacheCodexNative, quotaCodexProvider, authIndex, storageJSON, extra), nil
}

type quotaCacheEntry struct {
	value     quotaCacheValue
	expiresAt time.Time
}

type quotaCacheFlight struct {
	done  chan struct{}
	value quotaCacheValue
	err   error
}

// quotaReadCache stores only successful read results. It coalesces simultaneous
// misses for the same auth_index, but never caches errors or reset operations.
// The zero value is ready to use, including in Runtime values built by tests.
type quotaReadCache struct {
	mu       sync.Mutex
	entries  map[quotaCacheKey]quotaCacheEntry
	inFlight map[quotaCacheKey]*quotaCacheFlight
}

func (c *quotaReadCache) get(ctx context.Context, key quotaCacheKey, ttl time.Duration, load func() (quotaCacheValue, bool, error)) (quotaCacheValue, error) {
	if err := ctx.Err(); err != nil {
		return quotaCacheValue{}, err
	}
	if ttl <= 0 || key.authIndex == "" {
		value, _, err := load()
		return value, err
	}

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			c.mu.Unlock()
			return entry.value, nil
		}
		delete(c.entries, key)
	}
	if flight := c.inFlight[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return quotaCacheValue{}, ctx.Err()
		case <-flight.done:
			return flight.value, flight.err
		}
	}
	if c.inFlight == nil {
		c.inFlight = make(map[quotaCacheKey]*quotaCacheFlight)
	}
	flight := &quotaCacheFlight{done: make(chan struct{})}
	c.inFlight[key] = flight
	c.mu.Unlock()

	value, cacheable, err := load()
	c.mu.Lock()
	flight.value, flight.err = value, err
	if c.inFlight[key] == flight {
		delete(c.inFlight, key)
		if err == nil && cacheable {
			if c.entries == nil {
				c.entries = make(map[quotaCacheKey]quotaCacheEntry)
			}
			now := time.Now()
			for staleKey, entry := range c.entries {
				if !now.Before(entry.expiresAt) {
					delete(c.entries, staleKey)
				}
			}
			if len(c.entries) >= quotaCacheMaxEntries {
				var oldestKey quotaCacheKey
				var oldestTime time.Time
				for existingKey, entry := range c.entries {
					if oldestTime.IsZero() || entry.expiresAt.Before(oldestTime) {
						oldestKey, oldestTime = existingKey, entry.expiresAt
					}
				}
				delete(c.entries, oldestKey)
			}
			c.entries[key] = quotaCacheEntry{value: value, expiresAt: now.Add(ttl)}
		}
	}
	close(flight.done)
	c.mu.Unlock()
	return value, err
}

// invalidate removes all cache entries and in-flight bindings for one CPA
// credential. An empty authIndex invalidates the provider's full cache, used
// when /quota/direct/reset has no CPA auth_index to identify.
func (c *quotaReadCache) invalidate(provider, authIndex string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	authIndex = strings.TrimSpace(authIndex)
	c.mu.Lock()
	for key := range c.entries {
		if key.provider == provider && (authIndex == "" || key.authIndex == authIndex) {
			delete(c.entries, key)
		}
	}
	for key := range c.inFlight {
		if key.provider == provider && (authIndex == "" || key.authIndex == authIndex) {
			delete(c.inFlight, key)
		}
	}
	c.mu.Unlock()
}
