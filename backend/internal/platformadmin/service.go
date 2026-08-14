// Package platformadmin is TSM's instantiation of the shared platform-admin
// carrier and the transactional audit outbox that records every change to it.
//
// # What is here and what is not
//
// The MECHANISM is the shared identity module's
// (identity/platformadmin, identity/auditoutbox): per-request resolution, the
// never-zero floor, the api-key exclusion, the outbox and its relay. This
// package supplies the POLICY and the plumbing that mechanism is parameterised
// by — which tables, on which connection, resolved against which identity store,
// with which audit vocabulary — and nothing else. Everything that could be
// answered once for the suite is answered there; everything that depends on
// TSM's topology is answered here.
//
// # The two connections, and why the outbox exists
//
// The carrier lives on the APP connection, because "who administers THIS app" is
// per-app authorization state and belongs in TSM's own schema. The audit trail
// lives in identity.audit_logs on the IDENTITY connection, which may be another
// schema or another database (TSM_IDENTITY_DATABASE_*). Those two cannot share a
// transaction, so a mutation cannot write its own audit entry — which is how the
// highest privilege in a product changes hands with no record of it. The outbox
// removes the second write from the request path: the audit INTENT is written in
// the same transaction, on the same connection, as the grant or revocation, and
// a relay delivers it afterwards. A DEFERRABLE constraint trigger (migration
// 000030) makes that a property the database enforces rather than one this code
// intends.
//
// # Phase 2 is additive
//
// SessionScopes here is deliberately NOT the module's carrier-only answer. Until
// TSM's per-app role tables land, `admin` still reaches a session through an
// admin-bearing role template in shared identity, and a carrier-only reading
// would strip that on upgrade — leaving every existing deployment with no
// administrator, no carrier row, and a setup wizard that has already been burnt.
// So the carrier ADDS authority and never removes it yet. See SessionScopes.
package platformadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	idauditoutbox "github.com/sethbacon/terraform-suite-identity/identity/auditoutbox"
	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
	idplatformadmin "github.com/sethbacon/terraform-suite-identity/identity/platformadmin"
	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

// The tables this service addresses.
//
// SPELLED ONCE, AND UNQUALIFIED. The carrier's floor lock is namespaced by the
// table name AS GIVEN, so a deployment that constructed one process with
// "platform_admins" and another with "public.platform_admins" would address one
// table under two different advisory locks and lose the serialisation between
// them. Unqualified is also what lets the app connection's search_path place
// them, which is the same routing every other unqualified name in this repo
// uses. internal/db/migration_ddl_test.go pins migration 000030 to these names.
const (
	// CarrierTable holds one row per platform administrator of THIS deployment.
	CarrierTable = "platform_admins"
	// OutboxTable holds audit intents, on the same connection as the carrier.
	OutboxTable = "audit_outbox"
	// AuditLogsTable is the delivery destination, resolved through the IDENTITY
	// connection's search_path (identity,public) to identity.audit_logs — the
	// same table every other TSM audit write already lands in, so the carrier's
	// history is not a second, separate trail.
	AuditLogsTable = "audit_logs"
)

// ErrUnknownUser is returned by Grant when the target does not resolve to an
// identity user.
//
// THIS GUARD IS THE APPLICATION'S, and the module says so: Carrier.Grant does
// not resolve its target, because only the application knows where its
// principals live. Granting to an id that answers to nobody would mint an
// orphan — a row that elevates no one, counts for nothing in the floor, and sits
// in the administrator list looking like a person.
var ErrUnknownUser = errors.New("platformadmin: no identity user with that id")

// ErrNotConfigured reports a service that was never constructed. Fail-closed:
// nothing is read and nothing is written.
var ErrNotConfigured = errors.New("platformadmin: service is not configured")

// Actor is the acting principal of a carrier mutation, as the request knew it.
//
// Email and IP are captured HERE, on the request path, rather than resolved at
// delivery time: the outbox may deliver minutes later, and identity may be
// another database, so a join across that boundary is both forbidden by the
// model and unable to recover an address the user has since changed. An empty
// Actor is a mutation with no attributable principal — the first-boot bootstrap.
type Actor struct {
	UserID    string
	Email     string
	IPAddress string
}

// Entry is one carrier row as an operator needs to see it.
//
// Exists is the whole reason this type is not just a Grant. The carrier holds no
// foreign key to identity, so a deleted user leaves its row behind; that row
// elevates nobody and does not count towards the floor, but it is still in the
// table and the ONLY surface that can remove it is the one listing it. Labelling
// it beats filtering it: a hidden row cannot be cleaned up.
type Entry struct {
	idplatformadmin.Grant
	Exists bool
}

