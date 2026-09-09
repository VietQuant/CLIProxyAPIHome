package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestExtractSessionIDsHierarchy(t *testing.T) {
	tests := []struct {
		name         string
		headers      http.Header
		payload      []byte
		wantPrimary  string
		wantFallback string
	}{
		{
			name: "X-Session-ID with X-Parent-Session-ID",
			headers: http.Header{
				"X-Session-ID":        []string{"child-1"},
				"X-Parent-Session-ID": []string{"parent-1"},
			},
			wantPrimary:  "header:child-1",
			wantFallback: "header:parent-1",
		},
		{
			name: "X-Session-ID with already prefixed IDs from CPA",
			headers: http.Header{
				"X-Session-ID":        []string{"slot:pi-sub1"},
				"X-Parent-Session-ID": []string{"slot:pi-main"},
			},
			wantPrimary:  "slot:pi-sub1",
			wantFallback: "slot:pi-main",
		},
		{
			name: "Codex Session-Id with Parent Thread",
			headers: http.Header{
				"Session-Id":               []string{"thread-child"},
				"x-codex-parent-thread-id": []string{"thread-parent"},
			},
			wantPrimary:  "codex:thread-child",
			wantFallback: "codex:thread-parent",
		},
		{
			name: "Claude Code headers with subagent",
			headers: http.Header{
				"X-Claude-Code-Session-Id":      []string{"session-uuid"},
				"X-Claude-Code-Agent-Id":        []string{"subagent-uuid"},
				"X-Claude-Code-Parent-Agent-Id": []string{"main"},
			},
			wantPrimary:  "claude:session-uuid:agent:subagent-uuid",
			wantFallback: "claude:session-uuid",
		},
		{
			name: "Claude Code metadata.user_id json format with parent_session_id",
			payload: []byte(`{
				"metadata": {
					"user_id": "{\"session_id\":\"session-123\",\"parent_session_id\":\"parent-456\",\"agent_id\":\"sub-1\"}"
				}
			}`),
			wantPrimary:  "claude:session-123:agent:sub-1",
			wantFallback: "claude:parent-456",
		},
		{
			name: "Claude Code metadata.user_id json format with agent only",
			payload: []byte(`{
				"metadata": {
					"user_id": "{\"session_id\":\"session-123\",\"agent_id\":\"sub-1\"}"
				}
			}`),
			wantPrimary:  "claude:session-123:agent:sub-1",
			wantFallback: "claude:session-123",
		},
		{
			name: "Payload root session_id and parent_session_id",
			payload: []byte(`{
				"session_id": "sub-task",
				"parent_session_id": "root-task"
			}`),
			wantPrimary:  "header:sub-task",
			wantFallback: "header:root-task",
		},
		{
			name: "Antigravity X-Http-Session-Id and X-Parent-Session-ID",
			headers: http.Header{
				"X-Http-Session-Id":   []string{"agy-sub"},
				"X-Parent-Session-ID": []string{"agy-main"},
			},
			wantPrimary:  "agy:agy-sub",
			wantFallback: "agy:agy-main",
		},
		{
			name: "Nested request payload",
			payload: []byte(`{
				"request": {
					"session_id": "nested-child",
					"parent_session_id": "nested-parent"
				}
			}`),
			wantPrimary:  "header:nested-child",
			wantFallback: "header:nested-parent",
		},
		{
			name: "Reject control characters and invalid explicit ID",
			headers: http.Header{
				"X-Session-ID":        []string{"bad\u0000id"},
				"X-Parent-Session-ID": []string{"valid-parent"},
			},
			wantPrimary:  "",
			wantFallback: "",
		},
		{
			name: "Trim whitespace and newlines around valid ID",
			headers: http.Header{
				"X-Session-ID":        []string{"\n  valid-child \r\n"},
				"X-Parent-Session-ID": []string{"\tvalid-parent\t"},
			},
			wantPrimary:  "header:valid-child",
			wantFallback: "header:valid-parent",
		},
		{
			name: "Avoid double prefix for user in payload",
			payload: []byte(`{
				"metadata": {
					"user_id": "user:already-prefixed-user"
				}
			}`),
			wantPrimary:  "user:already-prefixed-user",
			wantFallback: "",
		},
		{
			name: "Avoid double prefix for conv in payload",
			payload: []byte(`{
				"conversation_id": "conv:already-prefixed-conv"
			}`),
			wantPrimary:  "conv:already-prefixed-conv",
			wantFallback: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPrimary, gotFallback := extractSessionIDs(tc.headers, tc.payload, nil)
			if gotPrimary != tc.wantPrimary {
				t.Errorf("primary = %q, want %q", gotPrimary, tc.wantPrimary)
			}
			if gotFallback != tc.wantFallback {
				t.Errorf("fallback = %q, want %q", gotFallback, tc.wantFallback)
			}
		})
	}
}

