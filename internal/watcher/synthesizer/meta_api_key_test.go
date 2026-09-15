package synthesizer

import (
	"testing"
	"time"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestConfigSynthesizerBuildsMetaAPIKeyAuth(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	disableCooling := true
	cfg := &appconfig.Config{
		MetaKey: []appconfig.MetaKey{{
			APIKey:         "meta-key",
			Priority:       7,
			Prefix:         "meta",
			BaseURL:        "https://api.meta.ai/v1",
			ProxyURL:       "socks5://proxy.example:1080",
			Headers:        map[string]string{"X-Test": "value"},
			ExcludedModels: []string{"muse-spark-1.1"},
			DisableCooling: &disableCooling,
			Models: []appconfig.MetaModel{{
				Name:         "muse-spark-1.3",
				Alias:        "muse-latest",
				DisplayName:  "Muse Latest",
				ForceMapping: true,
			}},
		}},
	}

	auths, errSynthesize := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         now,
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize() error = %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "meta" || auth.Label != "meta-apikey" || auth.Prefix != "meta" {
		t.Fatalf("auth identity = %+v", auth)
	}
	if auth.Attributes["source"] == "" || auth.Attributes["api_key"] != "meta-key" || auth.Attributes["base_url"] != "https://api.meta.ai/v1" {
		t.Fatalf("auth attributes = %v", auth.Attributes)
	}
	if auth.Attributes["priority"] != "7" || auth.Attributes["header:X-Test"] != "value" {
		t.Fatalf("auth routing attributes = %v", auth.Attributes)
	}
	if auth.Attributes["excluded_models"] != "muse-spark-1.1" || auth.ProxyURL != "socks5://proxy.example:1080" {
		t.Fatalf("auth exclusion/proxy = %q/%q", auth.Attributes["excluded_models"], auth.ProxyURL)
	}
}
