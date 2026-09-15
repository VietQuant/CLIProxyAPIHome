// Package devin provides OAuth helpers for Devin / Cognition credentials.
package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// DefaultAppBaseURL is the user-facing web app for Devin OAuth.
	DefaultAppBaseURL = "https://app.devin.ai"
	// DefaultAPIBaseURL is the API backend for token exchange and user status.
	DefaultAPIBaseURL = "https://api.devin.ai"
	// DefaultServerURL is the upstream Codeium/Devin reasoning backend.
	DefaultServerURL = "https://server.codeium.com"
	// RedirectHost is the loopback host used by Devin OAuth.
	RedirectHost = "127.0.0.1"
	// CallbackPort is the preferred loopback callback port.
	CallbackPort = 57121
	// RedirectPath is the loopback callback path used by Devin OAuth.
	RedirectPath = "/callback"

	devinTokenPrefix  = "devin-session-token$"
	devinHTTPTimeout  = 30 * time.Second
	devinMaxBodyBytes = 1 << 20
)

// DevinAuthService coordinates Devin PKCE authorization and token exchange.
type DevinAuthService struct {
	client     *http.Client
	appBaseURL string
	apiBaseURL string
}

// NewDevinAuthService creates a Devin authentication helper using config proxy settings.
func NewDevinAuthService(cfg *config.Config) *DevinAuthService {
	return NewDevinAuthServiceWithProxyURL(cfg, "")
}

// NewDevinAuthServiceWithProxyURL creates a Devin authentication helper with an explicit proxy URL.
func NewDevinAuthServiceWithProxyURL(cfg *config.Config, proxyURL string) *DevinAuthService {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &DevinAuthService{
		client:     util.SetProxy(&sdkCfg, &http.Client{Timeout: devinHTTPTimeout}),
		appBaseURL: DefaultAppBaseURL,
		apiBaseURL: DefaultAPIBaseURL,
	}
}

// BuildAuthorizationURL constructs the PKCE login URL.
func (s *DevinAuthService) BuildAuthorizationURL(redirectURI, codeChallenge, state string) string {
	appBase := DefaultAppBaseURL
	if s != nil && strings.TrimSpace(s.appBaseURL) != "" {
		appBase = s.appBaseURL
	}
	trimmedRedirect := strings.TrimSpace(redirectURI)
	var queryParts []string
	if trimmedRedirect != "" {
		queryParts = append(queryParts, "redirect_uri="+url.QueryEscape(trimmedRedirect))
	}
	if state != "" {
		queryParts = append(queryParts, "state="+url.QueryEscape(state))
	}
	queryParts = append(queryParts,
		"prompt=select_account",
		"code_challenge="+url.QueryEscape(codeChallenge),
		"code_challenge_method=S256",
	)
	if trimmedRedirect == "" {
		queryParts = append(queryParts, "cli_pkce_marker=1")
	}
	return fmt.Sprintf("%s/auth/cli/continue?%s", strings.TrimRight(appBase, "/"), strings.Join(queryParts, "&"))
}

// ExchangeCodeForToken exchanges the authorization code for a session token.
func (s *DevinAuthService) ExchangeCodeForToken(ctx context.Context, code, codeVerifier string) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("devin: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reqBody := map[string]string{
		"code":          strings.TrimSpace(code),
		"code_verifier": strings.TrimSpace(codeVerifier),
	}
	jsonBody, errMarshal := json.Marshal(reqBody)
	if errMarshal != nil {
		return "", errMarshal
	}

	endpoint := fmt.Sprintf("%s/auth/cli/token", strings.TrimRight(s.apiBaseURL, "/"))
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBody))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, errDo := s.client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("devin token exchange failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("devin token exchange: response body close error: %v", errClose)
		}
	}()

	respBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, devinMaxBodyBytes))
	if errRead != nil {
		return "", fmt.Errorf("read token exchange response: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token exchange failed with status %d", resp.StatusCode)
	}
	token := strings.TrimSpace(gjson.GetBytes(respBytes, "token").String())
	if token == "" {
		return "", fmt.Errorf("response did not contain a valid token")
	}
	return token, nil
}

// FetchSelfProfile retrieves the authenticated user's profile from api.devin.ai/v3/self.
func (s *DevinAuthService) FetchSelfProfile(ctx context.Context, sessionToken string) (userName, userID, orgID string, err error) {
	if s == nil || s.client == nil {
		return "", "", "", fmt.Errorf("devin: client not ready")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := fmt.Sprintf("%s/v3/self", strings.TrimRight(s.apiBaseURL, "/"))
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errReq != nil {
		return "", "", "", errReq
	}
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	req.Header.Set("Accept", "application/json")

	resp, errDo := s.client.Do(req)
	if errDo != nil {
		return "", "", "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("devin self profile: response body close error: %v", errClose)
		}
	}()

	respBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, devinMaxBodyBytes))
	if errRead != nil {
		return "", "", "", errRead
	}
	if resp.StatusCode == http.StatusOK {
		root := gjson.ParseBytes(respBytes)
		userName = strings.TrimSpace(root.Get("user_name").String())
		userID = strings.TrimSpace(root.Get("user_id").String())
		orgID = strings.TrimSpace(root.Get("org_id").String())
	}
	return userName, userID, orgID, nil
}

// FormatSessionToken ensures the token carries the mandatory devin-session-token$ prefix.
func FormatSessionToken(rawToken string) string {
	t := strings.TrimSpace(rawToken)
	if strings.HasPrefix(t, devinTokenPrefix) {
		return t
	}
	if strings.HasPrefix(t, "eyJ") {
		return devinTokenPrefix + t
	}
	return t
}

// DefaultRedirectURI returns the loopback callback URL used by Home OAuth.
func DefaultRedirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", RedirectHost, CallbackPort, RedirectPath)
}
