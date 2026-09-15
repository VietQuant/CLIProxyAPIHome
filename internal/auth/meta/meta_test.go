package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMintAPIKeyOmitsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"api_key":"LLM|leaked","error":"invalid_token"}`))
	}))
	t.Cleanup(server.Close)

	auth := NewMetaAuth(nil)
	auth.httpClient = server.Client()
	auth.mintURL = server.URL
	_, errMint := auth.MintAPIKey(context.Background(), "dca:test")
	if errMint == nil {
		t.Fatal("MintAPIKey() error = nil, want status error")
	}
	if !strings.Contains(errMint.Error(), "HTTP 401") {
		t.Fatalf("error = %v, want HTTP 401", errMint)
	}
	if strings.Contains(errMint.Error(), "LLM|leaked") || strings.Contains(errMint.Error(), "invalid_token") {
		t.Fatalf("error leaked upstream body: %v", errMint)
	}
}
