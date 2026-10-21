// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/deployment_platforms.json is a copy of flyedge's conformance/deployment_platforms.json;
// keep them identical so every SDK reports the same platform values.
func TestDetectDeployment(t *testing.T) {
	b, err := os.ReadFile("testdata/deployment_platforms.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string            `json:"name"`
		Env       map[string]string `json:"env"`
		Dockerenv bool              `json:"dockerenv"`
		Platform  string            `json:"platform"`
		Region    string            `json:"region"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			d := detectDeployment(func(k string) string { return c.Env[k] }, c.Dockerenv)
			if d.Platform != c.Platform {
				t.Errorf("platform = %q, want %q", d.Platform, c.Platform)
			}
			if d.Region != c.Region {
				t.Errorf("region = %q, want %q", d.Region, c.Region)
			}
		})
	}
}
