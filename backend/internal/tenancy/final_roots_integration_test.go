//go:build integration

// THE PHASE 3 READ FLIP FOR THE LAST TWO PARTITION ROOTS — notification_channels
// and state_transfers (#393). With these two, all nine roots read through a
// tenant-scoped reader.
//
// The three files beside this one — scoped_read_integration_test.go,
// schedule_scoped_read_integration_test.go and callback_roots_integration_test.go
// — do the same job for the earlier roots and explain at length why a mock
// cannot answer these questions: `NULL = ANY(ARRAY[...])` is a fact about
// PostgreSQL, and a mock asked about it tells you whatever the person writing
// the mock believed. That warning is not inherited politeness. It was earned
// three times in this estate, most recently by an UPDATE expectation that bound
// no arguments at all, which left a platform-admin scope swap invisible.
//
// # What is different about these two roots
//
// notification_channels is the only root whose reader lives in ANOTHER MODULE.
// identity/notify owns the statement; this application owns the decision about
// which organization is asking. So the thing under test here is the composition
// of the two, and it is exercised through
// repositories.NotificationChannelRepository — the wrapper — rather than through
// the shared DAO, because the wrapper is what the handlers hold and the
// conversion inside it is the part that can be wrong.
//
// It is also the only root whose ROW IS ITSELF A CREDENTIAL. encrypted_target
// holds a Slack or generic webhook URL — anyone who has it can post to it — so
// "the read was withheld" is not enough on this root and the assertions below
// check the SECRET SPECIFICALLY: the target string of one organization's channel
// must not appear in anything another organization's scope produces.
//
// state_transfers is the deliberate two-organization case. The row records ONE
// organization by design and this file asserts exactly that and nothing more
// generous — see transfer_scope.go for why admitting the counterparty here would
// be a leak rather than a courtesy.
//
// EVERY REFUSAL HERE HAS ITS CONTROL. The same row is read back under its
// rightful owner's scope in the same test, because a refusal and a reader that
// returns nothing to anybody are otherwise indistinguishable.
//
// Run with:
//
//	TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
//	  go test -tags integration ./internal/tenancy/...
package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"testing"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/tenantscope"
)

// The two targets are the SECRETS under test. They are distinguishable strings
// so an assertion can say "beta's webhook URL did not appear anywhere in what
// alpha was served" rather than only "the row count was one".
const (
	alphaTarget = "https://hooks.example.test/alpha-only-webhook-secret"
	betaTarget  = "https://hooks.example.test/beta-only-webhook-secret"
)

