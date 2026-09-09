package dynamic

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestRESPDispatchSessionHierarchyAffinity(t *testing.T) {
	rt, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	// Use SessionAffinitySelector with FillFirst fallback so initial selection is deterministic
	rt.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{},
	}))
	env := protocolOneDispatchEnv(rt, "session-hierarchy-test")

	// 1. First request with parent session ID
	parentReq := `{"type":"auth","model":"gpt","session_id":"slot:pi-main","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`
	parentReply := handleAuth(context.Background(), env, []string{"RPOP", "auth", parentReq})
	parentBulk := parentReply.BulkString
	if len(parentBulk) == 0 {
		t.Fatalf("parentReply.BulkString is empty")
	}
	parentAuthID := gjson.GetBytes(parentBulk, "auth.id").String()
	if parentAuthID != "cred-a" {
		t.Fatalf("parentAuthID = %q, want cred-a", parentAuthID)
	}

	// 2. Child request with session_id="slot:pi-sub1" and parent_session_id="slot:pi-main"
	childReq := `{"type":"auth","model":"gpt","session_id":"slot:pi-sub1","parent_session_id":"slot:pi-main","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`
	childReply := handleAuth(context.Background(), env, []string{"RPOP", "auth", childReq})
	childBulk := childReply.BulkString
	if len(childBulk) == 0 {
		t.Fatalf("childReply.BulkString is empty")
	}
	childAuthID := gjson.GetBytes(childBulk, "auth.id").String()
	if childAuthID != parentAuthID {
		t.Fatalf("childAuthID = %q, want %q (inherited from parent)", childAuthID, parentAuthID)
	}

	// 3. Child subsequent request with ONLY session_id="slot:pi-sub1" (no parent_session_id)
	childSubsequentReq := `{"type":"auth","model":"gpt","session_id":"slot:pi-sub1","concurrency_protocol":1,"headers":{"x-api-key":"dispatch-client-key"}}`
	childSubsequentReply := handleAuth(context.Background(), env, []string{"RPOP", "auth", childSubsequentReq})
	childSubsequentBulk := childSubsequentReply.BulkString
	if len(childSubsequentBulk) == 0 {
		t.Fatalf("childSubsequentReply.BulkString is empty")
	}
	subsequentAuthID := gjson.GetBytes(childSubsequentBulk, "auth.id").String()
	if subsequentAuthID != parentAuthID {
		t.Fatalf("subsequentAuthID = %q, want %q (from child binding cache)", subsequentAuthID, parentAuthID)
	}
}

func TestRESPDispatchHeaderPrecedenceForParentSession(t *testing.T) {
	rt, _ := newConcurrencyDispatchRuntime(t, []string{"cred-a", "cred-b"})
	rt.CoreManager().SetSelector(coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{},
	}))
	env := protocolOneDispatchEnv(rt, "session-header-test")

	// Parent bound via header
	parentReq := `{"type":"auth","model":"gpt","headers":{"x-api-key":"dispatch-client-key","X-Session-ID":"parent-custom"}}`
	parentReply := handleAuth(context.Background(), env, []string{"RPOP", "auth", parentReq})
	parentBulk := parentReply.BulkString
	parentAuthID := gjson.GetBytes(parentBulk, "auth.id").String()
	if parentAuthID != "cred-a" {
		t.Fatalf("parentAuthID = %q, want cred-a", parentAuthID)
	}

	// Child bound via parent_session_id in JSON payload
	childReq := `{"type":"auth","model":"gpt","session_id":"child-custom","parent_session_id":"parent-custom","headers":{"x-api-key":"dispatch-client-key"}}`
	childReply := handleAuth(context.Background(), env, []string{"RPOP", "auth", childReq})
	childBulk := childReply.BulkString
	childAuthID := gjson.GetBytes(childBulk, "auth.id").String()
	if childAuthID != parentAuthID {
		t.Fatalf("childAuthID = %q, want %q", childAuthID, parentAuthID)
	}
}
