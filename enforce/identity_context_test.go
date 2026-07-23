package enforce

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrincipalEncodeRoundTrip(t *testing.T) {
	p := Principal{Provider: "entra", TenantID: "t1", OBOID: "u-42", UPN: "alice@corp.example", URN: "compfly:identity:v1:entra:user:id=alice", Scope: map[string]string{"role": "reader", "plan": "pro"}}
	enc := p.encode()
	if enc == "" {
		t.Fatal("encode returned empty for non-zero principal")
	}
	if strings.ContainsAny(enc, "=+/") {
		t.Fatalf("encode must be base64url with no padding, got %q", enc)
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("prism decode (URL_SAFE_NO_PAD) failed: %v", err)
	}
	var got Principal
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoded value is not JSON: %v", err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, p)
	}
}

func TestPrincipalEncodeZero(t *testing.T) {
	if got := (Principal{}).encode(); got != "" {
		t.Fatalf("zero principal must encode to empty, got %q", got)
	}
}

func TestIdentityHeaders(t *testing.T) {
	ctx := context.Background()
	if h := IdentityHeaders(ctx); len(h) != 0 {
		t.Fatalf("bare context should yield no headers, got %v", h)
	}

	ctx = ContextWithPrincipal(ctx, Principal{Provider: "mock", UPN: "bob@corp"})
	ctx = ContextWithDelegation(ctx, "jwt.abc.def")
	ctx = ContextWithAgentIdentity(ctx, "sid-1", "compfly:identity:v1:mock:agent:id=x")

	h := IdentityHeaders(ctx)
	if h[HeaderDelegationToken] != "jwt.abc.def" {
		t.Errorf("delegation header = %q", h[HeaderDelegationToken])
	}
	if h[HeaderAgentSID] != "sid-1" {
		t.Errorf("agent sid header = %q", h[HeaderAgentSID])
	}
	if h[HeaderAgentURN] != "compfly:identity:v1:mock:agent:id=x" {
		t.Errorf("agent urn header = %q", h[HeaderAgentURN])
	}
	raw, err := base64.RawURLEncoding.DecodeString(h[HeaderOBOPrincipal])
	if err != nil {
		t.Fatalf("OBO header not base64url-no-pad decodable: %v", err)
	}
	if !strings.Contains(string(raw), `"upn":"bob@corp"`) {
		t.Fatalf("OBO envelope missing upn: %s", raw)
	}
}

func TestIdentityHeadersEmptyInputsIgnored(t *testing.T) {
	ctx := context.Background()
	ctx = ContextWithPrincipal(ctx, Principal{}) // zero -> ignored
	ctx = ContextWithDelegation(ctx, "")         // empty -> ignored
	ctx = ContextWithAgentIdentity(ctx, "", "")  // both empty -> ignored
	if h := IdentityHeaders(ctx); len(h) != 0 {
		t.Fatalf("empty inputs should attach nothing, got %v", h)
	}
}

// TestCheckAttachesIdentityHeaders proves the enforcer sends the OBO/delegation/agent headers, that
// the OBO value decodes to the principal, and — critically — that the principal rides ONLY in the
// header, never in the signed request body (the frozen /check schema is untouched).
func TestCheckAttachesIdentityHeaders(t *testing.T) {
	var gotHeaders http.Header
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"allow"}`))
	}))
	defer srv.Close()

	e := NewHTTPEnforcer(srv.URL, nil, 5*time.Second) // nil signer isolates identity headers
	ctx := ContextWithPrincipal(context.Background(), Principal{Provider: "mock", UPN: "alice@corp.example", OBOID: "alice"})
	ctx = ContextWithAgentIdentity(ctx, "sid-9", "")

	dec, err := e.Check(ctx, CheckRequest{Stage: StagePreLLM, Content: Content{Full: "hello"}})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if dec.Action != ActionAllow {
		t.Fatalf("decision = %v", dec.Action)
	}

	obo := gotHeaders.Get(HeaderOBOPrincipal)
	if obo == "" {
		t.Fatal("OBO header not sent")
	}
	raw, err := base64.RawURLEncoding.DecodeString(obo)
	if err != nil {
		t.Fatalf("OBO header not decodable: %v", err)
	}
	var got Principal
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("OBO header not JSON: %v", err)
	}
	if got.UPN != "alice@corp.example" || got.OBOID != "alice" {
		t.Fatalf("OBO envelope wrong: %+v", got)
	}
	if gotHeaders.Get(HeaderAgentSID) != "sid-9" {
		t.Errorf("agent sid header = %q", gotHeaders.Get(HeaderAgentSID))
	}

	// The signed body must be a plain CheckRequest with no identity leakage.
	if strings.Contains(gotBody, "alice@corp.example") || strings.Contains(gotBody, "\"provider\"") {
		t.Fatalf("principal leaked into signed body: %s", gotBody)
	}
	var cr CheckRequest
	if err := json.Unmarshal([]byte(gotBody), &cr); err != nil {
		t.Fatalf("body is not a CheckRequest: %v", err)
	}
	if cr.Stage != StagePreLLM {
		t.Fatalf("body stage = %q", cr.Stage)
	}
}