// Service is TSM's platform-admin carrier plus its audit outbox.
type Service struct {
	carrier  *idplatformadmin.Carrier
	outbox   *idauditoutbox.Outbox
	sink     *idauditoutbox.TableSink
	relay    *idauditoutbox.Relay
	resolver idplatformadmin.Resolver

	// floor builds the never-zero predicate Revoke runs INSIDE its transaction,
	// between the locking read and the DELETE.
	//
	// A field rather than a direct call because that predicate is the only point
	// in the revoking transaction a test can stop at, and forcing the
	// interleaving is the only way a concurrency test can fail without the lock:
	// two goroutines started together do not reliably land in a window a few
	// hundred microseconds wide. Registry learned that the expensive way — its
	// original test passed with AND without the row lock. Always
	// RequireAnotherExercisableAdmin in production; there is no exported way to
	// change it.
	floor func(idplatformadmin.Resolver) idplatformadmin.Predicate
}

// New constructs the service over TSM's two connections.
//
// appDB MUST be the connection the carrier's mutations run on, because the
// outbox is on it too and "the intent commits with the mutation" is the entire
// design. identityDB is where principals resolve and where delivered audit
// records land.
//
// It performs no I/O: a connection that is down is a startup failure to report
// from Verify, with the resolved table names, not a constructor that half-works.
func New(appDB, identityDB *sql.DB) (*Service, error) {
	if appDB == nil {
		return nil, fmt.Errorf("%w: no application database connection", ErrNotConfigured)
	}
	if identityDB == nil {
		return nil, fmt.Errorf("%w: no identity database connection", ErrNotConfigured)
	}

	carrier, err := idplatformadmin.New(appDB, CarrierTable)
	if err != nil {
		return nil, err
	}
	outbox, err := idauditoutbox.New(appDB, OutboxTable)
	if err != nil {
		return nil, err
	}
	// The outbox must be on the SAME handle the carrier mutates through. An
	// outbox pointed at the identity connection would reintroduce exactly the
	// cross-connection split it exists to remove, and the constraint trigger
	// would refuse every commit rather than that being noticed here.
	if outbox.DB() != appDB {
		return nil, fmt.Errorf("%w: the audit outbox is not on the connection the carrier mutates through", ErrNotConfigured)
	}
	sink, err := idauditoutbox.NewTableSink(identityDB, AuditLogsTable)
	if err != nil {
		return nil, err
	}

	s := &Service{
		carrier:  carrier,
		outbox:   outbox,
		sink:     sink,
		resolver: identityResolver{users: idstore.NewUserRepository(identityDB)},
		floor:    idplatformadmin.RequireAnotherExercisableAdmin,
	}
	// No Shipper: TSM has no external SIEM integration, and the durable
	// destination is the audit trail.
	s.relay = idauditoutbox.NewRelay(outbox, sink, nil, idauditoutbox.RelayConfig{})
	return s, nil
}

// Relay is the outbox drain, for the host to start alongside its other
// background jobs. Never nil on a constructed service.
func (s *Service) Relay() *idauditoutbox.Relay {
	if s == nil {
		return nil
	}
	return s.relay
}

// Verify asserts at startup that the three tables this service addresses exist
// on the connections it was given, in the shape the module's statements require,
// and logs where each one actually resolved to.
//
// THE RESOLVED NAMES ARE THE POINT. All three names are unqualified and placed
// by their connection's search_path, so a deployment that changes that path — or
// acquires a second platform_admins in another schema — sees it in these lines
// rather than discovering it as an empty administrator list or an audit trail
// that stopped draining.
//
// A failure is fatal to startup by design. A carrier whose shape is wrong does
// not fail here; it fails at the first grant, in production, on the one
// operation nobody can retry their way out of.
//
// coverage:skip:requires-database
func (s *Service) Verify(ctx context.Context) error {
	if s == nil || s.carrier == nil {
		return ErrNotConfigured
	}
	carrierName, err := s.carrier.VerifyTable(ctx)
	if err != nil {
		return err
	}
	outboxName, err := s.outbox.Verify(ctx)
	if err != nil {
		return err
	}
	sinkName, err := s.sink.Verify(ctx)
	if err != nil {
		return err
	}
	slog.Info("platform-admin carrier ready",
		"carrier", carrierName, "audit_outbox", outboxName, "audit_destination", sinkName)
	return nil
}

