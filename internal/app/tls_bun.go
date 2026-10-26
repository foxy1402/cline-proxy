package app

import (
	utls "github.com/refraction-networking/utls"
)

// bunSpecForConn returns a FRESH ClientHelloSpec per connection.
//
// The spec holds mutable per-handshake state: ApplyPreset auto-generates the
// X25519 key share into KeyShares[0].Data and ApplyConfig/session sync mutate
// extension objects in place. Sharing one global spec across concurrent (or
// even sequential) handshakes races on that state — the losers send malformed
// hellos and fail with "local error: tls: internal error". A fresh deep copy
// per DialTLS keeps the wire bytes identical (verified byte-for-byte against
// the official CLI capture modulo random/session/keyshare) and is race-free.
//
// Captured live from opencode CLI 2.1.4 (Bun/BoringSSL) via a CONNECT
// relay 2026-10-26 (SNI opencode.ai, bridge via a clean proxy IP — the
// VM IP alone 429s even with a fresh account, so the capture IP matters).
// Previous spec was CLI 1.18.31; v2 changes three things, all load-bearing
// now that upstream 429s curl/Python stacks with the same IP/account/body:
//  1. Curves/KeyShare gain X25519MLKEM768 (4588, 1216 bytes) FIRST,
//     then X25519. uTLS v1.8.2 supports it; ApplyPreset auto-generates
//     both shares when both groups are listed.
//  2. No Padding extension at the end — v2 sends none.
//  3. SignatureAlgorithms grows from 9 to 20, in the exact v2 order
//     below (raw uint16 values where uTLS has no named constant).
// Cipher list, ALPN (http/1.1 only), extension order (minus padding),
// and SupportedVersions (TLS1.3, TLS1.2) are unchanged from v1.
//
// JA3 (v2, measured 2026-10-26):
// 771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0
// JA3 md5: 1523504b38f0fae0d881d4b6554aac1b
func bunSpecForConn() *utls.ClientHelloSpec {
	spec := utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1303, // TLS_CHACHA20_POLY1305_SHA256 (CLI sends 1303, NOT 1304/CCM8)
			0xC02B, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
			0xC02F, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
			0xC02C, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
			0xC030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
			0xCCA9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305
			0xCCA8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305
			0xC009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
			0xC013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
			0xC00A, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
			0xC014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
			0x009C, // TLS_RSA_WITH_AES_128_GCM_SHA256
			0x009D, // TLS_RSA_WITH_AES_256_GCM_SHA384
			0x002F, // TLS_RSA_WITH_AES_128_CBC_SHA
			0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
		},
	CompressionMethods: []uint8{0x00},
	Extensions: []utls.TLSExtension{
		&utls.SNIExtension{},
		&utls.ExtendedMasterSecretExtension{},
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
			utls.X25519MLKEM768, // 4588, v2 only — must be first
			utls.X25519,         // 29
			utls.CurveP256,      // 23
			utls.CurveP384,      // 24
		}},
		&utls.SupportedPointsExtension{SupportedPoints: []byte{0x00}},
		&utls.SessionTicketExtension{},
		&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
		&utls.StatusRequestExtension{},
		// v2 order, verbatim from the 2026-10-26 capture. Named constants
		// cover the first 14; the last 6 are legacy schemes with no uTLS
		// constant, sent as raw values (uTLS marshals the list as-is).
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			utls.ECDSAWithP256AndSHA256, // 1027
			utls.ECDSAWithP384AndSHA384, // 1283
			utls.ECDSAWithP521AndSHA512, // 1539
			utls.Ed25519,               // 2055
			2056, // 0x0808, v2 only
			2057, // 0x0809, v2 only
			2058, // 0x080a, v2 only
			2059, // 0x080b, v2 only
			utls.PSSWithSHA256, // 2052
			utls.PSSWithSHA384, // 2053
			utls.PSSWithSHA512, // 2054
			utls.PKCS1WithSHA256, // 1025
			utls.PKCS1WithSHA384, // 1281
			utls.PKCS1WithSHA512, // 1537
			771,  // 0x0303, legacy, v2 only
			769,  // 0x0301, legacy, v2 only
			770,  // 0x0302, legacy, v2 only
			1026, // 0x0402, legacy, v2 only
			1282, // 0x0502, legacy, v2 only
			1538, // 0x0602, legacy, v2 only
		}},
		&utls.SCTExtension{},
		// Empty Data = auto-generate fresh shares per handshake, in order:
		// MLKEM768 first (1216 bytes), then X25519 (32 bytes), matching v2.
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
			{Group: utls.X25519MLKEM768},
			{Group: utls.X25519},
		}},
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
		&utls.SupportedVersionsExtension{Versions: []uint16{
			utls.VersionTLS13, // 772
			utls.VersionTLS12, // 771
		}},
		// No Padding extension: CLI v2 sends none. The old BoringPadding
		// extension was a v1 fingerprint marker and must not be sent.
		},
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
	}
	return &spec
}



