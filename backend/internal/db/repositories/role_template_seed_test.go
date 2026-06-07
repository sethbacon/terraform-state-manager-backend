package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

// fakeExecer records ExecContext calls and returns a configurable error. It
// satisfies systemRoleTemplateExecer without a live database.
type fakeExecer struct {
	calls []seedExecCall
	err   error
}

type seedExecCall struct {
	query string
	args  []any
}

func (f *fakeExecer) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	f.calls = append(f.calls, seedExecCall{query: query, args: args})
	return nil, f.err
}

func TestSeedSystemRoleTemplates_UpsertsEachTemplate(t *testing.T) {
	desc := "Operational access"
	templates := []models.RoleTemplate{
		{Name: "admin", DisplayName: "Administrator", Scopes: []string{"admin"}},
		{Name: "operator", DisplayName: "Operator", Description: &desc, Scopes: []string{"analysis:read", "sources:write"}},
	}

	f := &fakeExecer{}
	if err := SeedSystemRoleTemplates(context.Background(), f, templates); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(f.calls) != len(templates) {
		t.Fatalf("expected %d Exec calls, got %d", len(templates), len(f.calls))
	}
	for i, call := range f.calls {
		if call.query != upsertSystemRoleTemplateQuery {
			t.Errorf("call %d: query did not match the upsert statement", i)
		}
		if len(call.args) != 4 {
			t.Fatalf("call %d: expected 4 args, got %d", i, len(call.args))
		}
		if call.args[0] != templates[i].Name {
			t.Errorf("call %d: name arg = %v, want %v", i, call.args[0], templates[i].Name)
		}
		if call.args[1] != templates[i].DisplayName {
			t.Errorf("call %d: display_name arg = %v, want %v", i, call.args[1], templates[i].DisplayName)
		}
		// Description (*string) passes through by pointer.
		if call.args[2] != templates[i].Description {
			t.Errorf("call %d: description arg did not pass through", i)
		}
		// Scopes are JSON-encoded for the JSONB column.
		scopesJSON, ok := call.args[3].([]byte)
		if !ok {
			t.Fatalf("call %d: scopes arg is %T, want []byte", i, call.args[3])
		}
		var gotScopes []string
		if err := json.Unmarshal(scopesJSON, &gotScopes); err != nil {
			t.Fatalf("call %d: scopes arg is not valid JSON: %v", i, err)
		}
		if !reflect.DeepEqual(gotScopes, templates[i].Scopes) {
			t.Errorf("call %d: scopes = %v, want %v", i, gotScopes, templates[i].Scopes)
		}
	}
}

func TestSeedSystemRoleTemplates_PropagatesError(t *testing.T) {
	f := &fakeExecer{err: errors.New("db unavailable")}
	templates := []models.RoleTemplate{{Name: "viewer", Scopes: []string{"analysis:read"}}}

	err := SeedSystemRoleTemplates(context.Background(), f, templates)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "viewer") {
		t.Errorf("error should name the failing role template, got: %v", err)
	}
}

func TestSeedSystemRoleTemplates_EmptyTemplates(t *testing.T) {
	f := &fakeExecer{}
	if err := SeedSystemRoleTemplates(context.Background(), f, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("expected no Exec calls for empty templates, got %d", len(f.calls))
	}
}
