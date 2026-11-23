package userrouting

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaCacheReleasesFlightWhenLoadPanics(t *testing.T) {
	var cache quotaReadCache
	key := newQuotaCacheKey(quotaCacheCodexResource, quotaCodexProvider, "auth-1", []byte(`{}`), nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("load panic was swallowed")
			}
		}()
		_, _ = cache.get(context.Background(), key, time.Minute, func() (quotaCacheValue, bool, error) {
			panic("boom")
		})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	loads := 0
	if _, err := cache.get(ctx, key, time.Minute, func() (quotaCacheValue, bool, error) {
		loads++
		return quotaCacheValue{}, true, nil
	}); err != nil || loads != 1 {
		t.Fatalf("read after panic = %v, loads = %d; want a fresh load instead of waiting on the dead flight", err, loads)
	}
	if len(cache.inFlight) != 0 {
		t.Fatalf("in-flight entries leaked: %d", len(cache.inFlight))
	}
}

func TestModelCatalogCoalescesConcurrentMisses(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"data":[{"id":"a/gpt-6"}]}`))
	}))
	defer server.Close()
	catalog, err := newModelCatalog(runtimeConfig{ModelsURL: server.URL, ModelCacheTTL: time.Minute, ModelLookupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exists, err := catalog.Exists(context.Background(), "key-a", "a/gpt-6")
			if err == nil && !exists {
				err = context.Canceled
			}
			errs <- err
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Exists() error = %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("catalog downloads = %d, want 1 shared lookup", got)
	}
	if len(catalog.inFlight) != 0 {
		t.Fatalf("in-flight entries leaked: %d", len(catalog.inFlight))
	}
}

func TestModelCatalogPrunesExpiredSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a/gpt-6"}]}`))
	}))
	defer server.Close()
	catalog, err := newModelCatalog(runtimeConfig{ModelsURL: server.URL, ModelCacheTTL: 20 * time.Millisecond, ModelLookupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Exists(context.Background(), "old-key", "a/gpt-6"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := catalog.Exists(context.Background(), "new-key", "a/gpt-6"); err != nil {
		t.Fatal(err)
	}
	if _, stale := catalog.snapshots["old-key"]; stale || len(catalog.snapshots) != 1 {
		t.Fatalf("snapshots = %d entries (old-key retained=%v), want only new-key", len(catalog.snapshots), stale)
	}
}

func TestRuntimeCloseDropsIdleCatalogConnections(t *testing.T) {
	closed := make(chan struct{}, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a/gpt-6"}]}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	server.Start()
	defer server.Close()
	catalog, err := newModelCatalog(runtimeConfig{ModelsURL: server.URL, ModelLookupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Models(context.Background(), "key-a"); err != nil {
		t.Fatal(err)
	}
	(&Runtime{catalog: catalog}).Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle catalog connection survived Runtime.Close")
	}
}