// seedChannelInOrg writes one notification_channels row and returns its id.
//
// EVERY COLUMN THE READER PROJECTS IS POPULATED, including the ones a lazy
// fixture leaves NULL. last_status/last_error/last_sent_at are set because the
// scan maps them into pointers, and a fixture that left them NULL would make two
// channels compare equal on those fields no matter what the scoped query
// selected — the shortcut that once hid a dropped encrypted_credentials column
// in the state_sources fixture, found only by mutating the reader and watching
// the suite stay green.
func seedChannelInOrg(t *testing.T, db *sql.DB, orgID, name, target string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO notification_channels
			(name, type, encrypted_target, events, enabled,
			 last_status, last_error, last_sent_at, organization_id)
		VALUES ($1, 'webhook', $2, '["drift_detected"]'::jsonb, true,
			 'sent', 'none', now(), $3)
		RETURNING id::text`, name, target, orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed notification channel %q in %s: %v", name, orgID, err)
	}
	return id
}

// seedUnownedChannel writes a channel with NO organization, standing in for a
// row restored from a backup taken before 000034 made the column NOT NULL.
func seedUnownedChannel(t *testing.T, db *sql.DB, name, target string) string {
	t.Helper()
	defer withRelaxedOrganizationNotNull(t, db, "notification_channels")()
	// The column DEFAULT would stamp this row if organization_id were omitted, so
	// it is named and set to NULL explicitly. Omitting it is exactly the mistake
	// that filed every tenant's channel into the default organization before
	// suite-identity#251, and a fixture that made it would not be seeding an
	// unowned row at all.
	var id string
	err := db.QueryRow(`
		INSERT INTO notification_channels
			(name, type, encrypted_target, events, enabled, organization_id)
		VALUES ($1, 'webhook', $2, '[]'::jsonb, true, NULL)
		RETURNING id::text`, name, target).Scan(&id)
	if err != nil {
		t.Fatalf("seed unowned notification channel: %v", err)
	}
	return id
}

// withRelaxedOrganizationNotNull drops the Phase 4 NOT NULL for the duration of
// a fixture and asserts, on the way out, that it CANNOT be restored.
//
// The state under test is a database restored from a pre-000034 backup: 000034
// made organization_id NOT NULL, and the reader still has to answer correctly
// for a row that predates it. The returned closure tries to put the constraint
// back and FAILS THE TEST IF IT SUCCEEDS -- a successful restore would mean no
// unowned row was created, and the test would then be asserting that a STAMPED
// row is invisible to its own tenant, which is a different and much more
// alarming claim than the one it means to make.
func withRelaxedOrganizationNotNull(t *testing.T, db *sql.DB, table string) func() {
	t.Helper()
	// #nosec G202 -- table is a compile-time literal from this file, never input.
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ALTER COLUMN organization_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax the NOT NULL on %s for the unowned fixture: %v", table, err)
	}
	return func() {
		// #nosec G202 -- same literal.
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ALTER COLUMN organization_id SET NOT NULL`); err == nil {
			t.Fatalf("the NOT NULL on %s was restored while an unowned row was supposed to "+
				"exist, so the fixture never created one and this test is not testing what it says",
				table)
		}
	}
}

// seedTransferInOrg writes one state_transfers row and returns its id.
//
// Raw SQL rather than TransferRepository.Create, for the reason seedSourceInOrg
// gives: the subject is the READ path, and going through the writer would make
// this depend on whether the writer stamps the column — a different question,
// answered by TestIntegration_CreateStampsTheActingOrganization.
func seedTransferInOrg(t *testing.T, db *sql.DB, orgID, srcID, targetID, key string) string {
	t.Helper()
	var id string
	err := db.QueryRow(`
		INSERT INTO state_transfers
			(mode, source_id, source_key, target_source_id, target_key, status,
			 verified, decommissioned, detail, actor, organization_id)
		VALUES ('migrate', $1, $2, $3, $4, 'success', true, true, 'parity ok', 'alice', $5)
		RETURNING id::text`, srcID, key, targetID, key+".moved", orgID).Scan(&id)
	if err != nil {
		t.Fatalf("seed transfer in %s: %v", orgID, err)
	}
	return id
}

func channelIDs(items []repositories.NotificationChannel) []string {
	out := make([]string, 0, len(items))
	for _, c := range items {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

// assertChannelFixtureIsNotVacuous is the guard on the guard: an assertion about
// a column can only fail if the fixture put something in it.
func assertChannelFixtureIsNotVacuous(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	var target, org sql.NullString
	var lastStatus sql.NullString
	err := db.QueryRow(
		`SELECT encrypted_target, organization_id::text, last_status
		   FROM notification_channels WHERE id = $1::uuid`, id).Scan(&target, &org, &lastStatus)
	if err != nil {
		t.Fatalf("re-read seeded channel %s: %v", id, err)
	}
	switch {
	case !target.Valid || target.String == "":
		t.Fatal("the fixture left encrypted_target empty, so no assertion below can see " +
			"whether the secret crossed an organization boundary")
	case !org.Valid || org.String == "":
		t.Fatal("the fixture left organization_id unstamped, so the predicate has nothing to match")
	case !lastStatus.Valid:
		t.Fatal("the fixture left the delivery-status columns NULL, so a reader that dropped " +
			"them would compare equal anyway")
	}
}

// ---------------------------------------------------------------------------
// notification_channels
// ---------------------------------------------------------------------------

// The deployment shape almost every TSM install has: ONE organization, the
// `default` one bootstrap.Run creates. The flip must be invisible there.
func TestIntegration_ScopedChannelReads_AreEquivalentInOneOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewNotificationChannelRepository(db)

	id := seedChannelInOrg(t, db, orgAlpha, "ops", alphaTarget)
	seedChannelInOrg(t, db, orgAlpha, "oncall", alphaTarget+"-2")
	assertChannelFixtureIsNotVacuous(t, db, id)

	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}

	unscoped, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	scoped, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	if len(unscoped) != 2 {
		t.Fatalf("the fixture did not produce two channels (%d); the comparison below "+
			"would agree on an empty result", len(unscoped))
	}
	if got, want := channelIDs(scoped), channelIDs(unscoped); !equalStrings(got, want) {
		t.Errorf("in a single-organization deployment the scoped list must return exactly "+
			"what the unscoped one does.\nscoped   = %v\nunscoped = %v", got, want)
	}

	byID, err := repo.GetByIDInScope(ctx, id, scope)
	if err != nil || byID == nil {
		t.Fatalf("GetByIDInScope on the caller's own channel = %+v, %v; want the row", byID, err)
	}
	if byID.EncryptedTarget != alphaTarget {
		t.Errorf("the scoped by-id read did not return the target the notifier needs: %q",
			byID.EncryptedTarget)
	}
}

