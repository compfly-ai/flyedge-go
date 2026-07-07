// Command flyedge-proxy is a standalone signing + policy-enforcing HTTP proxy for LLM traffic.
// Any-language agent points its LLM base URL at this proxy; the proxy runs a flyedge pre_llm check
// (via the prism gateway) before forwarding to the real provider, and returns 403 on a policy
// Deny. It is the network-level counterpart to the in-process SDK wrap — built on the SAME core:
// an httputil.ReverseProxy whose Transport is guard.WrapRoundTripper, so extraction/check/deny
// logic is shared with the library, not duplicated.
//
// Routing (by path):
//   POST /v1/chat/completions  → https://api.openai.com     (point the OpenAI SDK base_url here)
//   POST /v1/messages          → https://api.anthropic.com  (point the Anthropic SDK base_url here)
//
// The agent still sends its own provider API key (Authorization/x-api-key); the proxy forwards it
// unchanged — it never needs the provider credential itself.
//
// Env: LISTEN_ADDR (default :9000) + the flyedge COMPFLY_*/FLYEDGE_* vars (see flyedge.LoadEnv).
// Per-request session: send X-Session-Id to scope multi-turn correlation.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	flyedge "github.com/compfly-ai/flyedge-go"
)

// upstreams maps a request path to the real provider origin.
var upstreams = map[string]*url.URL{
	"/v1/chat/completions": mustURL("https://api.openai.com"),
	"/v1/messages":         mustURL("https://api.anthropic.com"),
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "flyedge-proxy:", err)
		os.Exit(1)
	}
}

func run() error {
	guard, err := flyedge.New(flyedge.LoadEnv())
	if err != nil {
		return fmt.Errorf("build guard: %w", err)
	}
	defer guard.Close()

	// The proxy's transport IS the flyedge wrap: it checks the (rewritten, provider-bound) request
	// before forwarding. A Deny surfaces as a *DenyError from RoundTrip → ErrorHandler → 403.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			up := upstreams[pr.In.URL.Path]
			if up == nil {
				return // unknown path: leave as-is; check-transport passes it through, forward 502s
			}
			pr.SetURL(up)                // scheme+host → provider
			pr.Out.Host = up.Host        // Host header the provider expects
			// carry an explicit session (from the client header) into the guard check
			if sid := pr.In.Header.Get("X-Session-Id"); sid != "" {
				pr.Out = pr.Out.WithContext(flyedge.ContextWithSession(pr.Out.Context(), sid))
			}
		},
		Transport: guard.WrapRoundTripper(http.DefaultTransport),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var de *flyedge.DenyError
			if errors.As(err, &de) {
				writeJSON(w, http.StatusForbidden, map[string]any{
					"error":  "policy_denied",
					"reason": de.Decision.Reason,
					"detail": de.Decision.Message,
				})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream_error", "detail": err.Error()})
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/", rp)

	addr := envOr("LISTEN_ADDR", ":9000")
	log.Printf("flyedge-proxy listening on %s (guard DID=%s) — route /v1/chat/completions→openai, /v1/messages→anthropic", addr, guard.DID())
	return http.ListenAndServe(addr, mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
