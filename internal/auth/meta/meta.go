// Package meta provides authentication and token management for Meta Muse (api.meta.ai).
// It implements the RFC 8628 OAuth2 Device Authorization Grant flow for Meta accounts.
package meta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultAPIBaseURL is the default official Meta API base URL.
	DefaultAPIBaseURL = "https://api.meta.ai/v1"
	// AuthHost is the Meta account OAuth authorization server.
	AuthHost = "https://auth.meta.com"
	// DeviceAuthorizationEndpoint is the device authorization request endpoint.
	DeviceAuthorizationEndpoint = AuthHost + "/oidc/device/authorization/"
	// TokenEndpoint is the token issuance and polling endpoint.
	TokenEndpoint = AuthHost + "/oidc/device/token/"
	// ClientID is Meta Muse CLI's official OAuth client ID.
	ClientID = "1031625952748946"
	// DeviceCodeGrantType is the RFC 8628 grant type parameter.
	DeviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// DefaultPollInterval is the default polling interval when unspecified.
	DefaultPollInterval = 5 * time.Second
	// MaxPollDuration is the upper bound for waiting on user authorization.
	MaxPollDuration   = 15 * time.Minute
	httpClientTimeout = 30 * time.Second
	museUserAgent     = "muse-code/1.0.2"
)

// DeviceCodeResponse represents Meta's device authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	TokenEndpoint           string `json:"-"`
}

// TokenData represents the token response from Meta's OAuth token endpoint.
type TokenData struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// MintedKeyResponse represents the API key response minted from https://api.meta.ai/muse-code/key.
type MintedKeyResponse struct {
	APIKey       string `json:"api_key"`
	BaseURL      string `json:"base_url"`
	UserEmail    string `json:"user_email"`
	UserFullName string `json:"user_full_name"`
}

// MetaAuthBundle packages the token data, minted key, and user metadata.
type MetaAuthBundle struct {
	TokenData *TokenData
	MintedKey *MintedKeyResponse
	Email     string
	Name      string
}

// MetaTokenStorage represents persisted Meta OAuth tokens.
type MetaTokenStorage struct {
	Type         string
	AuthKind     string
	AccessToken  string
	DCAToken     string
	APIKey       string
	TokenType    string
	ExpiresIn    int
	Expired      string
	DCAExpired   string
	DCAExpiresAt int64
	LastRefresh  string
	BaseURL      string
	Email        string
	Name         string
}

// MetaAuth manages Meta OAuth operations.
type MetaAuth struct {
	httpClient *http.Client
	mintURL    string
}

// NewMetaAuth creates a new MetaAuth service instance.
func NewMetaAuth(cfg *config.Config) *MetaAuth {
	return NewMetaAuthWithProxyURL(cfg, "")
}

// NewMetaAuthWithProxyURL creates a MetaAuth service instance with an optional proxy URL.
func NewMetaAuthWithProxyURL(cfg *config.Config, proxyURL string) *MetaAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &MetaAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{Timeout: httpClientTimeout}),
	}
}

