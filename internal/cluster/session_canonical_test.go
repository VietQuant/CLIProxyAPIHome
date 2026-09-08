package cluster

import (
	"testing"
)

func TestNormalizeToCanonicalUUID(t *testing.T) {
	t.Parallel()

	// 1. Empty and whitespace
	if got := NormalizeToCanonicalUUID(""); got != "" {
		t.Fatalf("NormalizeToCanonicalUUID(\"\") = %q, want empty", got)
	}
	if got := NormalizeToCanonicalUUID("   "); got != "" {
		t.Fatalf("NormalizeToCanonicalUUID(\"   \") = %q, want empty", got)
	}

	// 2. Pure prefix guard (P1-1 bug fix verification)
	for _, purePrefix := range []string{"slot:", "task:", "lcp:", "lcp:v1:", "ctx:v1:", "codex:", "claude:", "header:"} {
		if got := NormalizeToCanonicalUUID(purePrefix); got != "" {
			t.Errorf("NormalizeToCanonicalUUID(%q) = %q, want empty (pure prefix guard)", purePrefix, got)
		}
	}

	// 3. Native UUIDs (v4 and v7, various casings)
	rawUUIDv4 := "b2839f64-668d-4dc3-a42a-64da829d1e33"
	if got := NormalizeToCanonicalUUID(rawUUIDv4); got != rawUUIDv4 {
		t.Fatalf("NormalizeToCanonicalUUID(rawUUIDv4) = %q, want %q", got, rawUUIDv4)
	}
	upperUUID := "B2839F64-668D-4DC3-A42A-64DA829D1E33"
	if got := NormalizeToCanonicalUUID(upperUUID); got != rawUUIDv4 {
		t.Fatalf("NormalizeToCanonicalUUID(upperUUID) = %q, want %q", got, rawUUIDv4)
	}
	rawUUIDv7 := "01a07e72-c84d-7fd3-8207-d217b41cc649"
	if got := NormalizeToCanonicalUUID(rawUUIDv7); got != rawUUIDv7 {
		t.Fatalf("NormalizeToCanonicalUUID(rawUUIDv7) = %q, want %q", got, rawUUIDv7)
	}

	// 4. Known prefixes with UUIDs
	prefixedCases := map[string]string{
		"codex:01a07e72-c84d-7fd3-8207-d217b41cc649":         "01a07e72-c84d-7fd3-8207-d217b41cc649",
		"claude:b2839f64-668d-4dc3-a42a-64da829d1e33":        "b2839f64-668d-4dc3-a42a-64da829d1e33",
		"header:7a8b9c0d-1111-2222-3333-444455556666":        "7a8b9c0d-1111-2222-3333-444455556666",
		"session:b2839f64-668d-4dc3-a42a-64da829d1e33":       "b2839f64-668d-4dc3-a42a-64da829d1e33",
		"thread:01a07e72-c84d-7fd3-8207-d217b41cc649":        "01a07e72-c84d-7fd3-8207-d217b41cc649",
		"custom-prefix:01a07e72-c84d-7fd3-8207-d217b41cc649": "01a07e72-c84d-7fd3-8207-d217b41cc649",
	}
	for input, want := range prefixedCases {
		got := NormalizeToCanonicalUUID(input)
		if got != want {
			t.Errorf("NormalizeToCanonicalUUID(%q) = %q, want %q", input, got, want)
		}
	}

	// 5. LCP 64-hex and non-UUID inputs projected to RFC 9562 UUIDv8
	nonUUIDCases := []string{
		"lcp:v1:c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985",
		"ctx:v1:c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985",
		"c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985",
		"slot:pi-worker-1",
		"task:deploy-abc",
		"ses_f8189891effeCLIq0MasUgMQsC",
	}
	for _, input := range nonUUIDCases {
		got := NormalizeToCanonicalUUID(input)
		if len(got) != 36 {
			t.Errorf("NormalizeToCanonicalUUID(%q) length = %d, want 36", input, len(got))
		}
		if !canonicalUUIDPattern.MatchString(got) {
			t.Errorf("NormalizeToCanonicalUUID(%q) = %q, does not match UUID pattern", input, got)
		}
		// RFC 9562 Version 8
		if got[14] != '8' {
			t.Errorf("NormalizeToCanonicalUUID(%q) = %q, version char is %c, want '8'", input, got, got[14])
		}
		// RFC 4122 Variant
		variantChar := got[19]
		if variantChar != '8' && variantChar != '9' && variantChar != 'a' && variantChar != 'b' {
			t.Errorf("NormalizeToCanonicalUUID(%q) = %q, variant char is %c, want 8/9/a/b", input, got, variantChar)
		}
		// Idempotency
		if reGot := NormalizeToCanonicalUUID(got); reGot != got {
			t.Errorf("NormalizeToCanonicalUUID is not idempotent: first=%q, second=%q", got, reGot)
		}
	}

	// 6. Same content with lcp:v1:, ctx:v1:, or bare produces identical UUID
	lcpPrefixed := "lcp:v1:c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985"
	ctxPrefixed := "ctx:v1:c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985"
	lcpBare := "c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985"
	normLCP := NormalizeToCanonicalUUID(lcpPrefixed)
	normCTX := NormalizeToCanonicalUUID(ctxPrefixed)
	normBare := NormalizeToCanonicalUUID(lcpBare)
	if normLCP != normCTX || normLCP != normBare {
		t.Fatalf("lcp (%q), ctx (%q), and bare (%q) produced divergent UUIDs", normLCP, normCTX, normBare)
	}

	// 7. Multi-layered nested prefixes iteratively stripped
	nestedPrefixed := "derived:ctx:v1:lcp:v1:c28621bab78eacdb3ae128c0f6aaa0147842f063fda10ae9dc5473cc81d58985"
	if got := NormalizeToCanonicalUUID(nestedPrefixed); got != normBare {
		t.Fatalf("iteratively stripped nested prefixes (%q) produced %q, want %q", nestedPrefixed, got, normBare)
	}
}

