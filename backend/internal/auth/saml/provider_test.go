package saml

import (
	"reflect"
	"testing"

	"github.com/crewjam/saml"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

func TestNewProvider_MissingACSURL(t *testing.T) {
	cfg := config.SAMLConfig{Enabled: true}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}
	if _, err := NewProvider(cfg, idp); err == nil {
		t.Fatal("expected error for missing acs_url")
	}
}

func TestNewProvider_InvalidACSURL(t *testing.T) {
	cfg := config.SAMLConfig{Enabled: true, ACSURL: "://bad"}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}
	if _, err := NewProvider(cfg, idp); err == nil {
		t.Fatal("expected error for invalid acs_url")
	}
}

func TestNewProvider_NoMetadata(t *testing.T) {
	cfg := config.SAMLConfig{Enabled: true, ACSURL: "https://tsm.example.com/api/v1/auth/saml/acs"}
	idp := config.SAMLIdPConfig{Name: "test-idp"}
	if _, err := NewProvider(cfg, idp); err == nil {
		t.Fatal("expected error when neither metadata_url nor metadata_xml is set")
	}
}

func TestNewProvider_WithMetadataXML(t *testing.T) {
	cfg := config.SAMLConfig{
		Enabled:  true,
		ACSURL:   "https://tsm.example.com/api/v1/auth/saml/acs",
		EntityID: "https://tsm.example.com",
	}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}
	p, err := NewProvider(cfg, idp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name() != "test-idp" {
		t.Errorf("Name() = %q, want %q", p.Name(), "test-idp")
	}
}

// By default IdP-initiated SSO must be off (SP-initiated only), so unsolicited
// responses are rejected unless an operator opts in.
func TestNewProvider_IDPInitiatedDefaultsOff(t *testing.T) {
	cfg := config.SAMLConfig{Enabled: true, ACSURL: "https://tsm.example.com/api/v1/auth/saml/acs"}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}

	p, err := NewProvider(cfg, idp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.sp.AllowIDPInitiated {
		t.Error("AllowIDPInitiated should default to false")
	}

	cfg.AllowIDPInitiated = true
	p2, err := NewProvider(cfg, idp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p2.sp.AllowIDPInitiated {
		t.Error("AllowIDPInitiated should be true when configured")
	}
}

func TestNewProvider_EntityIDFallback(t *testing.T) {
	cfg := config.SAMLConfig{
		Enabled: true,
		ACSURL:  "https://tsm.example.com/api/v1/auth/saml/acs",
		// EntityID intentionally empty — should derive from ACSURL.
	}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}
	p, err := NewProvider(cfg, idp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if md := p.GetMetadata(); md.EntityID != "https://tsm.example.com/api/v1/auth" {
		t.Errorf("entity ID = %q, want derived value", md.EntityID)
	}
}

func TestGetMetadata_ReturnsValidDescriptor(t *testing.T) {
	cfg := config.SAMLConfig{
		Enabled:  true,
		ACSURL:   "https://tsm.example.com/api/v1/auth/saml/acs",
		EntityID: "https://tsm.example.com",
	}
	idp := config.SAMLIdPConfig{Name: "test-idp", MetadataXML: minimalIdPMetadata}
	p, err := NewProvider(cfg, idp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	md := p.GetMetadata()
	if md == nil {
		t.Fatal("GetMetadata() returned nil")
	}
	if md.EntityID != "https://tsm.example.com" {
		t.Errorf("EntityID = %q, want %q", md.EntityID, "https://tsm.example.com")
	}
}

func TestExtractUserInfo_EmailAndName(t *testing.T) {
	assertion := &saml.Assertion{
		Subject: &saml.Subject{NameID: &saml.NameID{Value: "user@example.com"}},
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{
				{
					Name:         "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
					FriendlyName: "email",
					Values:       []saml.AttributeValue{{Value: "jane@example.com"}},
				},
				{
					Name:         "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
					FriendlyName: "displayName",
					Values:       []saml.AttributeValue{{Value: "Jane Doe"}},
				},
			},
		}},
	}
	info := extractUserInfo(assertion, "")
	if info.NameID != "user@example.com" {
		t.Errorf("NameID = %q", info.NameID)
	}
	if info.Email != "jane@example.com" {
		t.Errorf("Email = %q", info.Email)
	}
	if info.Name != "Jane Doe" {
		t.Errorf("Name = %q", info.Name)
	}
}

