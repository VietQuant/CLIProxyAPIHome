package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
)

func TestMetaAPIKeyDoesNotUseOAuthModelAliases(t *testing.T) {
	if channel := OAuthModelAliasChannel("meta", "api_key"); channel != "" {
		t.Fatalf("OAuthModelAliasChannel(meta, api_key) = %q, want empty", channel)
	}
}

func TestDispatchResolvesMetaAPIKeyModelAlias(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		MetaKey: []internalconfig.MetaKey{{
			APIKey:  "meta-key",
			BaseURL: "https://api.meta.ai/v1",
			Models: []internalconfig.MetaModel{{
				Name:         "muse-spark-1.3",
				Alias:        "muse-latest",
				ForceMapping: true,
			}},
		}},
	})
	auth := &Auth{
		ID:       "meta-api-key-auth",
		Provider: "meta",
		Status:   StatusActive,
		Attributes: map[string]string{
			"api_key":  "meta-key",
			"base_url": "https://api.meta.ai/v1",
		},
		Metadata: map[string]any{
			homeConfigModelsMetadataKey: []map[string]any{{
				"id":            "muse-latest",
				"name":          "muse-spark-1.3",
				"user_defined":  true,
				"force_mapping": true,
			}},
		},
	}
	registerDispatchTestAuth(t, manager, auth, "muse-latest")

	decision, errDispatch := manager.Dispatch(context.Background(), []string{"meta"}, "muse-latest", Options{})
	if errDispatch != nil {
		t.Fatalf("Dispatch() error = %v", errDispatch)
	}
	if decision == nil || decision.Auth == nil || decision.Auth.ID != auth.ID {
		t.Fatalf("Dispatch() decision = %#v", decision)
	}
	if decision.Provider != "meta" || decision.UpstreamModel != "muse-spark-1.3" {
		t.Fatalf("Dispatch() provider/model = %q/%q, want meta/muse-spark-1.3", decision.Provider, decision.UpstreamModel)
	}
	if !decision.ForceMapping || decision.OriginalAlias != "muse-latest" {
		t.Fatalf("Dispatch() force mapping = %t/%q, want true/muse-latest", decision.ForceMapping, decision.OriginalAlias)
	}
}
