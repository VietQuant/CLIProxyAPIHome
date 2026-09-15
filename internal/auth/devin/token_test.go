package devin

import "testing"

func TestFormatSessionToken(t *testing.T) {
	if got := FormatSessionToken("devin-session-token$abc"); got != "devin-session-token$abc" {
		t.Fatalf("prefixed token = %q", got)
	}
	if got := FormatSessionToken("eyJabc"); got != "devin-session-token$eyJabc" {
		t.Fatalf("jwt token = %q", got)
	}
}

func TestCredentialFileName(t *testing.T) {
	if got := CredentialFileName("user@example.com", ""); got != "devin-user@example.com.json" {
		t.Fatalf("email filename = %q", got)
	}
	if got := CredentialFileName("", "abc"); got != "devin-abc.json" {
		t.Fatalf("user id filename = %q", got)
	}
}
