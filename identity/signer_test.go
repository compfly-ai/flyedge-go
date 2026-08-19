// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strconv"
	"testing"
	"time"
)

// newTestPEM generates an Ed25519 key and returns its PKCS#8 PEM + the public key.
func newTestPEM(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), pub
}

// TestSignVerifiesLikePrism reconstructs exactly what prism's verifier does — SHA-256(ts||body)
// then Ed25519 verify over that digest — and asserts our signature passes. This is the wire-contract
// guarantee: if this holds, prism accepts our signature.
func TestSignVerifiesLikePrism(t *testing.T) {
	pemBytes, pub := newTestPEM(t)
	did := "did:compfly:acme01:deadbeef"
	s, err := NewFileSigner(pemBytes, did)
	if err != nil {
		t.Fatalf("NewFileSigner: %v", err)
	}

	body := []byte(`{"stage":"pre_llm","content":"hello"}`)
	ts := time.UnixMilli(1750000000000)
	hdrs, err := s.Sign(body, ts)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// verifier side (prism): message = SHA-256(timestamp_str || body); verify(message, sig)
	h := sha256.New()
	h.Write([]byte(hdrs[HeaderTimestamp]))
	h.Write(body)
	message := h.Sum(nil)

	sig, err := base64.StdEncoding.DecodeString(hdrs[HeaderSignature])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if !ed25519.Verify(pub, message, sig) {
		t.Fatal("signature did NOT verify over SHA-256(ts||body) — not wire-compatible with prism")
	}

	// header sanity
	if hdrs[HeaderTimestamp] != strconv.FormatInt(ts.UnixMilli(), 10) {
		t.Errorf("timestamp header = %q", hdrs[HeaderTimestamp])
	}
	if hdrs[HeaderAgentDID] != did {
		t.Errorf("did header = %q", hdrs[HeaderAgentDID])
	}
	if hdrs[HeaderKeyID] != s.KeyID() {
		t.Errorf("keyID header mismatch")
	}
	if hdrs[HeaderTenantID] != "acme01" {
		t.Errorf("tenant header = %q, want acme01", hdrs[HeaderTenantID])
	}
}

// TestKeyIDLength: 32 hex chars of SHA-256(SPKI DER).
func TestKeyIDDerivation(t *testing.T) {
	_, pub := newTestPEM(t)
	kid, err := KeyIDFromPublic(pub)
	if err != nil {
		t.Fatalf("KeyIDFromPublic: %v", err)
	}
	if len(kid) != 32 {
		t.Errorf("keyID len = %d, want 32", len(kid))
	}
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	if want := hex.EncodeToString(sum[:])[:32]; kid != want {
		t.Errorf("keyID = %q, want first 32 hex of SHA-256(SPKI) = %q", kid, want)
	}
}

func TestTenantFromDID(t *testing.T) {
	cases := map[string]string{
		"did:compfly:acme01:abc123": "acme01",
		"did:compfly:org:fp":        "org",
		"not-a-did":                 "",
		"":                          "",
	}
	for in, want := range cases {
		if got := TenantFromDID(in); got != want {
			t.Errorf("TenantFromDID(%q) = %q, want %q", in, got, want)
		}
	}
}
