package scim

import (
	"testing"
	"time"

	"github.com/sethbacon/terraform-suite-identity/identity/models"
)

func TestExtractFilterValue(t *testing.T) {
	tests := []struct {
		filter string
		want   string
	}{
		{`userName eq "jane@example.com"`, "jane@example.com"},
		{`externalId eq "ext-123"`, "ext-123"},
		{`userName eq "test"`, "test"},
		{"invalid filter", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := extractFilterValue(tt.filter); got != tt.want {
			t.Errorf("extractFilterValue(%q) = %q, want %q", tt.filter, got, tt.want)
		}
	}
}

func TestUserToSCIM(t *testing.T) {
	sub := "scim:ext-123"
	now := time.Now()
	u := &models.User{
		ID:        "user-1",
		Email:     "jane@example.com",
		Name:      "Jane Doe",
		OIDCSub:   &sub,
		CreatedAt: now,
		UpdatedAt: now,
	}

	scimUser := userToSCIM(u, "https://tsm.example.com")

	if scimUser.ID != "user-1" {
		t.Errorf("ID = %q, want %q", scimUser.ID, "user-1")
	}
	if scimUser.UserName != "jane@example.com" {
		t.Errorf("UserName = %q", scimUser.UserName)
	}
	if scimUser.ExternalID != "ext-123" {
		t.Errorf("ExternalID = %q, want %q (should strip scim: prefix)", scimUser.ExternalID, "ext-123")
	}
	if scimUser.Name == nil || scimUser.Name.Formatted != "Jane Doe" {
		t.Errorf("Name.Formatted = %v, want %q", scimUser.Name, "Jane Doe")
	}
	if len(scimUser.Emails) != 1 || scimUser.Emails[0].Value != "jane@example.com" {
		t.Errorf("Emails = %v", scimUser.Emails)
	}
	if !scimUser.Active {
		t.Error("Active should be true")
	}
	if scimUser.Meta.Location != "https://tsm.example.com/scim/v2/Users/user-1" {
		t.Errorf("Meta.Location = %q", scimUser.Meta.Location)
	}
	if scimUser.Meta.ResourceType != "User" {
		t.Errorf("Meta.ResourceType = %q, want User", scimUser.Meta.ResourceType)
	}
}

func TestUserToSCIM_NoOIDCSub(t *testing.T) {
	now := time.Now()
	u := &models.User{ID: "user-2", Email: "bob@example.com", Name: "Bob", OIDCSub: nil, CreatedAt: now, UpdatedAt: now}
	if scimUser := userToSCIM(u, "https://example.com"); scimUser.ExternalID != "" {
		t.Errorf("ExternalID = %q, want empty when OIDCSub is nil", scimUser.ExternalID)
	}
}

func TestUserToSCIM_NonSCIMPrefix(t *testing.T) {
	sub := "oidc-regular-sub"
	now := time.Now()
	u := &models.User{ID: "user-3", Email: "alice@example.com", Name: "Alice", OIDCSub: &sub, CreatedAt: now, UpdatedAt: now}
	// Non-SCIM subs are returned unchanged (no scim: prefix to strip).
	if scimUser := userToSCIM(u, "https://example.com"); scimUser.ExternalID != "oidc-regular-sub" {
		t.Errorf("ExternalID = %q, want %q", scimUser.ExternalID, "oidc-regular-sub")
	}
}

func TestOrgToSCIMGroup(t *testing.T) {
	now := time.Now()
	org := &models.Organization{ID: "org-1", Name: "my-org", DisplayName: "My Organization", CreatedAt: now, UpdatedAt: now}

	group := orgToSCIMGroup(org, "https://tsm.example.com")

	if group["id"] != "org-1" {
		t.Errorf("id = %v, want org-1", group["id"])
	}
	if group["displayName"] != "my-org" {
		t.Errorf("displayName = %v, want my-org", group["displayName"])
	}
	schemas, ok := group["schemas"].([]string)
	if !ok || len(schemas) != 1 || schemas[0] != SchemaGroup {
		t.Errorf("schemas = %v, want [%s]", group["schemas"], SchemaGroup)
	}
	meta, ok := group["meta"].(SCIMMeta)
	if !ok {
		t.Fatal("meta is not SCIMMeta")
	}
	if meta.ResourceType != "Group" {
		t.Errorf("meta.ResourceType = %q, want Group", meta.ResourceType)
	}
	if meta.Location != "https://tsm.example.com/scim/v2/Groups/org-1" {
		t.Errorf("meta.Location = %q", meta.Location)
	}
}

func TestSCIMSchemas(t *testing.T) {
	cases := map[string]string{
		SchemaUser:     "urn:ietf:params:scim:schemas:core:2.0:User",
		SchemaGroup:    "urn:ietf:params:scim:schemas:core:2.0:Group",
		SchemaListResp: "urn:ietf:params:scim:api:messages:2.0:ListResponse",
		SchemaError:    "urn:ietf:params:scim:api:messages:2.0:Error",
		SchemaPatchOp:  "urn:ietf:params:scim:api:messages:2.0:PatchOp",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("schema = %q, want %q", got, want)
		}
	}
}
