package flyedge

import (
	"encoding/json"
	"testing"
)

// TestBuildManifestEnterprise verifies the enterprise identity block + token serialize onto the
// manifest, and that rotating them does NOT change the manifest hash (they are pass-through
// metadata, deliberately excluded from drift detection).
func TestBuildManifestEnterprise(t *testing.T) {
	base := ManifestInfo{Framework: "anthropic-sdk-go", Tools: []string{"whoami"}, Models: []string{"claude-sonnet-4-5"}}

	plain := buildManifest(base)

	ent := base
	ent.Enterprise = map[string]any{"provider": "entra", "tenantId": "t1", "roles": []string{"user"}}
	ent.EnterpriseToken = "eyJhbGciOiJFZERTQSJ9.payload.sig"
	withEnt := buildManifest(ent)

	// Enterprise fields present in the wire shape.
	b, err := json.Marshal(withEnt)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["enterprise"]; !ok {
		t.Error("enterprise block missing from manifest JSON")
	}
	if wire["enterprise_token"] != ent.EnterpriseToken {
		t.Errorf("enterprise_token = %v", wire["enterprise_token"])
	}

	// Hash unchanged: enterprise is not part of drift detection.
	if withEnt.ManifestHash != plain.ManifestHash {
		t.Errorf("enterprise fields must not change manifest hash:\n plain %s\n ent   %s", plain.ManifestHash, withEnt.ManifestHash)
	}

	// Absent enterprise omits both keys (absent != empty on the wire).
	pb, _ := json.Marshal(plain)
	var pw map[string]any
	_ = json.Unmarshal(pb, &pw)
	if _, ok := pw["enterprise"]; ok {
		t.Error("enterprise should be omitted when unset")
	}
	if _, ok := pw["enterprise_token"]; ok {
		t.Error("enterprise_token should be omitted when unset")
	}
}
