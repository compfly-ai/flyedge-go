package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInvocationContractValidationAndSnapshot(t *testing.T) {
	c := &InvocationContract{State: StateContract{Fields: map[string]StateField{"order": {Type: StateString}}, Required: []string{"order"}}}
	copy, err := snapshotInvocationContract(c)
	if err != nil {
		t.Fatal(err)
	}
	c.State.Fields["order"] = StateField{Type: StateNumber}
	c.State.Required[0] = "changed"
	if copy.State.Fields["order"].Type != StateString || copy.State.Required[0] != "order" {
		t.Fatal("registration contract aliases caller memory")
	}
	for _, invalid := range []*InvocationContract{
		{State: StateContract{}},
		{State: StateContract{Fields: map[string]StateField{"order": {Type: "unknown"}}}},
		{State: StateContract{Fields: map[string]StateField{"bad.name": {Type: StateString}}}},
		{State: StateContract{Fields: map[string]StateField{}, Required: []string{"missing"}}},
		{State: StateContract{Fields: map[string]StateField{"order": {Type: StateString}}, Required: []string{"order", "order"}}},
	} {
		if _, err := snapshotInvocationContract(invalid); err == nil {
			t.Fatalf("accepted %+v", invalid)
		}
	}
	if _, err := (InvocationOptions{Contract: copy}).validate(); err == nil {
		t.Fatal("state contract must explicitly enable client state")
	}
}

func TestContractRegistrationRejectsLegacyBrokerWithoutAcknowledgement(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Error("opened socket before contract acknowledgement")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "test", "expires_at": time.Now().Add(time.Minute)})
	}))
	defer server.Close()
	g, err := New(Config{APIURL: server.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	_, err = g.ServeInvocations(context.Background(), InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: time.Minute, AllowClientState: true, AllowInsecureLoopback: true, Contract: &InvocationContract{State: StateContract{Fields: map[string]StateField{"order": {Type: StateString}}}}}, func(context.Context, InvocationRequest, InvocationWriter) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("expected unsupported broker error, got %v", err)
	}
}
