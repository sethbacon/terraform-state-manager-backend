package api

import "testing"

func TestChannelRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     channelRequest
		wantErr bool
	}{
		{"valid webhook", channelRequest{Name: "a", Type: "webhook", Target: "https://example.com/hook"}, false},
		{"valid slack", channelRequest{Name: "a", Type: "slack", Target: "https://hooks.slack.com/x"}, false},
		{"valid teams", channelRequest{Name: "a", Type: "teams", Target: "https://example.webhook.office.com/x"}, false},
		{"valid no target (edit)", channelRequest{Name: "a", Type: "webhook"}, false},
		{"valid event filter", channelRequest{Name: "a", Type: "webhook", Target: "https://x.io", Events: []string{"drift_detected"}}, false},
		{"valid email single", channelRequest{Name: "a", Type: "email", Target: "ops@example.com"}, false},
		{"valid email multiple", channelRequest{Name: "a", Type: "email", Target: "ops@example.com, oncall@example.com"}, false},
		{"bad type", channelRequest{Name: "a", Type: "pagerduty", Target: "https://x.io"}, true},
		{"bad event", channelRequest{Name: "a", Type: "webhook", Target: "https://x.io", Events: []string{"nope"}}, true},
		{"non-http target", channelRequest{Name: "a", Type: "webhook", Target: "ftp://x.io"}, true},
		{"garbage target", channelRequest{Name: "a", Type: "webhook", Target: "://nope"}, true},
		{"email with url target", channelRequest{Name: "a", Type: "email", Target: "https://x.io"}, true},
		{"email invalid address", channelRequest{Name: "a", Type: "email", Target: "not-an-email"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
