// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

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

// TestBuildManifestSkills verifies Anthropic Agent Skills reach the wire in the snake_case shape
// prism's SkillCapability expects, and that a skill change DOES move the manifest hash — the
// opposite of the enterprise block above. Skills are governed content: platform-backend's drift
// recognition matches a reported skill against the org's published edge packs, so a skill
// appearing or disappearing has to look like drift, not like pass-through metadata.
func TestBuildManifestSkills(t *testing.T) {
	base := ManifestInfo{Framework: "anthropic-sdk-go", Tools: []string{"whoami"}}

	withSkill := base
	withSkill.Skills = []SkillInfo{{
		Name:         "pr-conventions",
		Description:  "CompFly's git/PR workflow norms",
		AllowedTools: []string{"Bash"},
		Scripts:      []string{"check.sh"},
		Source:       "filesystem",
		SourcePath:   "~/.flyedge/managed-plugins/compfly/x/skills/pr-conventions/SKILL.md",
	}}
	m := buildManifest(withSkill)

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Capabilities struct {
			Skills []map[string]any `json:"skills"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Capabilities.Skills) != 1 {
		t.Fatalf("expected 1 skill on the wire, got %d", len(wire.Capabilities.Skills))
	}
	s := wire.Capabilities.Skills[0]
	if s["name"] != "pr-conventions" {
		t.Errorf("name = %v", s["name"])
	}
	// snake_case is the frozen contract with prism — camelCase would silently drop server-side.
	if _, ok := s["allowed_tools"]; !ok {
		t.Error("allowed_tools missing (wrong case would be dropped by prism)")
	}
	if _, ok := s["source_path"]; !ok {
		t.Error("source_path missing")
	}

	// A skill is drift-relevant: adding one must change the hash.
	if buildManifest(base).ManifestHash == m.ManifestHash {
		t.Error("adding a skill must change the manifest hash")
	}

	// And an agent with no skills must omit the key entirely — absent means "preserve existing"
	// to the backend, where an empty array would read as "this agent has zero skills".
	nb, _ := json.Marshal(buildManifest(base))
	var caps struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	_ = json.Unmarshal(nb, &caps)
	if _, present := caps.Capabilities["skills"]; present {
		t.Error("skills key must be omitted when the agent declares none")
	}
}
