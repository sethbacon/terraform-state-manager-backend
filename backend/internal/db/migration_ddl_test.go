package db

import (
	"strings"
	"testing"

	"github.com/sethbacon/terraform-suite-identity/identity/auditoutbox"
	"github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
)

// The platform-admin carrier, the audit outbox and the constraint trigger that
// binds them are defined by the shared identity module, not by this repo. The
// module renders them (platformadmin.TableDDL, auditoutbox.OutboxDDL,
// TriggerSpec.DDL/DropDDL, OutboxDropDDL) precisely so an application does not
// hand-write the contract its statements depend on — three registry migrations
// each had to rediscover the trigger's same-transaction rule the hard way.
//
// golang-migrate reads .sql files, so the rendered statements have to be
// CHECKED IN rather than produced at run time. This test is what makes that
// safe: it re-renders every block and fails when 000030 no longer contains the
// module's own output verbatim. A library upgrade that changes the outbox
// columns, the index set, or the assertion function fails HERE, at
// `go test ./internal/db`, instead of at the first grant against a table whose
// shape the module's statements no longer match.
//
// Substring rather than whole-file equality: the migration carries a header
// explaining the deviations-from-obvious (no foreign keys, no backfill), and
// asserting on the header would make every comment edit a test failure. What
// must not drift is the SQL.

const (
	upMigration   = "migrations/000030_platform_admins.up.sql"
	downMigration = "migrations/000030_platform_admins.down.sql"

	// carrierTable and outboxTable are the names the migration creates and the
	// names internal/platformadmin constructs against. They are spelled the same
	// way in both places on purpose: the carrier's floor lock is namespaced by
	// the table name AS GIVEN, so one process saying "platform_admins" and
	// another saying "public.platform_admins" would address one table under two
	// different advisory locks and lose the serialisation between them.
	carrierTable = "platform_admins"
	outboxTable  = "audit_outbox"
)

// triggerSpec is the trigger the migration installs. Declared here as well as
// in the migration's comment header so this test renders exactly what 000030
// claims to contain — including the deliberate UPDATE guard, which has no
// library constant because the library has no update path.
func triggerSpec() auditoutbox.TriggerSpec {
	return auditoutbox.TriggerSpec{
		Outbox:        outboxTable,
		Table:         carrierTable,
		SubjectColumn: "user_id",
		ResourceType:  platformadmin.AuditResourceType,
		OnInsert:      platformadmin.AuditActionGranted,
		OnUpdate:      "platform_admin.updated",
		OnDelete:      platformadmin.AuditActionRevoked,
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	body, err := migrationsFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func TestMigration000030MatchesLibraryRenderedDDL(t *testing.T) {
	spec := triggerSpec()

	carrierDDL, err := platformadmin.TableDDL(carrierTable)
	if err != nil {
		t.Fatalf("platformadmin.TableDDL: %v", err)
	}
	outboxDDL, err := auditoutbox.OutboxDDL(outboxTable)
	if err != nil {
		t.Fatalf("auditoutbox.OutboxDDL: %v", err)
	}
	triggerDDL, err := spec.DDL()
	if err != nil {
		t.Fatalf("TriggerSpec.DDL: %v", err)
	}
	triggerDrop, err := spec.DropDDL()
	if err != nil {
		t.Fatalf("TriggerSpec.DropDDL: %v", err)
	}
	outboxDrop, err := auditoutbox.OutboxDropDDL(outboxTable)
	if err != nil {
		t.Fatalf("auditoutbox.OutboxDropDDL: %v", err)
	}

	up := readMigration(t, upMigration)
	down := readMigration(t, downMigration)

	for _, tc := range []struct {
		name     string
		file     string
		contents string
		rendered string
	}{
		{"platformadmin.TableDDL", upMigration, up, carrierDDL},
		{"auditoutbox.OutboxDDL", upMigration, up, outboxDDL},
		{"auditoutbox.TriggerSpec.DDL", upMigration, up, triggerDDL},
		{"auditoutbox.TriggerSpec.DropDDL", downMigration, down, triggerDrop},
		{"auditoutbox.OutboxDropDDL", downMigration, down, outboxDrop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Non-vacuity: an empty or trivially short render would make the
			// containment assertion below pass while proving nothing.
			if len(strings.TrimSpace(tc.rendered)) < 40 {
				t.Fatalf("%s rendered %d bytes; the renderer is not producing DDL", tc.name, len(tc.rendered))
			}
			if !strings.Contains(tc.contents, tc.rendered) {
				t.Errorf("%s has drifted from %s.\n"+
					"Re-render it and replace the block verbatim — do not hand-edit the SQL.\n"+
					"--- expected (rendered by the library) ---\n%s\n--- end ---",
					tc.file, tc.name, tc.rendered)
			}
		})
	}
}

// The carrier's own DROP is the one statement the library does not render (it
// owns no table, so it renders no table drop), and it is the one that has to be
// LAST: the constraint trigger dropped above references it.
func TestMigration000030DownDropsTheCarrierAfterItsTrigger(t *testing.T) {
	down := readMigration(t, downMigration)

	carrierDrop := `DROP TABLE IF EXISTS "platform_admins";`
	triggerDrop := `DROP TRIGGER IF EXISTS "platform_admins_require_audit_intent" ON "platform_admins";`

	carrierAt := strings.Index(down, carrierDrop)
	triggerAt := strings.Index(down, triggerDrop)
	if carrierAt < 0 {
		t.Fatalf("%s does not drop the carrier table; rolling back would leave platform_admins behind "+
			"with no trigger and no outbox, so a grant could commit unaudited", downMigration)
	}
	if triggerAt < 0 {
		t.Fatalf("%s does not drop the constraint trigger", downMigration)
	}
	if triggerAt > carrierAt {
		t.Errorf("%s drops platform_admins at byte %d before its trigger at byte %d; "+
			"the trigger must go first", downMigration, carrierAt, triggerAt)
	}
}

// The library refuses a TriggerSpec that guards no operation, and the migration
// must not be the place where that refusal is discovered. Asserting the spec is
// complete here means a future edit that blanks OnDelete — leaving a revocation
// able to commit with no record — fails as a test rather than installing a
// trigger that reads as protection and enforces nothing.
func TestTriggerSpecGuardsEveryOperation(t *testing.T) {
	spec := triggerSpec()
	for _, tc := range []struct{ op, action string }{
		{"INSERT", spec.OnInsert},
		{"UPDATE", spec.OnUpdate},
		{"DELETE", spec.OnDelete},
	} {
		if tc.action == "" {
			t.Errorf("%s on %s is unguarded: an empty action leaves that operation able to "+
				"change the carrier with no audit record", tc.op, carrierTable)
		}
	}
	if spec.OnInsert != "platform_admin.granted" {
		t.Errorf("OnInsert = %q, want platform_admin.granted (platformadmin.AuditActionGranted)", spec.OnInsert)
	}
	if spec.OnDelete != "platform_admin.revoked" {
		t.Errorf("OnDelete = %q, want platform_admin.revoked (platformadmin.AuditActionRevoked)", spec.OnDelete)
	}
	if spec.ResourceType != "platform_admin" {
		t.Errorf("ResourceType = %q, want platform_admin (platformadmin.AuditResourceType)", spec.ResourceType)
	}
}