// TestIntegration_ScopedChannelReads_WithholdAnotherOrganization is the flip,
// and on this root it is a credential boundary rather than a listing one.
func TestIntegration_ScopedChannelReads_WithholdAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewNotificationChannelRepository(db)

	alphaID := seedChannelInOrg(t, db, orgAlpha, "alpha-ops", alphaTarget)
	betaID := seedChannelInOrg(t, db, orgBeta, "beta-ops", betaTarget)
	assertChannelFixtureIsNotVacuous(t, db, betaID)

	alpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	beta := tenantscope.Scope{OrgIDs: []string{orgBeta}}

	// LIST: alpha sees its own channel and not beta's.
	list, err := repo.ListInScope(ctx, alpha)
	if err != nil {
		t.Fatalf("ListInScope(alpha): %v", err)
	}
	if got := channelIDs(list); len(got) != 1 || got[0] != alphaID {
		t.Errorf("alpha's scoped list = %v, want exactly [%s]", got, alphaID)
	}

	// BY ID: beta's channel reads to alpha exactly as one that does not exist.
	got, err := repo.GetByIDInScope(ctx, betaID, alpha)
	if err != nil {
		t.Fatalf("GetByIDInScope(beta's channel, alpha's scope): %v", err)
	}
	if got != nil {
		t.Fatalf("alpha was served beta's notification channel: %+v", got)
	}

	// THE SECRET, SPECIFICALLY. A row withheld and a row served with its target
	// blanked are different failures, and only one of them is safe. This asserts
	// against the WHOLE of what alpha can obtain, so a future reader that started
	// including the target in the list would fail here too.
	assertTargetNotServed(t, ctx, repo, alpha, betaTarget)

	// THE CONTROL. Beta still reads its own channel, WITH the target, or the
	// assertions above would be satisfied by a reader that serves nobody.
	own, err := repo.GetByIDInScope(ctx, betaID, beta)
	if err != nil || own == nil {
		t.Fatalf("CONTROL FAILED: beta cannot read its own channel (%+v, %v). Every refusal "+
			"above is then indistinguishable from a reader that returns nothing at all.", own, err)
	}
	if own.EncryptedTarget != betaTarget {
		t.Errorf("CONTROL FAILED: beta's own channel came back without its target (%q); the "+
			"notifier and the test-send both need it", own.EncryptedTarget)
	}

	// THE MUTATING SIDES, in the same test because a boundary a read enforces and
	// a by-id mutation does not is not a boundary.
	if _, err := repo.UpdateInScope(ctx, betaID, "hijacked", "webhook", nil, true,
		"attacker-sealed-target", alpha); !errors.Is(err, idstore.ErrNotFound) {
		t.Errorf("alpha's scoped UPDATE of beta's channel = %v; want store.ErrNotFound. An "+
			"update carrying a target REDIRECTS the channel, so this is one tenant pointing "+
			"another's alerts at an endpoint they control.", err)
	}
	if err := repo.DeleteInScope(ctx, betaID, alpha); !errors.Is(err, idstore.ErrNotFound) {
		t.Errorf("alpha's scoped DELETE of beta's channel = %v; want store.ErrNotFound", err)
	}
	// And beta's row survived both, which is what makes the two assertions above
	// about the STATEMENT rather than about its error value.
	after, err := repo.GetByIDInScope(ctx, betaID, beta)
	if err != nil || after == nil {
		t.Fatalf("beta's channel did not survive alpha's refused mutations: %+v, %v", after, err)
	}
	if after.Name != "beta-ops" || after.EncryptedTarget != betaTarget {
		t.Errorf("alpha's refused UPDATE still changed beta's row: %+v", after)
	}
}

