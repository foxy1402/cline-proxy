package app

import (
	"testing"

	utls "github.com/refraction-networking/utls"
)

// CLI v2.1.4 ClientHello, measured 2026-10-26 via a clean-proxy bridge.
// Regression test: the spec must not drift back to the v1 shape, because
// upstream now 429s non-CLI TLS stacks with the same IP/account/body.
func TestBunSpecMatchesCLIV2(t *testing.T) {
	spec := bunSpecForConn()
	// Curves: MLKEM768 first, then X25519, P256, P384.
	wantCurves := []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}
	var curves []utls.CurveID
	var keyShares []utls.CurveID
	var sigAlgs []utls.SignatureScheme
	hasPadding := false
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.SupportedCurvesExtension:
			curves = e.Curves
		case *utls.KeyShareExtension:
			for _, ks := range e.KeyShares {
				keyShares = append(keyShares, ks.Group)
			}
		case *utls.SignatureAlgorithmsExtension:
			sigAlgs = e.SupportedSignatureAlgorithms
		case *utls.UtlsPaddingExtension:
			hasPadding = true
		}
	}
	if len(curves) != len(wantCurves) {
		t.Fatalf("curves = %v, want %v", curves, wantCurves)
	}
	for i := range wantCurves {
		if curves[i] != wantCurves[i] {
			t.Fatalf("curves[%d] = %v, want %v", i, curves[i], wantCurves[i])
		}
	}
	if len(keyShares) != 2 || keyShares[0] != utls.X25519MLKEM768 || keyShares[1] != utls.X25519 {
		t.Fatalf("keyShares = %v, want [X25519MLKEM768 X25519]", keyShares)
	}
	wantSig := []utls.SignatureScheme{1027, 1283, 1539, 2055, 2056, 2057, 2058, 2059, 2052, 2053, 2054, 1025, 1281, 1537, 771, 769, 770, 1026, 1282, 1538}
	if len(sigAlgs) != len(wantSig) {
		t.Fatalf("sigAlgs len = %d, want %d (%v)", len(sigAlgs), len(wantSig), sigAlgs)
	}
	for i := range wantSig {
		if sigAlgs[i] != wantSig[i] {
			t.Fatalf("sigAlgs[%d] = %d, want %d", i, sigAlgs[i], wantSig[i])
		}
	}
	if hasPadding {
		t.Fatal("CLI v2 sends no Padding extension; spec must not include one")
	}
	// Gate tools must be the v2 12-tool set, not the v1 11-tool set.
	defs := zenGateToolDefs()
	if len(defs) != 12 {
		t.Fatalf("gate tool defs = %d, want 12", len(defs))
	}
	wantNames := []string{"edit", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "execute"}
	for i, n := range wantNames {
		if defs[i].Name != n {
			t.Fatalf("gate tool[%d] = %q, want %q", i, defs[i].Name, n)
		}
		if defs[i].ParametersChat == nil {
			t.Fatalf("gate tool %q has no chat parameters", n)
		}
	}
	if zenNativeUA != "opencode/latest/2.1.4/cli" {
		t.Fatalf("zenNativeUA = %q, want CLI v2 UA", zenNativeUA)
	}
}
