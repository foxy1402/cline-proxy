package app

import (
	"testing"

	"cline-go-proxy/internal/kit"
)

// 2026-10-27: upstream calls must use a fresh session per API call.
// A sticky session reused across unrelated history-less requests gets
// flagged to midnight UTC after the first call passes, while a fresh
// session per CLI run does not (cli-multi-observation-2026-10-27.md).
// This test pins the per-call identity contract, independent of the
// legacy sticky store (which is kept only for admin/rotator/tests).
func TestZenCallIdentityFreshPerCall(t *testing.T) {
	// Same call, same key: same session (retries of one call share it),
	// fresh request ID per attempt.
	call1 := newZenCallIdentity()
	s1, r1, ua1 := call1.forKey("key-a")
	s1b, r1b, ua1b := call1.forKey("key-a")
	if s1 != s1b {
		t.Fatalf("same call, same key: session changed %s -> %s", s1, s1b)
	}
	if ua1 != ua1b {
		t.Fatalf("same call, same key: UA changed %s -> %s", ua1, ua1b)
	}
	if r1 == r1b {
		t.Fatalf("same call: request ID should be fresh per attempt, got same %s", r1)
	}
	if !kit.ValidZenSessionID(s1) {
		t.Fatalf("fresh session %q fails format gate", s1)
	}
	if ua1 != zenNativeUA {
		t.Fatalf("UA = %q, want CLI v2 UA %q", ua1, zenNativeUA)
	}

	// A different API call (new identity object) with the same key:
	// a different session. This is the IDE/agent case: Cursor, ZCode,
	// OpenClaw, Hermes send full history themselves, so upstream needs
	// no session continuity — and must not see a reused session.
	call2 := newZenCallIdentity()
	s2, _, _ := call2.forKey("key-a")
	if s2 == s1 {
		t.Fatalf("new API call, same key: session reused %s", s1)
	}

	// A different key in the same call: a different session.
	sOther, _, _ := call1.forKey("key-b")
	if sOther == s1 {
		t.Fatalf("different key in same call: session reused %s", s1)
	}

	// 403 refresh within a call: next attempt mints a fresh session.
	call1.refresh("key-a")
	s3, _, _ := call1.forKey("key-a")
	if s3 == s1 {
		t.Fatalf("after refresh: session reused %s", s1)
	}
	if !kit.ValidZenSessionID(s3) {
		t.Fatalf("refreshed session %q fails format gate", s3)
	}
}
