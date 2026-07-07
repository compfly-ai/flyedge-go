package flyedge

import (
	"os"
	"time"
)

// Mode governs local detectors (loop/injection/etc). A server-side deny always enforces
// regardless of Mode. Mirrors FLYEDGE_MODE in the Python SDK.
type Mode string

const (
	ModeEnforce Mode = "enforce"
	ModeWarn    Mode = "warn" // default
	ModeAudit   Mode = "audit"
	ModeOff     Mode = "off"
)

// FailMode decides what happens when the enforcement call itself fails (network/5xx). It is
// distinct from Mode: FailOpen allows the request through, FailClosed denies it.
type FailMode string

const (
	FailOpen   FailMode = "fail_open" // default — availability over strictness
	FailClosed FailMode = "fail_closed"
)

// DefaultAPIURL is the prism gateway base URL used when Config.APIURL is empty.
const DefaultAPIURL = "https://prism.p.compfly.ai"

// Config is the full configuration for a Guard. Zero values are sane: an empty Config yields a
// warn-mode, fail-open, check-only guard against the default API URL. Populate via LoadEnv or set
// fields directly — there are no scattered env reads elsewhere in the package.
type Config struct {
	// APIURL is the prism gateway base (COMPFLY_API_URL). Empty → DefaultAPIURL.
	APIURL string
	// DID is the agent's decentralized identifier (COMPFLY_AGENT_DID), e.g.
	// did:compfly:<org_short>:<fingerprint>. Required to sign requests.
	DID string
	// KeyPEMPath is the path to the Ed25519 private key PEM (COMPFLY_AGENT_PRIVATE_KEY_PATH).
	// KeyPEM is the inline PEM (COMPFLY_AGENT_PRIVATE_KEY); it takes precedence when set.
	KeyPEMPath string
	KeyPEM     []byte
	// Mode + FailMode; empty values default to ModeWarn / FailOpen in New.
	Mode     Mode
	FailMode FailMode
	// ProxyMode routes wrapped LLM traffic through prism /v1/proxy (M2). Off = check-only.
	ProxyMode bool
	// Timeout bounds each enforcement HTTP call. Zero → 30s.
	Timeout time.Duration
}

// LoadEnv builds a Config from COMPFLY_*/FLYEDGE_* environment variables. This is the single,
// explicit place env is read — callers may then override fields before calling New.
func LoadEnv() Config {
	cfg := Config{
		APIURL:     os.Getenv("COMPFLY_API_URL"),
		DID:        os.Getenv("COMPFLY_AGENT_DID"),
		KeyPEMPath: os.Getenv("COMPFLY_AGENT_PRIVATE_KEY_PATH"),
		Mode:       Mode(os.Getenv("FLYEDGE_MODE")),
		FailMode:   FailMode(os.Getenv("FLYEDGE_FAIL_MODE")),
	}
	if inline := os.Getenv("COMPFLY_AGENT_PRIVATE_KEY"); inline != "" {
		cfg.KeyPEM = []byte(inline)
	}
	return cfg
}

// withDefaults returns a copy with empty fields filled by package defaults.
func (c Config) withDefaults() Config {
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	if c.Mode == "" {
		c.Mode = ModeWarn
	}
	if c.FailMode == "" {
		c.FailMode = FailOpen
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	return c
}