// assertTargetNotServed checks that a secret does not appear in ANY read the
// scope can perform, not merely in the one the test happened to call.
func assertTargetNotServed(t *testing.T, ctx context.Context,
	repo *repositories.NotificationChannelRepository, scope tenantscope.Scope, secret string) {
	t.Helper()
	list, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	for _, ch := range list {
		if strings.Contains(ch.EncryptedTarget, secret) {
			t.Fatalf("the scoped LIST served another organization's channel target (%s). "+
				"That value is a capability: anyone holding it can post to the endpoint.", ch.ID)
		}
	}
}

func TestIntegration_ScopedChannelReads_FailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewNotificationChannelRepository(db)

	id := seedChannelInOrg(t, db, orgAlpha, "ops", alphaTarget)

	// A scope that was RESOLVED and permits nothing — a caller holding the
	// required scope in no organization. It is an answer, and the answer is
	// nothing; it is not the unresolved case, which the handlers render as 500.
	empty := tenantscope.Scope{}
	if list, err := repo.ListInScope(ctx, empty); err != nil || len(list) != 0 {
		t.Errorf("ListInScope(empty) = %v, %v; want no channels", channelIDs(list), err)
	}
	if got, err := repo.GetByIDInScope(ctx, id, empty); err != nil || got != nil {
		t.Errorf("GetByIDInScope(empty) = %+v, %v; want nothing", got, err)
	}
	if _, err := repo.UpdateInScope(ctx, id, "x", "webhook", nil, true, "", empty); !errors.Is(err, repositories.ErrNotInScope) {
		t.Errorf("UpdateInScope(empty) = %v; want ErrNotInScope", err)
	}
	if err := repo.DeleteInScope(ctx, id, empty); !errors.Is(err, repositories.ErrNotInScope) {
		t.Errorf("DeleteInScope(empty) = %v; want ErrNotInScope", err)
	}
	// THE CONTROL: the row is really there, so the four refusals above are about
	// the scope and not about an empty table.
	if got, err := repo.GetByIDInScope(ctx, id, tenantscope.Scope{OrgIDs: []string{orgAlpha}}); err != nil || got == nil {
		t.Fatalf("CONTROL FAILED: the seeded channel is not readable by its owner either "+
			"(%+v, %v)", got, err)
	}
}

// An unstamped channel belongs to NO tenant and is reachable only by a platform
// admin. `NULL = ANY(...)` is NULL rather than true, so this falls out of the
// predicate — and asserting it is what keeps a row restored from an old backup
// repairable instead of quietly readable by whoever asks first.
func TestIntegration_ScopedChannelReads_UnownedRowsBelongToNoTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewNotificationChannelRepository(db)

	unowned := seedUnownedChannel(t, db, "legacy", "https://hooks.example.test/legacy-secret")
	owned := seedChannelInOrg(t, db, orgAlpha, "ops", alphaTarget)

	alpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	if got, err := repo.GetByIDInScope(ctx, unowned, alpha); err != nil || got != nil {
		t.Errorf("an unstamped channel was served to a tenant scope: %+v, %v", got, err)
	}
	if ids := channelIDs(mustListChannels(t, ctx, repo, alpha)); len(ids) != 1 || ids[0] != owned {
		t.Errorf("alpha's list = %v, want exactly its own channel [%s]", ids, owned)
	}

	admin := tenantscope.Scope{PlatformAdmin: true}
	got, err := repo.GetByIDInScope(ctx, unowned, admin)
	if err != nil || got == nil {
		t.Fatalf("a platform admin cannot reach the unstamped channel (%+v, %v); it is then "+
			"unreachable by everyone and unrepairable", got, err)
	}
	if ids := channelIDs(mustListChannels(t, ctx, repo, admin)); len(ids) != 2 {
		t.Errorf("a platform admin's list = %v, want both channels including the unstamped one", ids)
	}
}