// SessionScopes returns the effective scopes for a USER SESSION.
//
// ADDITIVE, FOR THIS PHASE ONLY. The module's Carrier.SessionScopes strips
// `admin` on every return path and re-adds it only from the carrier, which is
// the end state: authority answered per request, from one place, revocable
// immediately. TSM is not there yet — `admin` still reaches a session through an
// admin-bearing role template in shared identity, which is where every existing
// deployment's administrators come from. Shipping the carrier-only reading now
// would strip that on upgrade and leave those deployments with no administrator,
// no carrier row, and a setup wizard that has already been burnt. So the carrier
// can only ADD here: effective admin is `carrier OR the existing scope union`,
// exactly the non-breaking shape registry's own first carrier migration took.
//
// What the carrier already buys, in full, is the direction that matters: a
// principal elevated BY THE CARRIER is elevated per request, from a live read,
// so removing their row takes effect on the next request rather than whenever
// their longest session happens to expire. The legacy half stays a token claim,
// and stays governed by the existing revoke-all watermark (#330), until reads
// move to the carrier alone.
//
// An error is returned WITH the module's stripped scopes rather than the
// caller's, so a caller that chooses to continue continues unelevated. The
// caller should normally abort the request instead: an authority question that
// did not resolve is not a completed "no", and serving it as one downgrades a
// platform administrator to a permission denial during exactly the incident in
// which they need the admin surface.
func (s *Service) SessionScopes(ctx context.Context, userID string, scopes []string) ([]string, error) {
	if s == nil || s.carrier == nil {
		return nil, ErrNotConfigured
	}
	effective, err := s.carrier.SessionScopes(ctx, userID, scopes)
	if err != nil {
		return effective, err
	}
	if !hasAdmin(effective) && hasAdmin(scopes) {
		// carrier.SessionScopes builds its result with make(..., 0, len(scopes)),
		// so this append writes into a backing array it owns; the caller's slice
		// is untouched.
		effective = append(effective, idauth.ScopeAdmin)
	}
	return effective, nil
}

func hasAdmin(scopes []string) bool {
	for _, s := range scopes {
		if s == idauth.ScopeAdmin {
			return true
		}
	}
	return false
}

// List returns every carrier row, oldest grant first, each labelled with whether
// its principal still resolves.
//
// A resolution FAILURE is an error for the whole listing rather than a row
// silently marked absent: an administrator list that quietly reports live
// administrators as orphans during an identity outage is worse than no list,
// because the obvious response to it is to delete them.
func (s *Service) List(ctx context.Context) ([]Entry, error) {
	if s == nil || s.carrier == nil {
		return nil, ErrNotConfigured
	}
	grants, err := s.carrier.List(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(grants))
	for _, g := range grants {
		exists, err := s.resolver.UserExists(ctx, g.UserID)
		if err != nil {
			return nil, fmt.Errorf("%w: resolving platform-admin grant %s: %w",
				idplatformadmin.ErrIdentityUnavailable, g.UserID, err)
		}
		entries = append(entries, Entry{Grant: g, Exists: exists})
	}
	return entries, nil
}

// IsPlatformAdmin reports whether userID holds a carrier row right now.
func (s *Service) IsPlatformAdmin(ctx context.Context, userID string) (bool, error) {
	if s == nil || s.carrier == nil {
		return false, ErrNotConfigured
	}
	return s.carrier.IsPlatformAdmin(ctx, userID)
}

// Grant records platform-admin authority for targetUserID.
//
// The target is RESOLVED FIRST. The module does not do it, on purpose — only the
// application knows where its principals live — so this is where an id that
// answers to nobody is refused with ErrUnknownUser instead of becoming a row
// that looks like an administrator and elevates no one.
//
// Returns idplatformadmin.ErrAlreadyPlatformAdmin when the user already holds a
// row. The existing row is left alone: granted_by/granted_at/note are the
// provenance this table exists to keep, and a re-grant that rewrote them would
// erase who originally conferred the privilege.
//
// The audit intent is written INSIDE the insert's own transaction, so the grant
// and the record of the grant commit together or neither does — and the
// constraint trigger in migration 000030 re-checks that at COMMIT, so this is
// not merely an intention of the code.
func (s *Service) Grant(ctx context.Context, targetUserID string, actor Actor, note *string) (*idplatformadmin.Grant, error) {
	if s == nil || s.carrier == nil {
		return nil, ErrNotConfigured
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return nil, fmt.Errorf("%w: Grant names no principal", ErrNotConfigured)
	}

	exists, err := s.resolver.UserExists(ctx, targetUserID)
	if err != nil {
		return nil, fmt.Errorf("%w: resolving the grant target %s: %w",
			idplatformadmin.ErrIdentityUnavailable, targetUserID, err)
	}
	if !exists {
		return nil, ErrUnknownUser
	}

	grantedBy := optional(actor.UserID)
	intent := s.intent(idplatformadmin.AuditActionGranted, targetUserID, actor, map[string]interface{}{
		"note": derefOr(note, ""),
	})
	return s.carrier.Grant(ctx, targetUserID, grantedBy, note,
		idplatformadmin.AuditIntentWriter(s.outbox.Writer(intent)))
}

