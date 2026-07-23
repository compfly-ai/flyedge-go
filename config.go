package flyedge

import (
	"os"
	"time"
)

// Mode is the local enforcement posture. A server-side deny or kill ALWAYS enforces regardless of
// Mode — Mode only decides (a) whether the policy check is called at all and (b) how an advisory
// server `warn` is treated locally:
//   - ModeOff:     skip the policy check entirely (local dev) — Check returns allow, no network call.
//   - ModeAudit:   check + record; never block on an advisory warn (server deny/kill still enforce).
//   - ModeWarn:    (default) block on server deny/kill only; a warn is advisory (returned, not blocked).
//   - ModeEnforce: also treat a server warn as blocking.
//
// Mirrors FLYEDGE_MODE in the Python SDK, whose policy middleware likewise enforces server denials
// independently of mode. (There are no local detectors in flyedge-go yet; when they land they follow
// the same posture — advisory in warn/audit, blocking in enforce.)
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
	// SimTelemetryURL overrides the simulation telemetry WebSocket URL the server advertises in the
	// config's `simulation` block (COMPFLY_SIM_TELEMETRY_URL). Server-authoritative by default (empty);
	// set it only for split-horizon local dev, where the agent runs on the host but the gateway hands
	// back an in-cluster URL (e.g. ws://prism:8080) the host can't resolve — point it at the host's
	// port-forwarded gateway (e.g. ws://localhost:8080/v1/simulation/telemetry).
	SimTelemetryURL string
}

// LoadEnv builds a Config from COMPFLY_*/FLYEDGE_* environment variables. This is the single,
// explicit place env is read — callers may then override fields before calling New.
func LoadEnv() Config {
	cfg := Config{
		APIURL:          os.Getenv("COMPFLY_API_URL"),
		DID:             os.Getenv("COMPFLY_AGENT_DID"),
		KeyPEMPath:      os.Getenv("COMPFLY_AGENT_PRIVATE_KEY_PATH"),
		Mode:            Mode(os.Getenv("FLYEDGE_MODE")),
		FailMode:        FailMode(os.Getenv("FLYEDGE_FAIL_MODE")),
		SimTelemetryURL: os.Getenv("COMPFLY_SIM_TELEMETRY_URL"),
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