// StartDeviceFlow initiates the device authorization flow with Meta.
func (a *MetaAuth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	if a == nil || a.httpClient == nil {
		return nil, fmt.Errorf("meta: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	form := url.Values{
		"client_id": {ClientID},
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if errRequest != nil {
		return nil, fmt.Errorf("meta device flow: create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", museUserAgent)

	resp, errDo := a.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("meta device flow: request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("meta device flow: close response body error: %v", errClose)
		}
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("meta device flow: read response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta device flow failed (HTTP %d)", resp.StatusCode)
	}

	var dcr DeviceCodeResponse
	if errUnmarshal := json.Unmarshal(body, &dcr); errUnmarshal != nil {
		return nil, fmt.Errorf("meta device flow: parse response: %w", errUnmarshal)
	}
	if strings.TrimSpace(dcr.DeviceCode) == "" || strings.TrimSpace(dcr.UserCode) == "" {
		return nil, fmt.Errorf("meta device flow: response missing required device_code or user_code")
	}
	dcr.TokenEndpoint = TokenEndpoint
	return &dcr, nil
}

// WaitForAuthorization polls the token endpoint until the user approves the device code.
func (a *MetaAuth) WaitForAuthorization(ctx context.Context, dcr *DeviceCodeResponse) (*MetaAuthBundle, error) {
	if a == nil || a.httpClient == nil {
		return nil, fmt.Errorf("meta: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if dcr == nil || dcr.DeviceCode == "" {
		return nil, fmt.Errorf("meta auth: missing device code response")
	}

	tokenEndpoint := dcr.TokenEndpoint
	if tokenEndpoint == "" {
		tokenEndpoint = TokenEndpoint
	}

	interval := time.Duration(dcr.Interval) * time.Second
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	maxDuration := MaxPollDuration
	if dcr.ExpiresIn > 0 {
		expiresDuration := time.Duration(dcr.ExpiresIn) * time.Second
		if expiresDuration < maxDuration {
			maxDuration = expiresDuration
		}
	}

	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("meta auth: authorization timed out or canceled: %w", ctx.Err())
		case <-ticker.C:
			form := url.Values{
				"grant_type":  {DeviceCodeGrantType},
				"device_code": {dcr.DeviceCode},
				"client_id":   {ClientID},
			}
			req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
			if errRequest != nil {
				return nil, fmt.Errorf("meta auth: create token request: %w", errRequest)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", museUserAgent)

			resp, errDo := a.httpClient.Do(req)
			if errDo != nil {
				log.Warnf("meta auth: poll request error: %v (retrying)", errDo)
				continue
			}

			body, errRead := io.ReadAll(resp.Body)
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("meta auth: close token response body error: %v", errClose)
			}
			if errRead != nil {
				log.Warnf("meta auth: read token response error: %v (retrying)", errRead)
				continue
			}

			if resp.StatusCode == http.StatusOK {
				var tokenData TokenData
				if errUnmarshal := json.Unmarshal(body, &tokenData); errUnmarshal != nil {
					return nil, fmt.Errorf("meta auth: parse token response: %w", errUnmarshal)
				}
				if tokenData.AccessToken == "" {
					return nil, fmt.Errorf("meta auth: response missing access_token")
				}
				if tokenData.ExpiresIn > 0 {
					tokenData.ExpiresAt = time.Now().Add(time.Duration(tokenData.ExpiresIn) * time.Second).Unix()
				}
				bundle := &MetaAuthBundle{TokenData: &tokenData}
				minted, errMint := a.MintAPIKey(ctx, tokenData.AccessToken)
				if errMint != nil {
					log.Warnf("meta auth: could not mint api_key from dca_token: %v", errMint)
				} else if minted != nil {
					bundle.MintedKey = minted
					if minted.UserEmail != "" {
						bundle.Email = minted.UserEmail
					}
					if minted.UserFullName != "" {
						bundle.Name = minted.UserFullName
					}
				}
				return bundle, nil
			}

			var errResp TokenData
			_ = json.Unmarshal(body, &errResp)
			switch errResp.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				ticker.Reset(interval)
				continue
			case "access_denied":
				return nil, fmt.Errorf("meta auth: access was denied by user")
			case "expired_token":
				return nil, fmt.Errorf("meta auth: device code has expired")
			default:
				if errResp.Error != "" {
					return nil, fmt.Errorf("meta auth: error from authorization server: %s", errResp.Error)
				}
				log.Warnf("meta auth: unexpected response %d", resp.StatusCode)
			}
		}
	}
}

