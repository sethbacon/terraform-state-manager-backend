package api

import (
	"encoding/json"
	"testing"
)

// A full-replace PUT that omits "use_tls" must not be read as "disable TLS".
// As a plain bool it decoded to false and silently switched the relay to
// plaintext; the pointer makes absent distinguishable from an explicit false.
func TestSMTPInput_OmittedUseTLSIsAbsentNotFalse(t *testing.T) {
	ptr := func(b bool) *bool { return &b }
	for _, tc := range []struct {
		name string
		body string
		want *bool
	}{
		{"omitted stays absent", `{"host":"relay","port":587}`, nil},
		{"explicit false is honoured", `{"host":"relay","use_tls":false}`, ptr(false)},
		{"explicit true is honoured", `{"host":"relay","use_tls":true}`, ptr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in notificationsSMTPInput
			if err := json.Unmarshal([]byte(tc.body), &in); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			switch {
			case tc.want == nil && in.UseTLS != nil:
				t.Fatalf("omitted use_tls decoded to %v; absent must stay absent or a partial PUT silently disables TLS", *in.UseTLS)
			case tc.want != nil && in.UseTLS == nil:
				t.Fatalf("explicit use_tls=%v decoded to nil; an explicit choice must survive", *tc.want)
			case tc.want != nil && *in.UseTLS != *tc.want:
				t.Fatalf("use_tls = %v, want %v", *in.UseTLS, *tc.want)
			}
		})
	}
}
