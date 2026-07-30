package main

import "testing"

func TestUpstreamFor(t *testing.T) {
	cases := []struct {
		path     string
		wantHost string
	}{
		{"/v1/chat/completions", "api.openai.com"},
		{"/v1/messages", "api.anthropic.com"},
		{"/v1beta/models/gemini-1.5-pro:generateContent", "generativelanguage.googleapis.com"},
		{"/v1beta/models/gemini-1.5-pro:streamGenerateContent", "generativelanguage.googleapis.com"},
		{"/unknown/path", ""},
	}
	for _, c := range cases {
		u := upstreamFor(c.path)
		if c.wantHost == "" {
			if u != nil {
				t.Errorf("upstreamFor(%q) = %v, want nil", c.path, u)
			}
			continue
		}
		if u == nil || u.Host != c.wantHost {
			t.Errorf("upstreamFor(%q) host = %v, want %q", c.path, u, c.wantHost)
		}
	}
}
