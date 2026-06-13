package api

import (
	"encoding/json"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestEnforceEmailVerified(t *testing.T) {
	tests := []struct {
		name     string
		verified *bool
		require  bool
		wantErr  error
	}{
		{"nil+require", nil, true, errEmailVerifiedMissing},
		{"nil+no-require", nil, false, nil},
		{"false+require", boolPtr(false), true, errEmailNotVerified},
		{"false+no-require", boolPtr(false), false, errEmailNotVerified},
		{"true+require", boolPtr(true), true, nil},
		{"true+no-require", boolPtr(true), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := enforceEmailVerified(tt.verified, tt.require)
			if err != tt.wantErr {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

type fakeClaims struct {
	data []byte
	err  error
}

func (f *fakeClaims) Claims(v interface{}) error {
	if f.err != nil {
		return f.err
	}
	return json.Unmarshal(f.data, v)
}

func TestEmailVerifiedClaim(t *testing.T) {
	tests := []struct {
		name string
		json string
		want *bool
	}{
		{"present true", `{"email_verified":true}`, boolPtr(true)},
		{"present false", `{"email_verified":false}`, boolPtr(false)},
		{"absent", `{"email":"a@b.com"}`, nil},
		{"null", `{"email_verified":null}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := emailVerifiedClaim(&fakeClaims{data: []byte(tt.json)})
			if got == nil && tt.want == nil {
				return
			}
			if (got == nil) != (tt.want == nil) || *got != *tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoleScopesPermittedBy(t *testing.T) {
	tests := []struct {
		name   string
		caller []string
		role   []string
		want   bool
	}{
		{"empty role scopes", []string{"modules:read"}, nil, true},
		{"admin grants all", []string{"admin"}, []string{"modules:write", "admin"}, true},
		{"exact match", []string{"modules:read", "modules:write"}, []string{"modules:read"}, true},
		{"missing scope", []string{"modules:read"}, []string{"modules:write"}, false},
		{"non-admin assigning admin", []string{"modules:write"}, []string{"admin"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := roleScopesPermittedBy(tt.caller, tt.role)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
