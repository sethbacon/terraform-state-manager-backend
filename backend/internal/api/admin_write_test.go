package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestValidRoleTemplateID(t *testing.T) {
	if id, ok := validRoleTemplateID(nil); !ok || id != nil {
		t.Fatalf("nil input: want (nil, true), got (%v, %v)", id, ok)
	}
	empty := "  "
	if id, ok := validRoleTemplateID(&empty); !ok || id != nil {
		t.Fatalf("blank input: want (nil, true), got (%v, %v)", id, ok)
	}
	bad := "not-a-uuid"
	if _, ok := validRoleTemplateID(&bad); ok {
		t.Fatal("malformed uuid accepted")
	}
	good := " 6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0 "
	id, ok := validRoleTemplateID(&good)
	if !ok || id == nil || *id != "6e9a2b62-0e58-4b34-8f4b-2a6f9d3c1ab0" {
		t.Fatalf("valid uuid rejected or not trimmed: (%v, %v)", id, ok)
	}
}

func TestAuditFiltersFromQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET",
		"/admin/audit-logs?action=user.create&resource_type=user&user_email=alice&start_date=2026-06-01T00:00:00Z&end_date=bogus", nil)

	f := auditFiltersFromQuery(c)
	if f.Action == nil || *f.Action != "user.create" {
		t.Fatalf("action filter not mapped: %+v", f.Action)
	}
	if f.ResourceType == nil || *f.ResourceType != "user" {
		t.Fatalf("resource_type filter not mapped: %+v", f.ResourceType)
	}
	if f.UserEmail == nil || *f.UserEmail != "alice" {
		t.Fatalf("user_email filter not mapped: %+v", f.UserEmail)
	}
	want, _ := time.Parse(time.RFC3339, "2026-06-01T00:00:00Z")
	if f.StartDate == nil || !f.StartDate.Equal(want) {
		t.Fatalf("start_date filter not mapped: %+v", f.StartDate)
	}
	// Malformed dates are ignored rather than erroring the whole request.
	if f.EndDate != nil {
		t.Fatalf("bogus end_date should be ignored, got %+v", f.EndDate)
	}
}

func TestCallbackLooksUnreachable(t *testing.T) {
	unreachable := []string{
		"", "http://localhost:8081", "http://127.0.0.1:3000", "http://backend:8080",
		"http://10.1.2.3", "http://192.168.1.5:8081", "http://host.docker.internal:8081",
		"http://thegn.local", "https://api.corp.internal",
	}
	for _, u := range unreachable {
		if !callbackLooksUnreachable(u) {
			t.Errorf("expected unreachable: %q", u)
		}
	}
	reachable := []string{"https://tsm.example.com", "https://abc123.ngrok.app", "http://203.0.113.7:8081"}
	for _, u := range reachable {
		if callbackLooksUnreachable(u) {
			t.Errorf("expected reachable: %q", u)
		}
	}
}
