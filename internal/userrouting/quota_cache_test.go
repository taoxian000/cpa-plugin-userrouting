package userrouting

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaReadCacheScopesAuthIndexProviderAndCredential(t *testing.T) {
	var cache quotaReadCache
	ctx := context.Background()
	key := newQuotaCacheKey(quotaCacheProviderQuery, "Claude", "auth-1", []byte("credential-1"), nil)
	reads := 0
	load := func() (quotaCacheValue, bool, error) {
		reads++
		return quotaCacheValue{query: cpaQuotaQueryResult{status: reads}}, true, nil
	}
	for i := 0; i < 2; i++ {
		value, err := cache.get(ctx, key, time.Minute, load)
		if err != nil || value.query.status != 1 {
			t.Fatalf("cached query %d = (%#v, %v), want first result", i, value, err)
		}
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want one", reads)
	}
	for _, other := range []quotaCacheKey{
		newQuotaCacheKey(quotaCacheProviderQuery, "claude", "auth-2", []byte("credential-1"), nil),
		newQuotaCacheKey(quotaCacheProviderQuery, "antigravity", "auth-1", []byte("credential-1"), nil),
		newQuotaCacheKey(quotaCacheProviderQuery, "claude", "auth-1", []byte("credential-2"), nil),
	} {
		if _, err := cache.get(ctx, other, time.Minute, load); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 4 {
		t.Fatalf("reads = %d, want separate reads for auth_index, provider, and credential", reads)
	}
	cache.invalidate("CLAUDE", "auth-1")
	if _, err := cache.get(ctx, key, time.Minute, load); err != nil || reads != 5 {
		t.Fatalf("invalidation failed: reads=%d, err=%v", reads, err)
	}
}

func TestQuotaReadCacheSkipsFailuresIncompleteResultsAndDisabledTTL(t *testing.T) {
	var cache quotaReadCache
	ctx := context.Background()
	key := newQuotaCacheKey(quotaCacheCodexResource, "codex", "auth-1", []byte("credential"), nil)
	reads := 0
	loadIncomplete := func() (quotaCacheValue, bool, error) {
		reads++
		return quotaCacheValue{creditsIncomplete: true}, false, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.get(ctx, key, time.Minute, loadIncomplete); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Fatalf("incomplete results were cached: reads=%d", reads)
	}
	loadFailure := func() (quotaCacheValue, bool, error) {
		reads++
		return quotaCacheValue{}, false, errors.New("upstream failed")
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.get(ctx, key, time.Minute, loadFailure); err == nil {
			t.Fatal("expected upstream failure")
		}
	}
	if reads != 4 {
		t.Fatalf("errors were cached: reads=%d", reads)
	}
	loadSuccess := func() (quotaCacheValue, bool, error) {
		reads++
		return quotaCacheValue{}, true, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.get(ctx, key, 0, loadSuccess); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 6 {
		t.Fatalf("disabled cache skipped reads: reads=%d", reads)
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.get(ctx, key, time.Nanosecond, loadSuccess); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if reads != 8 {
		t.Fatalf("expired cache skipped reads: reads=%d", reads)
	}
}

func TestQuotaReadCacheCoalescesConcurrentMisses(t *testing.T) {
	var cache quotaReadCache
	key := newQuotaCacheKey(quotaCacheProviderQuery, "antigravity", "auth-1", []byte("credential"), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	load := func() (quotaCacheValue, bool, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		<-release
		return quotaCacheValue{query: cpaQuotaQueryResult{status: 200}}, true, nil
	}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 10)
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := cache.get(context.Background(), key, time.Minute, load)
			if err == nil && value.query.status != 200 {
				err = errors.New("incorrect shared result")
			}
			errorsSeen <- err
		}()
	}
	<-started
	close(release)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("upstream reads = %d, want one coalesced read", got)
	}
}

func TestQuotaReadCacheInvalidationRejectsOldInFlightResult(t *testing.T) {
	var cache quotaReadCache
	key := newQuotaCacheKey(quotaCacheCodexNative, "codex", "auth-1", []byte("credential"), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = cache.get(context.Background(), key, time.Minute, func() (quotaCacheValue, bool, error) {
			close(started)
			<-release
			return quotaCacheValue{query: cpaQuotaQueryResult{status: 100}}, true, nil
		})
	}()
	<-started
	cache.invalidate("codex", "auth-1")
	value, err := cache.get(context.Background(), key, time.Minute, func() (quotaCacheValue, bool, error) {
		return quotaCacheValue{query: cpaQuotaQueryResult{status: 200}}, true, nil
	})
	if err != nil || value.query.status != 200 {
		t.Fatalf("post-invalidation load = (%#v, %v), want fresh result", value, err)
	}
	close(release)
	<-oldDone
	value, err = cache.get(context.Background(), key, time.Minute, func() (quotaCacheValue, bool, error) {
		t.Fatal("old in-flight result evicted the new result")
		return quotaCacheValue{}, false, nil
	})
	if err != nil || value.query.status != 200 {
		t.Fatalf("cached result = (%#v, %v), want fresh result", value, err)
	}
}

func TestQuotaReadCacheDirectResetInvalidatesAllCodexCredentials(t *testing.T) {
	var cache quotaReadCache
	ctx := context.Background()
	reads := 0
	load := func() (quotaCacheValue, bool, error) {
		reads++
		return quotaCacheValue{}, true, nil
	}
	codexA := newQuotaCacheKey(quotaCacheCodexNative, "codex", "auth-a", []byte("a"), nil)
	codexB := newQuotaCacheKey(quotaCacheCodexResource, "codex", "auth-b", []byte("b"), nil)
	claude := newQuotaCacheKey(quotaCacheProviderQuery, "claude", "auth-c", []byte("c"), nil)
	for _, key := range []quotaCacheKey{codexA, codexB, claude} {
		if _, err := cache.get(ctx, key, time.Minute, load); err != nil {
			t.Fatal(err)
		}
	}
	cache.invalidate("codex", "")
	for _, key := range []quotaCacheKey{codexA, codexB, claude} {
		if _, err := cache.get(ctx, key, time.Minute, load); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 5 {
		t.Fatalf("upstream reads = %d, want both Codex credentials refreshed and Claude cached", reads)
	}
}
