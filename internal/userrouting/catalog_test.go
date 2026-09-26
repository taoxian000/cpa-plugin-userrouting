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