// MintAPIKey exchanges a Device Client Access token for an LLM API key.
func (a *MetaAuth) MintAPIKey(ctx context.Context, dcaToken string) (*MintedKeyResponse, error) {
	if a == nil || a.httpClient == nil {
		return nil, fmt.Errorf("meta: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dcaToken = strings.TrimSpace(dcaToken)
	if dcaToken == "" {
		return nil, fmt.Errorf("meta auth: missing dca token")
	}

	reqBody, errMarshal := json.Marshal(map[string]string{"dca_token": dcaToken})
	if errMarshal != nil {
		return nil, fmt.Errorf("meta auth: marshal mint request: %w", errMarshal)
	}

	mintURL := a.mintURL
	if mintURL == "" {
		if envMint := strings.TrimSpace(os.Getenv("META_MINT_URL")); envMint != "" {
			mintURL = envMint
		} else {
			mintURL = "https://api.meta.ai/muse-code/key"
		}
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, mintURL, bytes.NewReader(reqBody))
	if errRequest != nil {
		return nil, fmt.Errorf("meta auth: create mint request: %w", errRequest)
	}
	req.Header.Set("Authorization", "Bearer "+dcaToken)
	req.Header.Set("User-Agent", museUserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, errDo := a.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("meta auth: mint request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("meta auth: close mint response body error: %v", errClose)
		}
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("meta auth: read mint response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta auth: mint key failed (HTTP %d)", resp.StatusCode)
	}

	var minted MintedKeyResponse
	if errUnmarshal := json.Unmarshal(body, &minted); errUnmarshal != nil {
		return nil, fmt.Errorf("meta auth: parse mint response: %w", errUnmarshal)
	}
	if strings.TrimSpace(minted.APIKey) == "" {
		return nil, fmt.Errorf("meta auth: mint response missing api_key")
	}
	return &minted, nil
}

// CreateTokenStorage creates a serializable token storage record from the auth bundle.
func (a *MetaAuth) CreateTokenStorage(bundle *MetaAuthBundle) *MetaTokenStorage {
	if bundle == nil || bundle.TokenData == nil {
		return nil
	}
	dcaExpired := ""
	if bundle.TokenData.ExpiresAt > 0 {
		dcaExpired = time.Unix(bundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	apiKey := ""
	baseURL := DefaultAPIBaseURL
	email := bundle.Email
	name := bundle.Name
	if bundle.MintedKey != nil {
		apiKey = bundle.MintedKey.APIKey
		if mintedURL := strings.TrimSpace(bundle.MintedKey.BaseURL); mintedURL != "" {
			baseURL = mintedURL
		}
		if bundle.MintedKey.UserEmail != "" {
			email = bundle.MintedKey.UserEmail
		}
		if bundle.MintedKey.UserFullName != "" {
			name = bundle.MintedKey.UserFullName
		}
	}

	accessToken := bundle.TokenData.AccessToken
	expired := ""
	if apiKey != "" {
		accessToken = apiKey
	} else {
		expired = dcaExpired
	}

	return &MetaTokenStorage{
		Type:         "meta",
		AuthKind:     "oauth",
		AccessToken:  accessToken,
		DCAToken:     bundle.TokenData.AccessToken,
		APIKey:       apiKey,
		TokenType:    bundle.TokenData.TokenType,
		ExpiresIn:    bundle.TokenData.ExpiresIn,
		Expired:      expired,
		DCAExpired:   dcaExpired,
		DCAExpiresAt: bundle.TokenData.ExpiresAt,
		LastRefresh:  time.Now().UTC().Format(time.RFC3339),
		BaseURL:      baseURL,
		Email:        email,
		Name:         name,
	}
}

// BuildOAuthMetadata builds the persisted Meta OAuth credential metadata.
func BuildOAuthMetadata(storage *MetaTokenStorage) map[string]any {
	if storage == nil {
		return nil
	}
	metadata := map[string]any{
		"type":         "meta",
		"access_token": storage.AccessToken,
		"token_type":   storage.TokenType,
		"expires_in":   storage.ExpiresIn,
		"expired":      storage.Expired,
		"last_refresh": storage.LastRefresh,
		"base_url":     storage.BaseURL,
		"auth_kind":    "oauth",
	}
	if storage.DCAExpired != "" {
		metadata["dca_expired"] = storage.DCAExpired
	}
	if storage.DCAExpiresAt > 0 {
		metadata["dca_expires_at"] = storage.DCAExpiresAt
	}
	if storage.APIKey != "" {
		metadata["api_key"] = storage.APIKey
	}
	if storage.DCAToken != "" {
		metadata["dca_token"] = storage.DCAToken
	}
	if storage.Email != "" {
		metadata["email"] = storage.Email
	}
	if storage.Name != "" {
		metadata["name"] = storage.Name
	}
	return metadata
}

// CredentialFileName keeps a readable account name and hashes the original identity
// so distinct emails that sanitize identically cannot overwrite each other.
func CredentialFileName(email, sub string) string {
	clean := strings.TrimSpace(email)
	if clean != "" {
		sanitized := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, clean)
		if len(sanitized) > 120 {
			sanitized = sanitized[:120]
		}
		hash := sha256.Sum256([]byte(clean))
		return fmt.Sprintf("meta-%s-%s.json", sanitized, hex.EncodeToString(hash[:8]))
	}
	cleanSub := strings.TrimSpace(sub)
	if cleanSub != "" {
		hash := sha256.Sum256([]byte(cleanSub))
		return fmt.Sprintf("meta-%s.json", hex.EncodeToString(hash[:8]))
	}
	return "meta-oauth.json"
}
