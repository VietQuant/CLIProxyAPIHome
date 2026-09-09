package cluster

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Legacy compatibility: knownSessionPrefixes lists historical protocol and scheduler prefix wrappers
// emitted by older CPA nodes or legacy client harnesses.
// TODO(session-cleanup): Deprecate and remove this prefix stripping once all upstream CPA nodes and ingress
// pipelines natively emit canonical UUIDv8 at the protocol ingress boundary.
var knownSessionPrefixes = []string{
	"lcp:v1:", "lcp:",
	"ctx:v1:", "ctx:",
	"codex:", "claude:", "header:", "session:",
	"affinity:", "slot:", "task:", "conv:",
	"thread:", "clientreq:", "geminicache:",
	"pck:", "user:", "execution:", "agy:", "derived:",
}

// NormalizeToCanonicalUUID deterministically normalizes any session identifier to a
// canonical 36-character lowercase UUID (RFC 4122 / RFC 9562 compliant):
//  1. If rawID (or rawID without known protocol prefix) is already a standard UUID,
//     it strips the prefix and returns the lowercase UUID.
//  2. If rawID has a known protocol prefix but the remainder is not a UUID (e.g. custom task strings),
//     the known prefix is stripped and the remaining identifier is deterministically projected.
//  3. If rawID is not a standard UUID (e.g. LCP 64-hex hash, subagent hierarchy, or custom string),
//     it deterministically projects it to an RFC 9562 UUIDv8 using SHA-256 with domain separation.
//  4. Guard: pure-prefix inputs (e.g. "slot:", "task:") return "" to prevent collision with empty salt.
//  5. Idempotent: NormalizeToCanonicalUUID(NormalizeToCanonicalUUID(x)) == NormalizeToCanonicalUUID(x).
func NormalizeToCanonicalUUID(rawID string) string {
	clean := strings.TrimSpace(rawID)
	if clean == "" {
		return ""
	}

	// 1. Direct UUID match (already clean UUID)
	if canonicalUUIDPattern.MatchString(clean) {
		return strings.ToLower(clean)
	}

	// 2. Strip known protocol prefixes iteratively to unwrap layered prefixes (e.g., "derived:ctx:v1:...").
	for {
		stripped := false
		for _, p := range knownSessionPrefixes {
			if strings.HasPrefix(clean, p) {
				clean = strings.TrimPrefix(clean, p)
				clean = strings.TrimSpace(clean)
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	if clean == "" {
		return ""
	}
	if canonicalUUIDPattern.MatchString(clean) {
		return strings.ToLower(clean)
	}

	// 3. Check if any generic prefix "prefix:<uuid>" exists (using LastIndex to unwrap nested namespaces)
	if idx := strings.LastIndex(clean, ":"); idx > 0 {
		candidate := strings.TrimSpace(clean[idx+1:])
		if canonicalUUIDPattern.MatchString(candidate) {
			return strings.ToLower(candidate)
		}
	}

	// 4. Deterministic projection to RFC 9562 UUIDv8 for LCP hashes and arbitrary non-UUID strings
	sum := sha256.Sum256([]byte("cpa:canonical-uuid:v1\x00" + clean))
	u := [16]byte(sum[:16])
	u[6] = (u[6] & 0x0f) | 0x80 // RFC 9562 Version 8
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 / RFC 9562 Variant

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// SessionQueryCandidates returns the set of distinct search terms for a given session query parameter.
// Legacy compatibility: It returns both the trimmed raw identifier and its canonical UUID form (if different)
// to bridge legacy non-normalized database rows with modern canonical UUIDs during the migration transition.
// TODO(session-cleanup): Deprecate and remove this dual-candidate expansion once historical database records
// are backfilled/purged and all queries exclusively use canonical UUIDv8.
func SessionQueryCandidates(rawID string) []string {
	clean := strings.TrimSpace(rawID)
	if clean == "" {
		return nil
	}
	canonical := NormalizeToCanonicalUUID(clean)
	if canonical == "" || canonical == clean {
		return []string{clean}
	}
	return []string{clean, canonical}
}
