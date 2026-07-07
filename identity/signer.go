// Package identity implements the flyedge DID + Ed25519 request-signing contract. The wire format
// is fixed to match prism and policy-enforcer: the signature is Ed25519
// over SHA-256(ascii(decimal(timestamp_ms)) || body), and the key id is the first 32 hex chars of
// SHA-256 over the public key's SubjectPublicKeyInfo DER. See DESIGN.md §1a.
package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Header names carried on every signed request (must match the verifier verbatim).
const (
	HeaderSignature = "X-CompFly-Signature"
	HeaderKeyID     = "X-CompFly-Key-ID"
	HeaderAgentDID  = "X-CompFly-Agent-DID"
	HeaderTimestamp = "X-CompFly-Timestamp"
	HeaderTenantID  = "X-Tenant-ID"
)

// Signer produces the X-CompFly-* signature headers for a request body at a point in time.
// It is an interface so tests and alternate key backends (KMS, agent runtime) can substitute.
type Signer interface {
	// Sign returns the signature headers for body signed at ts.
	Sign(body []byte, ts time.Time) (map[string]string, error)
	DID() string
	KeyID() string
}

// FileSigner signs with an in-memory Ed25519 private key loaded from PEM.
type FileSigner struct {
	priv  ed25519.PrivateKey
	did   string
	keyID string
}

// NewFileSigner builds a signer from a PKCS#8 Ed25519 private key PEM and the agent DID.
func NewFileSigner(pemBytes []byte, did string) (*FileSigner, error) {
	if did == "" {
		return nil, fmt.Errorf("identity: DID is required")
	}
	priv, err := parseEd25519PrivatePEM(pemBytes)
	if err != nil {
		return nil, err
	}
	keyID, err := KeyIDFromPublic(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	return &FileSigner{priv: priv, did: did, keyID: keyID}, nil
}

// NewFileSignerFromPath loads the PEM from disk.
func NewFileSignerFromPath(path, did string) (*FileSigner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read key %q: %w", path, err)
	}
	return NewFileSigner(b, did)
}

func (s *FileSigner) DID() string   { return s.did }
func (s *FileSigner) KeyID() string { return s.keyID }

// Sign implements the frozen wire contract: digest = SHA-256(ascii(ms) || body); signature =
// base64(Ed25519.Sign(priv, digest)). We sign the digest as the message (manual prehash) — this is
// exactly what prism verifies via verifying_key.verify(&digest, &sig).
func (s *FileSigner) Sign(body []byte, ts time.Time) (map[string]string, error) {
	tsMS := ts.UnixMilli()
	digest := SigningDigest(tsMS, body)
	sig := ed25519.Sign(s.priv, digest)
	return map[string]string{
		HeaderSignature: base64.StdEncoding.EncodeToString(sig),
		HeaderKeyID:     s.keyID,
		HeaderAgentDID:  s.did,
		HeaderTimestamp: strconv.FormatInt(tsMS, 10),
		HeaderTenantID:  TenantFromDID(s.did),
	}, nil
}

// SigningDigest returns SHA-256(ascii(decimal(tsMS)) || body) — the exact bytes both sides sign
// and verify. Exported so tests (and the verifier-parity test) can reconstruct it.
func SigningDigest(tsMS int64, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(tsMS, 10)))
	h.Write(body)
	return h.Sum(nil)
}

// KeyIDFromPublic derives the key id: first 32 hex chars of SHA-256(SPKI DER of the public key).
func KeyIDFromPublic(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("identity: marshal SPKI: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:32], nil
}

func parseEd25519PrivatePEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("identity: no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse PKCS8 key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: key is %T, want ed25519.PrivateKey", key)
	}
	return priv, nil
}
