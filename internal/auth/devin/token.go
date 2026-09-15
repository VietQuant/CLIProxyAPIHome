package devin

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// CredentialFileName returns the filename used for Devin credentials.
func CredentialFileName(userName, userID string) string {
	identifier := strings.TrimSpace(userName)
	if identifier == "" {
		identifier = strings.TrimSpace(userID)
	}
	if identifier == "" {
		return fmt.Sprintf("devin-%d.json", time.Now().UnixMilli())
	}
	fileIdentifier := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '@' {
			return r
		}
		return '_'
	}, identifier)
	if fileIdentifier != identifier || len(fileIdentifier) > 160 {
		digest := sha256.Sum256([]byte(identifier))
		fileIdentifier = fmt.Sprintf("user-%x", digest[:8])
	}
	return fmt.Sprintf("devin-%s.json", fileIdentifier)
}

// BuildOAuthMetadata builds the persisted Devin OAuth credential metadata.
func BuildOAuthMetadata(sessionToken, userName, userID, orgID string) map[string]any {
	sessionToken = FormatSessionToken(sessionToken)
	metadata := map[string]any{
		"type":          "devin",
		"api_key":       sessionToken,
		"session_token": sessionToken,
		"auth_kind":     "oauth",
		"base_url":      DefaultServerURL,
	}
	if userName != "" {
		metadata["user_name"] = userName
	}
	if userID != "" {
		metadata["user_id"] = userID
	}
	if orgID != "" {
		metadata["org_id"] = orgID
	}
	return metadata
}