func mustListChannels(t *testing.T, ctx context.Context,
	repo *repositories.NotificationChannelRepository, scope tenantscope.Scope) []repositories.NotificationChannel {
	t.Helper()
	out, err := repo.ListInScope(ctx, scope)
	if err != nil {
		t.Fatalf("ListInScope: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// state_transfers
// ---------------------------------------------------------------------------

func TestIntegration_ScopedTransferReads_AreEquivalentInOneOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewTransferRepository(db)

	src := seedSourceInOrg(t, db, orgAlpha, "alpha-src")
	dst := seedSourceInOrg(t, db, orgAlpha, "alpha-dst")
	id := seedTransferInOrg(t, db, orgAlpha, src, dst, "prod.tfstate")

	scope := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	unscoped, err := repo.GetByID(ctx, id)
	if err != nil || unscoped == nil {
		t.Fatalf("GetByID: %+v, %v", unscoped, err)
	}
	scoped, err := repo.GetByIDInScope(ctx, id, scope)
	if err != nil || scoped == nil {
		t.Fatalf("GetByIDInScope: %+v, %v", scoped, err)
	}
	// Compared through a normaliser rather than with ==: Verified is a *bool, so
	// struct equality compares two heap addresses and reports a difference between
	// two identical rows. A test that "fails" for that reason is one somebody
	// eventually deletes.
	if got, want := normaliseTransfer(scoped), normaliseTransfer(unscoped); got != want {
		t.Errorf("in a single-organization deployment the scoped read must return exactly "+
			"what the unscoped one does.\nscoped   = %+v\nunscoped = %+v", got, want)
	}
	// The fixture is not vacuous: the fields the comparison could disagree on are
	// populated, so an equality that passes means something.
	if scoped.SourceID == "" || scoped.TargetSourceID == "" || scoped.Verified == nil ||
		!scoped.Decommissioned || scoped.Detail == "" || scoped.Actor == "" {
		t.Fatalf("the fixture left projected columns at their zero values: %+v", scoped)
	}
}

// TestIntegration_ScopedTransferReads_WithholdAnotherOrganization covers the
// cross-organization transfer specifically, because that is the case the design
// record calls supported and it is the one where "who may read this" is easy to
// get wrong in either direction.
func TestIntegration_ScopedTransferReads_WithholdAnotherOrganization(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewTransferRepository(db)

	alphaSrc := seedSourceInOrg(t, db, orgAlpha, "alpha-src")
	betaDst := seedSourceInOrg(t, db, orgBeta, "beta-dst")
	// Alpha moves one of its state files INTO beta. The row is stamped with
	// alpha, the acting organization, exactly as doTransfer stamps it.
	crossing := seedTransferInOrg(t, db, orgAlpha, alphaSrc, betaDst, "prod.tfstate")

	betaSrc := seedSourceInOrg(t, db, orgBeta, "beta-src")
	betaOwn := seedTransferInOrg(t, db, orgBeta, betaSrc, betaDst, "beta-internal.tfstate")

	alpha := tenantscope.Scope{OrgIDs: []string{orgAlpha}}
	beta := tenantscope.Scope{OrgIDs: []string{orgBeta}}

	// Beta cannot read a transfer performed by alpha, even though beta owns one
	// of its ENDS. That is the deliberate answer: the record names alpha's source
	// id and alpha's state key, and beta learns of the move through its own audit
	// entry instead (#541).
	if got, err := repo.GetByIDInScope(ctx, crossing, beta); err != nil || got != nil {
		t.Errorf("beta was served a transfer recorded under alpha (%+v, %v). The row names "+
			"alpha's source id and state key; the counterparty's record of the move is its "+
			"audit entry, not this row.", got, err)
	}
	// Alpha cannot read beta's own transfer either — the ordinary direction.
	if got, err := repo.GetByIDInScope(ctx, betaOwn, alpha); err != nil || got != nil {
		t.Errorf("alpha was served beta's transfer: %+v, %v", got, err)
	}

	// THE CONTROLS. Both owners still read their own rows.
	own, err := repo.GetByIDInScope(ctx, crossing, alpha)
	if err != nil || own == nil {
		t.Fatalf("CONTROL FAILED: alpha cannot read the transfer it performed (%+v, %v). "+
			"Deriving the row's organization from the SOURCE instead of the actor would look "+
			"exactly like this.", own, err)
	}
	if own.TargetSourceID != betaDst {
		t.Errorf("alpha's own record lost its target: %+v", own)
	}
	if got, err := repo.GetByIDInScope(ctx, betaOwn, beta); err != nil || got == nil {
		t.Fatalf("CONTROL FAILED: beta cannot read its own transfer (%+v, %v)", got, err)
	}
}

func TestIntegration_ScopedTransferReads_FailClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewTransferRepository(db)

	src := seedSourceInOrg(t, db, orgAlpha, "alpha-src")
	dst := seedSourceInOrg(t, db, orgAlpha, "alpha-dst")
	id := seedTransferInOrg(t, db, orgAlpha, src, dst, "prod.tfstate")

	if got, err := repo.GetByIDInScope(ctx, id, tenantscope.Scope{}); err != nil || got != nil {
		t.Errorf("GetByIDInScope(empty) = %+v, %v; want nothing", got, err)
	}
	if got, err := repo.GetByIDInScope(ctx, id, tenantscope.Scope{OrgIDs: []string{orgAlpha}}); err != nil || got == nil {
		t.Fatalf("CONTROL FAILED: the seeded transfer is not readable by its owner either "+
			"(%+v, %v)", got, err)
	}
}

// An unstamped transfer belongs to no tenant and is reachable only by a platform
// admin, on the same terms as every other root.
func TestIntegration_ScopedTransferReads_UnownedRowsBelongToNoTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := repositories.NewTransferRepository(db)

	src := seedSourceInOrg(t, db, orgAlpha, "alpha-src")
	dst := seedSourceInOrg(t, db, orgAlpha, "alpha-dst")
	var id string
	// organization_id is named and NULLed explicitly: omitting it would let the
	// column DEFAULT stamp the row, and this fixture would not be seeding an
	// unowned transfer at all.
	restore := withRelaxedOrganizationNotNull(t, db, "state_transfers")
	err := db.QueryRow(`
		INSERT INTO state_transfers
			(mode, source_id, source_key, target_source_id, target_key, status, organization_id)
		VALUES ('backup', $1, 'legacy.tfstate', $2, 'legacy.tfstate', 'success', NULL)
		RETURNING id::text`, src, dst).Scan(&id)
	if err != nil {
		t.Fatalf("seed unowned transfer: %v", err)
	}
	restore()

	if got, err := repo.GetByIDInScope(ctx, id, tenantscope.Scope{OrgIDs: []string{orgAlpha}}); err != nil || got != nil {
		t.Errorf("an unstamped transfer was served to a tenant scope: %+v, %v", got, err)
	}
	if got, err := repo.GetByIDInScope(ctx, id, tenantscope.Scope{PlatformAdmin: true}); err != nil || got == nil {
		t.Fatalf("a platform admin cannot reach the unstamped transfer (%+v, %v); it is then "+
			"unreachable by everyone and unrepairable", got, err)
	}
}

// normaliseTransfer flattens the one pointer field so two rows compare by value.
func normaliseTransfer(tr *repositories.Transfer) repositories.Transfer {
	out := *tr
	if tr.Verified != nil {
		if *tr.Verified {
			out.Verified = &verifiedTrue
		} else {
			out.Verified = &verifiedFalse
		}
	}
	return out
}

var (
	verifiedTrue  = true
	verifiedFalse = false
)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
