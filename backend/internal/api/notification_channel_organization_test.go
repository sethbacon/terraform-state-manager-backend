package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// notification_channels, the last of #436's nine partition roots, and the only
// one whose INSERT lives outside this repository.
//
// WHAT WAS ACTUALLY WRONG. The shared identity module's ChannelRepository.Create
// omitted organization_id from the statement on the reasoning that a partitioning
// consumer assigns the owner with a column DEFAULT in its own migration. Omission
// is exactly when a Postgres DEFAULT fires, so the column was never NULL -- it
// was tsm_default_organization_id(), one fixed organization, for every tenant.
// That is worse than NULL for this table: the row is well-formed, invisible to
// the tenant that created it, visible to whoever owns the default organization,
// and non-NULL, so the boot backfill that repairs NULLs never revisits it. The
// channel's encrypted_target is a capability-bearing secret, so the misfiled row
// carries a usable webhook URL with it.
//
// suite-identity#251 added WithOwningOrganization; these tests cover this side of
// it. The shared module's own PostgreSQL integration tests cover the half a mock
// cannot see -- whether the DEFAULT actually fired -- because sqlmock returns
// whatever the fixture declares and cannot tell an omitted column from a named
// one. What it CAN see is asserted here: the statement text and the bound value.

func TestCreateChannel_IsStampedWithTheActingOrganization(t *testing.T) {
	e := newNotificationsEnv(t)

	// The regex REQUIRES organization_id in the INSERT: if the consumer stops
	// passing WithOwningOrganization, the shared module omits the column again and
	// no expectation matches.
	e.mock.ExpectQuery(`INSERT INTO notification_channels[\s\S]*organization_id`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), testActingOrg).
		WillReturnRows(notifChannelRow(t, "https://hooks.example.com/x"))

	w := e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"https://hooks.example.com/x","events":["drift_detected"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (%s)", w.Code, w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the INSERT did not name organization_id, or bound the wrong value -- "+
			"the channel is being filed at the schema DEFAULT: %v", err)
	}
}

// TestCreateChannel_UnresolvedScopeIsRefusedBeforeTheWrite keeps the distinction
// #436 turns on: an unresolved scope is a WIRING fault, not an empty one. If this
// route's middleware.TenantScope came unwired, treating the absence as "no
// memberships" would quietly resume filing channels at the DEFAULT -- the exact
// bug, silently restored.
func TestCreateChannel_UnresolvedScopeIsRefusedBeforeTheWrite(t *testing.T) {
	e := newNotificationsEnvWithoutScope(t)
	// No expectations: reaching the database at all is the failure.

	w := e.do(http.MethodPost, "/api/v1/notifications/channels",
		`{"name":"ops","type":"webhook","target":"https://hooks.example.com/x","events":["drift_detected"]}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tenant scope was not resolved") {
		t.Fatalf("500 for the wrong reason: %s", w.Body.String())
	}
	// And exactly ONE response. A handler that writes the refusal and then carries
	// on still emits this message before failing further down, so the substring
	// check above passes either way -- two concatenated JSON objects do not
	// unmarshal, and that is what proves the handler actually stopped.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the handler wrote more than one response (%s): the refusal did not "+
			"return, so the request continued past it", w.Body.String())
	}
	if err := e.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a statement ran with no tenant scope resolved: %v", err)
	}
}
