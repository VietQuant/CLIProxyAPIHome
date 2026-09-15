package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateDevinModelsJSON(t *testing.T) {
	t.Run("valid envelope namespaced", func(t *testing.T) {
		models, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"devin/swe-2","display_name":"SWE-2"}]}`))
		if err != nil {
			t.Fatalf("ValidateDevinModelsJSON() error = %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/swe-2" || models[0].Type != "devin" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("bare id is namespaced", func(t *testing.T) {
		models, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"swe-2"}]}`))
		if err != nil {
			t.Fatalf("ValidateDevinModelsJSON() error = %v", err)
		}
		if len(models) != 1 || models[0].ID != "devin/swe-2" {
			t.Fatalf("models = %+v", models)
		}
	})
	t.Run("duplicate id", func(t *testing.T) {
		if _, err := ValidateDevinModelsJSON([]byte(`{"devin":[{"id":"devin/swe-2"},{"id":"devin/SWE-2"}]}`)); err == nil {
			t.Fatal("expected duplicate id error")
		}
	})
}

func TestEmbeddedDevinModelsLoadedOnStartup(t *testing.T) {
	models := GetDevinModels()
	if len(models) < 30 {
		t.Fatalf("GetDevinModels() count = %d, want at least 30", len(models))
	}
	found := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			found[model.ID] = true
		}
	}
	for _, id := range []string{"devin/swe-2", "devin/claude-fable-5-1", "devin/gpt-6-astra"} {
		if !found[id] {
			t.Errorf("embedded catalog missing %q", id)
		}
	}
}

func TestDevinModelsRemoteFetchFallback(t *testing.T) {
	origURLs := devinModelsURLs
	t.Cleanup(func() {
		devinModelsURLs = origURLs
		_, _ = loadDevinModelsFromBytes(embeddedDevinModelsJSON, "restore-embed")
	})

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	devinModelsURLs = []string{failing.URL + "/devin_models.json"}

	initialCount := len(GetDevinModels())
	if initialCount == 0 {
		t.Fatal("expected non-empty initial Devin models")
	}
	tryRefreshDevinModels(context.Background(), "test failing refresh")
	if got := len(GetDevinModels()); got != initialCount {
		t.Fatalf("catalog count after failed refresh = %d, want %d", got, initialCount)
	}

	valid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devin":[{"id":"devin/custom-test-model","display_name":"Custom Test Model"}]}`))
	}))
	t.Cleanup(valid.Close)
	devinModelsURLs = []string{valid.URL + "/devin_models.json"}
	tryRefreshDevinModels(context.Background(), "test succeeding refresh")

	updated := GetDevinModels()
	if len(updated) != 1 || updated[0].ID != "devin/custom-test-model" {
		t.Fatalf("updated catalog = %+v", updated)
	}
}

func TestDevinModelsRemoteSkipsInvalidPrimaryAndUsesBackup(t *testing.T) {
	origURLs := devinModelsURLs
	t.Cleanup(func() {
		devinModelsURLs = origURLs
		_, _ = loadDevinModelsFromBytes(embeddedDevinModelsJSON, "restore-embed")
	})

	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not a catalog</html>"))
	}))
	t.Cleanup(invalid.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devin":[{"id":"devin/backup-model","display_name":"Backup Model"}]}`))
	}))
	t.Cleanup(backup.Close)

	devinModelsURLs = []string{invalid.URL + "/devin_models.json", backup.URL + "/devin_models.json"}
	tryRefreshDevinModels(context.Background(), "test invalid primary")

	updated := GetDevinModels()
	if len(updated) != 1 || updated[0].ID != "devin/backup-model" {
		t.Fatalf("updated catalog = %+v, want backup-model from second source", updated)
	}
}
