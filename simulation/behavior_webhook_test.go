package simulation

import "testing"

func TestWebhookURLMatchesHostRatherThanSubstring(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://hooks.slack.com/services/T/B/X", true},
		{"https://discord.com/api/webhooks/123/token", true},
		{"https://team.ngrok.io/path", true},
		{"https://example.webhook.site/path", true},
		{"https://hooks.slack.com.attacker.example/services/T/B/X", false},
		{"https://discord.com.attacker.example/api/webhooks/123/token", false},
		{"https://attacker.example/?next=https://hooks.slack.com.evil.example/", false},
		{"https://discord.com/api/other/path", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := isWebhookURL(tc.url); got != tc.want {
				t.Fatalf("isWebhookURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
			flags := detectExfiltration("", tc.url)
			gotFlag := false
			for _, flag := range flags {
				gotFlag = gotFlag || flag == "webhook_url_detected"
			}
			if gotFlag != tc.want {
				t.Fatalf("detectExfiltration(%q) webhook flag = %v, want %v", tc.url, gotFlag, tc.want)
			}
		})
	}
}
