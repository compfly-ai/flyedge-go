// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import "testing"

func TestLoadEnvFailMode(t *testing.T) {
	t.Setenv("FLYEDGE_FAIL_MODE", "fail_closed")
	if got := LoadEnv().FailMode; got != FailClosed {
		t.Errorf("FailMode = %q, want %q", got, FailClosed)
	}
}

func TestFailModeDefaultsToOpen(t *testing.T) {
	// Unset env → LoadEnv yields "", which withDefaults fills to FailOpen.
	t.Setenv("FLYEDGE_FAIL_MODE", "")
	if got := LoadEnv().withDefaults().FailMode; got != FailOpen {
		t.Errorf("default FailMode = %q, want %q", got, FailOpen)
	}
}

func TestLoadEnvCloudTelemetry(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		want   bool
		absent bool
	}{
		{"", true, true}, {"true", true, false}, {"false", false, false}, {"0", false, false}, {"1", true, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("FLYEDGE_CLOUD_TELEMETRY", tc.raw)
			cfg := LoadEnv()
			if (cfg.CloudTelemetry == nil) != tc.absent {
				t.Fatal("unexpected configured value")
			}
			if got := *cfg.withDefaults().CloudTelemetry; got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	t.Setenv("FLYEDGE_CLOUD_TELEMETRY", "invalid")
	cfg := LoadEnv()
	if _, err := New(cfg); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	enabled := false
	cfg.CloudTelemetry = &enabled
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.cloudTelemetry {
		t.Fatal("explicit field override ignored")
	}
}
