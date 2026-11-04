package userrouting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModelCatalogMarksInternalLookup(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotToken = req.Header.Get(internalCatalogBypassHeader)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"dxy/gpt-6-luna"}]}`))
	}))
	defer server.Close()

	catalog, err := newModelCatalog(runtimeConfig{
		ModelsURL:          server.URL + "/v1/models",
		ModelLookupTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("newModelCatalog() error = %v", err)
	}
	if _, err := catalog.Models(context.Background(), "downstream-key"); err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if gotToken == "" || gotToken != catalog.internalBypassToken {
		t.Fatalf("internal catalog token = %q, want the catalog's private token", gotToken)
	}
}

func TestModelCatalogCachesByDownstreamAPIKey(t *testing.T) {
	reads := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("Authorization")
		reads[key]++
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "Bearer key-a":
			_, _ = w.Write([]byte(`{"data":[{"id":"a/gpt-6-sol"}]}`))
		case "Bearer key-b":
			_, _ = w.Write([]byte(`{"data":[{"id":"b/gpt-6-sol"}]}`))
		default:
			t.Errorf("unexpected Authorization header")
		}
	}))
	defer server.Close()
	catalog, err := newModelCatalog(runtimeConfig{
		ModelsURL: server.URL + "/v1/models", ModelCacheTTL: time.Minute, ModelLookupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if exists, err := catalog.Exists(context.Background(), "key-a", "a/gpt-6-sol"); err != nil || !exists {
			t.Fatalf("key-a catalog = (%v, %v)", exists, err)
		}
		if exists, err := catalog.Exists(context.Background(), "key-b", "b/gpt-6-sol"); err != nil || !exists {
			t.Fatalf("key-b catalog = (%v, %v)", exists, err)
		}
	}
	if reads["Bearer key-a"] != 1 || reads["Bearer key-b"] != 1 {
		t.Fatalf("catalog reads = %#v, want one read per key", reads)
	}
}
