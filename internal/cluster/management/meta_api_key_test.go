package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestMetaAPIKeyManagementCRUD(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()

	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	engine := gin.New()
	engine.GET("/meta-api-key", handler.GetMetaKeys)
	engine.PUT("/meta-api-key", handler.PutMetaKeys)
	engine.PATCH("/meta-api-key", handler.PatchMetaKey)
	engine.DELETE("/meta-api-key", handler.DeleteMetaKey)

	putBody := `[{
        "api-key":"meta-key",
        "priority":7,
        "prefix":"meta",
        "base-url":"https://api.meta.ai/v1",
        "proxy-url":"socks5://proxy.example:1080",
        "headers":{"X-Test":"value"},
        "models":[{
            "name":"muse-spark-1.3",
            "alias":"muse-latest",
            "display-name":"Muse Latest",
            "force-mapping":true
        }],
        "excluded-models":["muse-spark-1.1"],
        "disable-cooling":true,
        "request-retry":2
    }]`
	putResp := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/meta-api-key", strings.NewReader(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(putResp, putReq)
	if putResp.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", putResp.Code, putResp.Body.String())
	}

	auths, errAuths := repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() error = %v", errAuths)
	}
	if len(auths) != 1 || auths[0].Provider != "meta" || !strings.HasPrefix(auths[0].Attributes["source"], "config:meta[") {
		t.Fatalf("stored auths = %#v", auths)
	}

	item := getMetaAPIKeyItem(t, engine)
	if item["api-key"] != "meta-key" || item["base-url"] != "https://api.meta.ai/v1" {
		t.Fatalf("GET Meta fields = %#v", item)
	}
	if item["prefix"] != "meta" || item["priority"] != float64(7) {
		t.Fatalf("GET routing fields = %#v", item)
	}

	patchResp := httptest.NewRecorder()
	patchReq := httptest.NewRequest(http.MethodPatch, "/meta-api-key", strings.NewReader(`{"match":"meta-key","value":{"priority":11}}`))
	patchReq.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResp, patchReq)
	if patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", patchResp.Code, patchResp.Body.String())
	}
	item = getMetaAPIKeyItem(t, engine)
	if item["priority"] != float64(11) {
		t.Fatalf("PATCH priority = %#v, want 11", item["priority"])
	}

	deleteResp := httptest.NewRecorder()
	deleteReq := httptest.NewRequest(http.MethodDelete, "/meta-api-key?api-key=meta-key", nil)
	engine.ServeHTTP(deleteResp, deleteReq)
	if deleteResp.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", deleteResp.Code, deleteResp.Body.String())
	}
	auths, errAuths = repo.ListAuths(context.Background())
	if errAuths != nil {
		t.Fatalf("ListAuths() after delete error = %v", errAuths)
	}
	if len(auths) != 0 {
		t.Fatalf("auth count after delete = %d, want 0", len(auths))
	}
}

func getMetaAPIKeyItem(t *testing.T, engine http.Handler) map[string]any {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/meta-api-key", nil)
	engine.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", resp.Code, resp.Body.String())
	}
	var payload map[string][]map[string]any
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	items := payload["meta-api-key"]
	if len(items) != 1 {
		t.Fatalf("GET item count = %d, want 1", len(items))
	}
	return items[0]
}
