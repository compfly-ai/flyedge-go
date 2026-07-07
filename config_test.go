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
