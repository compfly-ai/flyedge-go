// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"runtime/debug"
	"strings"
)

const modulePath = "github.com/compfly-ai/flyedge-go"

// sdkVersion identifies this SDK in the manifest and telemetry. It is the module version the
// consuming binary was built with, so a release needs no edit here.
var sdkVersion = versionFromBuildInfo(readBuildInfo())

func readBuildInfo() *debug.BuildInfo {
	info, _ := debug.ReadBuildInfo()
	return info
}

// versionFromBuildInfo reports "flyedge-go/<version>", or "flyedge-go/devel" when the build has
// no released version of this module (its own tests, a local replace, or no build info).
func versionFromBuildInfo(info *debug.BuildInfo) string {
	const prefix, devel = "flyedge-go/", "flyedge-go/devel"
	if info == nil {
		return devel
	}
	mod := &info.Main
	if mod.Path != modulePath {
		mod = nil
		for _, dep := range info.Deps {
			if dep.Path == modulePath {
				mod = dep
				break
			}
		}
	}
	if mod == nil {
		return devel
	}
	if mod.Replace != nil {
		mod = mod.Replace
	}
	version := strings.TrimPrefix(mod.Version, "v")
	if version == "" || version == "(devel)" {
		return devel
	}
	return prefix + version
}