func TestSessionAffinitySelectorParentInheritance(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)

	auth1 := &Auth{ID: "auth-1", Provider: "openai", Status: StatusActive}
	auth2 := &Auth{ID: "auth-2", Provider: "openai", Status: StatusActive}
	auths := []*Auth{auth1, auth2}

	// 1. Parent session makes a request and picks an auth
	parentOpts := Options{
		Headers: http.Header{
			"X-Session-ID": []string{"session-parent"},
		},
	}
	pickedParent, errParent := selector.Pick(context.Background(), "openai", "gpt-4", parentOpts, auths)
	if errParent != nil {
		t.Fatalf("Pick(parent) error = %v", errParent)
	}
	if pickedParent == nil {
		t.Fatal("Pick(parent) returned nil auth")
	}
	expectedAuthID := pickedParent.ID

	// 2. Child session makes a request with parent session
	childOpts := Options{
		Headers: http.Header{
			"X-Session-ID":        []string{"session-child"},
			"X-Parent-Session-ID": []string{"session-parent"},
		},
	}
	pickedChild, errChild := selector.Pick(context.Background(), "openai", "gpt-4", childOpts, auths)
	if errChild != nil {
		t.Fatalf("Pick(child) error = %v", errChild)
	}
	if pickedChild == nil || pickedChild.ID != expectedAuthID {
		t.Fatalf("Pick(child) = %v, want %s (inherited from parent)", pickedChild, expectedAuthID)
	}

	// 3. Child session makes subsequent request without parent header -> should hit child cache binding
	childSubsequentOpts := Options{
		Headers: http.Header{
			"X-Session-ID": []string{"session-child"},
		},
	}
	pickedChildSubsequent, errSubsequent := selector.Pick(context.Background(), "openai", "gpt-4", childSubsequentOpts, auths)
	if errSubsequent != nil {
		t.Fatalf("Pick(child subsequent) error = %v", errSubsequent)
	}
	if pickedChildSubsequent == nil || pickedChildSubsequent.ID != expectedAuthID {
		t.Fatalf("Pick(child subsequent) = %v, want %s", pickedChildSubsequent, expectedAuthID)
	}
}

func TestSessionAffinitySelectorParentUnavailableFallback(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)

	auth1 := &Auth{ID: "auth-1", Provider: "openai", Status: StatusActive}
	auth2 := &Auth{ID: "auth-2", Provider: "openai", Status: StatusActive}

	// Parent binds to auth1
	parentOpts := Options{
		Headers: http.Header{
			"X-Session-ID": []string{"session-parent"},
		},
	}
	pickedParent, errParent := selector.Pick(context.Background(), "openai", "gpt-4", parentOpts, []*Auth{auth1, auth2})
	if errParent != nil || pickedParent.ID != "auth-1" {
		t.Fatalf("Pick(parent) = %v, err = %v, want auth-1", pickedParent, errParent)
	}

	// auth1 is now disabled
	auth1Disabled := &Auth{ID: "auth-1", Provider: "openai", Status: StatusDisabled, Disabled: true}

	// Child session requests with parent=session-parent; parent's auth1 is unavailable, should fallback to auth2
	childOpts := Options{
		Headers: http.Header{
			"X-Session-ID":        []string{"session-child"},
			"X-Parent-Session-ID": []string{"session-parent"},
		},
	}
	pickedChild, errChild := selector.Pick(context.Background(), "openai", "gpt-4", childOpts, []*Auth{auth1Disabled, auth2})
	if errChild != nil || pickedChild.ID != "auth-2" {
		t.Fatalf("Pick(child fallback) = %v, err = %v, want auth-2", pickedChild, errChild)
	}

	// Verify child is now bound to auth2
	childSubsequentOpts := Options{
		Headers: http.Header{
			"X-Session-ID": []string{"session-child"},
		},
	}
	pickedSubsequent, errSub := selector.Pick(context.Background(), "openai", "gpt-4", childSubsequentOpts, []*Auth{auth1, auth2})
	if errSub != nil || pickedSubsequent.ID != "auth-2" {
		t.Fatalf("Pick(child subsequent) = %v, err = %v, want auth-2", pickedSubsequent, errSub)
	}
}