func TestSessionQueryCandidates(t *testing.T) {
	t.Parallel()

	// 1. Empty and whitespace
	if got := SessionQueryCandidates(""); got != nil {
		t.Fatalf("SessionQueryCandidates(\"\") = %v, want nil", got)
	}
	if got := SessionQueryCandidates("   "); got != nil {
		t.Fatalf("SessionQueryCandidates(\"   \") = %v, want nil", got)
	}

	// 2. Pure clean UUID -> single candidate
	cleanUUID := "01a07e72-c84d-7fd3-8207-d217b41cc649"
	gotClean := SessionQueryCandidates(cleanUUID)
	if len(gotClean) != 1 || gotClean[0] != cleanUUID {
		t.Fatalf("SessionQueryCandidates(cleanUUID) = %v, want [%s]", gotClean, cleanUUID)
	}

	// 3. Prefixed UUID -> raw + canonical
	prefixedUUID := "codex:01a07e72-c84d-7fd3-8207-d217b41cc649"
	gotPrefixed := SessionQueryCandidates(prefixedUUID)
	if len(gotPrefixed) != 2 || gotPrefixed[0] != prefixedUUID || gotPrefixed[1] != cleanUUID {
		t.Fatalf("SessionQueryCandidates(prefixedUUID) = %v, want [%s, %s]", gotPrefixed, prefixedUUID, cleanUUID)
	}

	// 4. Non-UUID task name -> raw + UUIDv8
	taskName := "slot:pi-worker-1"
	wantUUIDv8 := NormalizeToCanonicalUUID(taskName)
	gotTask := SessionQueryCandidates(taskName)
	if len(gotTask) != 2 || gotTask[0] != taskName || gotTask[1] != wantUUIDv8 {
		t.Fatalf("SessionQueryCandidates(taskName) = %v, want [%s, %s]", gotTask, taskName, wantUUIDv8)
	}
}
