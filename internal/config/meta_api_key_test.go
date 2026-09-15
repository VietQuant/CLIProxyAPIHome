package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptionalParsesAndSanitizesMetaKeys(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	payload := []byte(`meta-api-key:
  - api-key: " meta-key "
    priority: 9
    prefix: "/meta/"
    proxy-url: "socks5://proxy.example:1080"
    headers:
      X-Test: " value "
    models:
      - name: "muse-spark-1.3"
        alias: "muse-latest"
        display-name: "Muse Latest"
        force-mapping: true
    excluded-models:
      - " MUSE-SPARK-1.1 "
    disable-cooling: false
  - api-key: "dca:dropped"
  - api-key: " "
`)
	if errWrite := os.WriteFile(configPath, payload, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if len(cfg.MetaKey) != 1 {
		t.Fatalf("MetaKey count = %d, want 1", len(cfg.MetaKey))
	}
	entry := cfg.MetaKey[0]
	if entry.APIKey != "meta-key" || entry.BaseURL != "https://api.meta.ai/v1" {
		t.Fatalf("Meta key/base URL = %q/%q", entry.APIKey, entry.BaseURL)
	}
	if entry.Prefix != "meta" || entry.Priority != 9 || entry.DisableCooling == nil || *entry.DisableCooling {
		t.Fatalf("Meta routing fields = %+v", entry)
	}
	if entry.Headers["X-Test"] != "value" || len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "muse-spark-1.1" {
		t.Fatalf("Meta normalized fields = headers:%v excluded:%v", entry.Headers, entry.ExcludedModels)
	}
	if len(entry.Models) != 1 {
		t.Fatalf("Meta model count = %d, want 1", len(entry.Models))
	}
	model := entry.Models[0]
	if model.Name != "muse-spark-1.3" || model.Alias != "muse-latest" || model.DisplayName != "Muse Latest" || !model.ForceMapping {
		t.Fatalf("Meta model = %+v", model)
	}
}
