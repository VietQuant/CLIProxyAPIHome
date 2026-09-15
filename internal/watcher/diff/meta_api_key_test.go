package diff

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestBuildConfigChangeDetailsIncludesRedactedMetaChanges(t *testing.T) {
	disableCooling := true
	oldCfg := &config.Config{MetaKey: []config.MetaKey{{
		APIKey:  "old-secret",
		BaseURL: "https://api.meta.ai/v1",
		Models:  []config.MetaModel{{Name: "muse-spark-1.3", Alias: "muse-latest"}},
	}}}
	newCfg := &config.Config{MetaKey: []config.MetaKey{{
		APIKey:         "new-secret",
		Priority:       8,
		BaseURL:        "https://api.meta.ai/v1",
		DisableCooling: &disableCooling,
		Models:         []config.MetaModel{{Name: "muse-spark-1.3", Alias: "muse-latest", ForceMapping: true}},
	}}}

	changes := strings.Join(BuildConfigChangeDetails(oldCfg, newCfg), "\n")
	for _, want := range []string{
		"meta[0].priority: 0 -> 8",
		"meta[0].disable-cooling: inherit -> true",
		"meta[0].api-key: updated",
		"meta[0].models: updated",
	} {
		if !strings.Contains(changes, want) {
			t.Fatalf("changes missing %q:\n%s", want, changes)
		}
	}
	if strings.Contains(changes, "old-secret") || strings.Contains(changes, "new-secret") {
		t.Fatalf("changes leaked Meta API key material:\n%s", changes)
	}
}