// Revoke removes targetUserID's carrier row unless doing so would leave TSM with
// no administrator who could actually exercise the privilege.
//
// TWO LOCKS, FOR TWO DIFFERENT RACES. The module's Revoke reads the carrier
// under FOR UPDATE and deletes in the same transaction, which orders one
// revocation against another. Serialize adds the carrier-wide advisory lock on
// top, which is what orders a revocation against any OTHER authority-reducing
// write this service is asked to run under it. Without the first, two
// administrators revoking each other both see the other still standing and the
// deployment ends with zero — two well-formed requests, no error anywhere.
//
// The predicate is RequireAnotherExercisableAdmin, not a row count: a grant whose
// user has been deleted is a record, not an administrator, and counting it would
// let the last real administrator revoke themselves against a count of two.
//
// RESIDUAL, NAMED RATHER THAN DISCOVERED: nothing else in TSM runs inside
// Serialize yet, so today the advisory lock is REDUNDANT with the row lock for
// revoke-against-revoke, and breaking it changes no test outcome. It is here
// because it is the enlistment point the module prescribes and because the gap
// it closes is real and open: TSM's own user-deletion, GDPR erasure and
// membership-removal paths reduce the exercisable administrator population from
// OTHER tables, on another connection, where FOR UPDATE over this one cannot
// reach them — so a deletion racing a revocation can still take the count below
// one. Wrapping those writes is a change to the user lifecycle rather than to
// the carrier, and it is the next thing to do here.
func (s *Service) Revoke(ctx context.Context, targetUserID string, actor Actor) (*idplatformadmin.Grant, error) {
	if s == nil || s.carrier == nil {
		return nil, ErrNotConfigured
	}
	targetUserID = strings.TrimSpace(targetUserID)
	if targetUserID == "" {
		return nil, fmt.Errorf("%w: Revoke names no principal", ErrNotConfigured)
	}

	intent := s.intent(idplatformadmin.AuditActionRevoked, targetUserID, actor, nil)

	var revoked *idplatformadmin.Grant
	err := s.carrier.Serialize(ctx, func(ctx context.Context) error {
		g, err := s.carrier.Revoke(ctx, targetUserID, s.floor(s.resolver),
			idplatformadmin.AuditIntentWriter(s.outbox.Writer(intent)))
		if err != nil {
			return err
		}
		revoked = g
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// EnsureAdmin grants platform-admin to userID if it is not already held, and
// reports whether a row was created.
//
// THE BOOTSTRAP ENTRY POINT, and idempotent by construction: an existing row is
// left exactly as it was, provenance included, and the call reports success.
// That matters because the first-run wizard step it backs can be replayed, and a
// bootstrap that either failed or rewrote provenance on a second run is a
// bootstrap operators learn to avoid re-running.
//
// It requires no pre-existing platform administrator — it cannot, it is what
// creates the first one. Its caller is responsible for the authorization: in
// TSM's case the setup-token middleware, which is only live before setup
// completes.
//
// grantedBy is NULL: at first boot there is no principal to attribute the grant
// to. The note records where the row came from, so nobody has to guess later.
func (s *Service) EnsureAdmin(ctx context.Context, userID, note string) (created bool, err error) {
	if s == nil || s.carrier == nil {
		return false, ErrNotConfigured
	}
	_, err = s.Grant(ctx, userID, Actor{}, optional(note))
	if errors.Is(err, idplatformadmin.ErrAlreadyPlatformAdmin) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// intent builds the audit record for a carrier mutation.
//
// Action, ResourceType and ResourceID are matched VERBATIM by the constraint
// trigger, so they come from the module's own constants rather than from
// literals spelled again here: a second spelling would fail the COMMIT, which is
// the correct direction to fail but a poor way to find out.
func (s *Service) intent(action, targetUserID string, actor Actor, metadata map[string]interface{}) *idauditoutbox.Intent {
	resourceType := idplatformadmin.AuditResourceType
	return &idauditoutbox.Intent{
		Action:       action,
		ActorUserID:  optional(actor.UserID),
		ActorEmail:   optional(actor.Email),
		ResourceType: &resourceType,
		ResourceID:   &targetUserID,
		IPAddress:    optional(actor.IPAddress),
		// OrganizationID stays nil: administering the deployment is not an act
		// within a tenant, and stamping one would hide the entry from every
		// other organization's audit view.
		Metadata: metadata,
	}
}

// optional returns nil for the empty string, so an absent value reaches Postgres
// as NULL rather than as an empty string that later reads as a real one.
func optional(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}