func TestExtractUserInfo_GroupAttribute(t *testing.T) {
	assertion := &saml.Assertion{
		Subject: &saml.Subject{NameID: &saml.NameID{Value: "user@example.com"}},
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{{
				Name:   "memberOf",
				Values: []saml.AttributeValue{{Value: "admins"}, {Value: "developers"}},
			}},
		}},
	}
	info := extractUserInfo(assertion, "memberOf")
	if len(info.Groups) != 2 || info.Groups[0] != "admins" || info.Groups[1] != "developers" {
		t.Errorf("Groups = %v, want [admins developers]", info.Groups)
	}
}

func TestExtractUserInfo_EmailFallbackToNameID(t *testing.T) {
	assertion := &saml.Assertion{
		Subject:             &saml.Subject{NameID: &saml.NameID{Value: "user@example.com"}},
		AttributeStatements: []saml.AttributeStatement{},
	}
	if info := extractUserInfo(assertion, ""); info.Email != "user@example.com" {
		t.Errorf("Email = %q, want fallback to NameID", info.Email)
	}
}

func TestExtractUserInfo_NoEmailNoFallback(t *testing.T) {
	assertion := &saml.Assertion{
		Subject:             &saml.Subject{NameID: &saml.NameID{Value: "not-an-email"}},
		AttributeStatements: []saml.AttributeStatement{},
	}
	if info := extractUserInfo(assertion, ""); info.Email != "" {
		t.Errorf("Email = %q, want empty (NameID is not an email)", info.Email)
	}
}

func TestResolveSAMLGroupMappings(t *testing.T) {
	mappings := []config.SAMLGroupMapping{
		{Group: "tf-admins", Organization: "default", Role: "admin"},
		{Group: "net", Organization: "network", Role: "editor"},
	}

	t.Run("matching group -> desired role, all orgs managed", func(t *testing.T) {
		desired, managed := ResolveSAMLGroupMappings([]string{"tf-admins"}, mappings)
		if !reflect.DeepEqual(desired, map[string]string{"default": "admin"}) {
			t.Fatalf("desired = %v", desired)
		}
		if len(managed) != 2 {
			t.Fatalf("managed = %v, want 2 orgs", managed)
		}
	})

	t.Run("no matching group -> empty desired, orgs still managed (revoked)", func(t *testing.T) {
		desired, managed := ResolveSAMLGroupMappings([]string{"unrelated"}, mappings)
		if len(desired) != 0 {
			t.Fatalf("desired should be empty, got %v", desired)
		}
		if _, ok := managed["default"]; !ok {
			t.Fatal("default org should be managed (eligible for revocation)")
		}
	})

	t.Run("group match is exact (case-sensitive)", func(t *testing.T) {
		desired, _ := ResolveSAMLGroupMappings([]string{"TF-Admins"}, mappings)
		if len(desired) != 0 {
			t.Fatalf("expected no match for differing case, got %v", desired)
		}
	})
}

func TestIsEmailAttr(t *testing.T) {
	tests := []struct {
		name, friendly string
		want           bool
	}{
		{"urn:oid:0.9.2342.19200300.100.1.3", "", true},
		{"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", "", true},
		{"email", "", true},
		{"mail", "", true},
		{"", "email", true},
		{"", "mail", true},
		{"givenName", "", false},
	}
	for _, tt := range tests {
		if got := isEmailAttr(tt.name, tt.friendly); got != tt.want {
			t.Errorf("isEmailAttr(%q, %q) = %v, want %v", tt.name, tt.friendly, got, tt.want)
		}
	}
}

func TestIsNameAttr(t *testing.T) {
	tests := []struct {
		name, friendly string
		want           bool
	}{
		{"urn:oid:2.16.840.1.113730.3.1.241", "", true},
		{"urn:oid:2.5.4.3", "", true},
		{"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name", "", true},
		{"displayName", "", true},
		{"", "displayName", true},
		{"", "cn", true},
		{"email", "", false},
	}
	for _, tt := range tests {
		if got := isNameAttr(tt.name, tt.friendly); got != tt.want {
			t.Errorf("isNameAttr(%q, %q) = %v, want %v", tt.name, tt.friendly, got, tt.want)
		}
	}
}

func TestFetchIdPMetadata_RequiresHTTPS(t *testing.T) {
	if _, err := fetchIdPMetadata("http://insecure.example.com/metadata"); err == nil {
		t.Fatal("expected error for non-HTTPS metadata URL")
	}
}

// minimalIdPMetadata is a valid SAML IdP metadata XML for tests.
const minimalIdPMetadata = `<?xml version="1.0"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata"
                  entityID="https://idp.example.com">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"
                         Location="https://idp.example.com/sso"/>
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
                         Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`