func TestFormatSessionIDPreservesSessionAndLCPPrefixes(t *testing.T) {
	tests := []struct {
		input  string
		prefix string
		want   string
	}{
		{"session:my-task-123", "header:", "session:my-task-123"},
		{"lcp:my-affinity-456", "header:", "lcp:my-affinity-456"},
		{"claude:root", "header:", "claude:root"},
		{"codex:thread-1", "header:", "codex:thread-1"},
		{"plain-session-id", "header:", "header:plain-session-id"},
		{"plain-session-id", "slot:", "slot:plain-session-id"},
	}

	for _, tt := range tests {
		got := formatSessionID(tt.input, tt.prefix)
		if got != tt.want {
			t.Errorf("formatSessionID(%q, %q) = %q, want %q", tt.input, tt.prefix, got, tt.want)
		}
	}
}

func TestNewSessionCacheHandlesDegenerateTTL(t *testing.T) {
	for _, degenerate := range []time.Duration{-time.Second, 0, time.Nanosecond, time.Millisecond} {
		cache := NewSessionCache(degenerate)
		if cache.ttl < time.Second {
			t.Errorf("NewSessionCache(%v).ttl = %v, want >= 1s", degenerate, cache.ttl)
		}
		cache.Set("test-key", "auth-1")
		if got, ok := cache.Get("test-key"); !ok || got != "auth-1" {
			t.Errorf("cache.Get(test-key) = (%q, %v), want (auth-1, true)", got, ok)
		}
		cache.Stop()
	}
}

func TestSessionCacheConcurrentStop(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Stop()
		}()
	}
	wg.Wait()
}

func TestSessionCacheGetTOCTOUProtection(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	t.Cleanup(cache.Stop)

	// 1. Set initial entry
	cache.Set("toctou-key", "auth-old")

	// 2. Artificially expire the entry in cache
	cache.mu.Lock()
	cache.entries["toctou-key"] = sessionEntry{
		authID:    "auth-old",
		expiresAt: time.Now().Add(-time.Minute),
	}
	cache.mu.Unlock()

	// 3. Update entry with a new binding (simulating concurrent write before write-lock acquisition in Get)
	cache.Set("toctou-key", "auth-new")
	val, ok := cache.Get("toctou-key")
	if !ok || val != "auth-new" {
		t.Fatalf("Get(toctou-key) = (%q, %v), want (auth-new, true)", val, ok)
	}

	// 4. Verify that truly expired entries are cleanly deleted by Get
	cache.mu.Lock()
	cache.entries["expired-key"] = sessionEntry{
		authID:    "auth-expired",
		expiresAt: time.Now().Add(-time.Minute),
	}
	cache.mu.Unlock()

	valExp, okExp := cache.Get("expired-key")
	if okExp || valExp != "" {
		t.Fatalf("Get(expired-key) = (%q, %v), want (\"\", false)", valExp, okExp)
	}
	cache.mu.RLock()
	_, stillExists := cache.entries["expired-key"]
	cache.mu.RUnlock()
	if stillExists {
		t.Fatal("expired-key still exists in cache entries map after Get detection")
	}
}
