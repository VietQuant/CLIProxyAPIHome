package devin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExchangeCodeForTokenOmitsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"token":"response-secret","error":"invalid_grant"}`))
	}))
	t.Cleanup(server.Close)

	svc := &DevinAuthService{client: server.Client(), apiBaseURL: server.URL}
	_, errExchange := svc.ExchangeCodeForToken(context.Background(), "code", "verifier")
	if errExchange == nil {
		t.Fatal("ExchangeCodeForToken() error = nil, want status error")
	}
	if !strings.Contains(errExchange.Error(), "status 400") {
		t.Fatalf("error = %v, want status 400", errExchange)
	}
	if strings.Contains(errExchange.Error(), "response-secret") || strings.Contains(errExchange.Error(), "invalid_grant") {
		t.Fatalf("error leaked upstream body: %v", errExchange)
	}
}
