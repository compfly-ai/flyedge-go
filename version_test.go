// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"runtime/debug"
	"testing"
)

func TestSDKVersionFromBuildInfo(t *testing.T) {
	cases := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"no build info", nil, "flyedge-go/devel"},
		{"dependency", &debug.BuildInfo{Deps: []*debug.Module{
			{Path: "golang.org/x/net", Version: "v0.55.0"},
			{Path: modulePath, Version: "v0.11.2"},
		}}, "flyedge-go/0.11.2"},
		{"replaced dependency", &debug.BuildInfo{Deps: []*debug.Module{
			{Path: modulePath, Version: "v0.11.2", Replace: &debug.Module{Path: "../flyedge-go"}},
		}}, "flyedge-go/devel"},
		{"replaced with a version", &debug.BuildInfo{Deps: []*debug.Module{
			{Path: modulePath, Version: "v0.11.2", Replace: &debug.Module{Path: "example.com/fork", Version: "v0.12.0"}},
		}}, "flyedge-go/0.12.0"},
		{"main module", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, "flyedge-go/devel"},
		{"tagged main module", &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.11.3"}}, "flyedge-go/0.11.3"},
	}
	for _, c := range cases {
		if got := versionFromBuildInfo(c.info); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
