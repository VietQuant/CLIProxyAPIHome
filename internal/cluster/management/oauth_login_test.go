package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestGetAuthStatusRejectsUnknownStateAndAcceptsCompletedState(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/get-auth-status", handler.GetAuthStatus)

	unknown := performOAuthStatusRequest(t, engine, "unknown-state")
	if unknown.Status != "error" || unknown.Error != "unknown or expired state" {
		t.Fatalf("unknown state response = %+v, want unknown/expired error", unknown)
	}

	record, errRecord := cluster.NewOAuthSessionRecord("codex", "completed-state", nil, time.Now().UTC())
	if errRecord != nil {
		t.Fatalf("NewOAuthSessionRecord() error = %v", errRecord)
	}
	if errUpsert := repo.UpsertOAuthSession(context.Background(), record); errUpsert != nil {
		t.Fatalf("UpsertOAuthSession() error = %v", errUpsert)
	}
	if errComplete := repo.CompleteOAuthSession(context.Background(), record.State); errComplete != nil {
		t.Fatalf("CompleteOAuthSession() error = %v", errComplete)
	}

	completed := performOAuthStatusRequest(t, engine, record.State)
	if completed.Status != "ok" || completed.Error != "" {
		t.Fatalf("completed state response = %+v, want success", completed)
	}
}

type oauthStatusResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func performOAuthStatusRequest(t *testing.T, handler http.Handler, state string) oauthStatusResponse {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/get-auth-status?state="+state, nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /get-auth-status returned %d, want %d; body=%s", resp.Code, http.StatusOK, resp.Body.String())
	}

	var body oauthStatusResponse
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &body); errDecode != nil {
		t.Fatalf("decode auth status response: %v", errDecode)
	}
	return body
}

func TestDeviceFlowAuthView(t *testing.T) {
	url, code, errView := deviceFlowAuthView("https://auth.example/complete", "https://auth.example/verify", "ABCD")
	if errView != nil || url != "https://auth.example/complete" || code != "ABCD" {
		t.Fatalf("complete URI view = %q %q %v", url, code, errView)
	}

	url, code, errView = deviceFlowAuthView("", "https://auth.example/verify", "ABCD")
	if errView != nil || url != "https://auth.example/verify" || code != "ABCD" {
		t.Fatalf("verification URI view = %q %q %v", url, code, errView)
	}

	if _, _, errView = deviceFlowAuthView("", "https://auth.example/verify", ""); errView == nil {
		t.Fatal("expected error when verification URI has no user code")
	}
	if _, _, errView = deviceFlowAuthView("", "", "ABCD"); errView == nil {
		t.Fatal("expected error when both verification URLs are empty")
	}
}
