package api

import (
	"strings"
	"testing"
)

// THE DERIVATION, TESTED ON THE DERIVATION — #393 option B, item 5.
//
// This is the test the previous increment initially lacked, and the reason it
// has to exist separately from every live-PostgreSQL predicate test beside it.
// A predicate test builds its scope BY HAND and then asks whether the SQL
// honours it. That answers "does an organization scope filter rows", which is a
// question about PostgreSQL. It cannot answer "is the scope this code hands to
// that predicate the right one", which is a question about these twenty lines —
// and a derivation that returned a platform-admin scope would pass every
// predicate test in the repository, because a platform-admin scope makes each
// InScope reader take its documented bypass branch and serve any organization's
// row.
//
// So the two halves are tested in two places on purpose: the predicate against a
// real database, and the derivation here, where it can be inspected directly.

const (
	authOrgA = "11111111-1111-4111-8111-111111111111"
	authOrgB = "22222222-2222-4222-8222-222222222222"
)

// TestAuthenticateCallback_ConfersExactlyTheRunsOrganization is the success
// direction, and every clause of it is load-bearing.
func TestAuthenticateCallback_ConfersExactlyTheRunsOrganization(t *testing.T) {
	auth, ok := authenticateCallback("drift_runs",
		callbackRun{ID: "d1", OrganizationID: authOrgA, StoredToken: "tok"}, "tok")
	if !ok {
		t.Fatal("a matching token on a stamped run did not authenticate; every legitimate " +
			"CI callback in the product goes through this path")
	}

	// EXACTLY ONE ORGANIZATION, and it is the run's.
	if got := auth.scope.OrgIDs; len(got) != 1 || got[0] != authOrgA {
		t.Errorf("derived scope = %v, want exactly [%s]. More than one organization would be an "+
			"authority the run never conferred; a different one would be somebody else's.", got, authOrgA)
	}
	if auth.organizationID != authOrgA {
		t.Errorf("auth.organizationID = %q, want %q — this is the value a row created under "+
			"this authority is stamped with", auth.organizationID, authOrgA)
	}

	// NEVER THE PLATFORM-ADMIN BYPASS. This is the mutation that survives every
	// predicate test: PlatformAdmin makes GetByIDInScope return r.GetByID, so a
	// callback would serve and write any organization's row while every
	// integration assertion about the predicate still passed.
	if auth.scope.PlatformAdmin {
		t.Error("the derived scope carries PlatformAdmin. That carrier means a live-checked human " +
			"administrator, and it takes the UNFILTERED branch of every InScope reader — so a CI job " +
			"holding one run's token would reach every organization's rows. A machine callback must " +
			"never produce it.")
	}
	// ...and it must not be empty either, which is the opposite failure: an
	// authority that reads nothing looks like a refusal and would break every
	// callback instead of scoping it.
	if auth.scope.Empty() {
		t.Error("the derived scope is empty, so the callback would read and write nothing at all")
	}

	// PROVENANCE, which is the auditable half of the decision: a refusal
	// downstream must be able to name the row the authority came from.
	if !auth.system {
		t.Error("the derived authority is not marked system-derived, so a log line cannot tell it " +
			"apart from a request-resolved one")
	}
	if want := "system:drift_runs/d1"; auth.origin != want {
		t.Errorf("auth.origin = %q, want %q", auth.origin, want)
	}
}

// TestAuthenticateCallback_YieldsNoAuthorityWhenItDoesNotAuthenticate is THE
// FAILURE DIRECTION, and it is the one that decides whether this mechanism is
// worth anything.
//
// Each case must produce an authority that permits NOTHING — not a narrower one,
// not one that happens to be unused because the caller checked the bool. The
// zero dispatchAuthority carries the zero Scope, which every InScope reader
// treats as "read nothing, without a query", so a caller that ignored the bool
// would still reach no row.
func TestAuthenticateCallback_YieldsNoAuthorityWhenItDoesNotAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		run       callbackRun
		presented string
		why       string
	}{
		{
			name:      "a wrong token",
			run:       callbackRun{ID: "d1", OrganizationID: authOrgA, StoredToken: "right"},
			presented: "wrong",
			why:       "the ordinary forged-callback case",
		},
		{
			name:      "no token presented",
			run:       callbackRun{ID: "d1", OrganizationID: authOrgA, StoredToken: "right"},
			presented: "",
			why:       "a caller who sent no credential at all",
		},
		{
			name:      "an already-consumed run",
			run:       callbackRun{ID: "d1", OrganizationID: authOrgA, StoredToken: ""},
			presented: "",
			why: "THE ONE THAT WOULD BE MISSED BY A PLAIN COMPARE: crypto/subtle's " +
				"ConstantTimeCompare(\"\", \"\") returns 1, so a run whose token has already been " +
				"consumed (or was cleared by the stuck-run reconciler) would authenticate a caller " +
				"who presented nothing",
		},
		{
			name:      "an already-consumed run, with a token presented",
			run:       callbackRun{ID: "d1", OrganizationID: authOrgA, StoredToken: ""},
			presented: "anything",
			why:       "a replay after the one-shot token was cleared",
		},
		{
			name:      "a run belonging to no organization",
			run:       callbackRun{ID: "d1", OrganizationID: "", StoredToken: "right"},
			presented: "right",
			why: "a database restored from a backup taken before 000034 holds unstamped rows. " +
				"A run that belongs to no organization confers authority over none — deriving " +
				"'everything' from it is the unpartitioned read this whole issue is about",
		},
		{
			name:      "a run belonging to a blank-but-present organization",
			run:       callbackRun{ID: "d1", OrganizationID: "   ", StoredToken: "right"},
			presented: "right",
			why:       "whitespace is not an organization; SystemActingIn trims before deciding",
		},
		{
			name:      "a run with no id",
			run:       callbackRun{ID: "", OrganizationID: authOrgA, StoredToken: "right"},
			presented: "right",
			why: "an authority nobody can trace back to a row is ambient authority with better " +
				"manners; SystemActingIn requires the provenance",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, ok := authenticateCallback("drift_runs", tc.run, tc.presented)
			if ok {
				t.Fatalf("authenticated: %s", tc.why)
			}
			if !auth.scope.Empty() {
				t.Errorf("refused but produced scope %+v; a caller that ignored the bool would still "+
					"reach rows. %s", auth.scope, tc.why)
			}
			if auth.scope.PlatformAdmin {
				t.Errorf("refused but produced a platform-admin scope, which reads EVERYTHING: %s", tc.why)
			}
			if auth.organizationID != "" || auth.origin != "" {
				t.Errorf("refused but produced an authority naming organization %q from %q",
					auth.organizationID, auth.origin)
			}
		})
	}
}

// TestAuthenticateCallback_BindsTheTokenToItsOwnRun is the property the name
// "token-to-organization binding" actually means: the organization comes from
// the run the presented token belongs to, and from nowhere else.
//
// The two runs here hold DIFFERENT tokens in DIFFERENT organizations, which is
// the arrangement a transposed argument or a shared package variable would get
// wrong while both single-run cases above still passed.
func TestAuthenticateCallback_BindsTheTokenToItsOwnRun(t *testing.T) {
	runA := callbackRun{ID: "d-a", OrganizationID: authOrgA, StoredToken: "token-a"}
	runB := callbackRun{ID: "d-b", OrganizationID: authOrgB, StoredToken: "token-b"}

	authA, okA := authenticateCallback("drift_runs", runA, "token-a")
	authB, okB := authenticateCallback("drift_runs", runB, "token-b")
	if !okA || !okB {
		t.Fatalf("both runs must authenticate with their own tokens (a=%v b=%v)", okA, okB)
	}
	if authA.organizationID == authB.organizationID {
		t.Fatalf("both callbacks derived organization %q; the organization is not coming from the "+
			"run", authA.organizationID)
	}
	if authA.organizationID != authOrgA || authB.organizationID != authOrgB {
		t.Fatalf("derived organizations are crossed: a=%q b=%q", authA.organizationID, authB.organizationID)
	}

	// A's token against B's run authenticates nothing. Ids are not secrets — the
	// list endpoints hand them out — so this is the shape an attacker with one
	// legitimate run would try.
	if _, ok := authenticateCallback("drift_runs", runB, "token-a"); ok {
		t.Error("run B authenticated with run A's token: the callback token is not bound to its run")
	}
	if _, ok := authenticateCallback("drift_runs", runA, "token-b"); ok {
		t.Error("run A authenticated with run B's token")
	}
}

// TestAuthenticateCallback_ProvenanceNamesTheTable keeps the two callbacks
// distinguishable in a log line. Both post to a /runs/:id/results route and both
// derive their authority the same way, so an origin that did not say which table
// would send an operator to the wrong one.
func TestAuthenticateCallback_ProvenanceNamesTheTable(t *testing.T) {
	for _, table := range []string{"drift_runs", "health_runs"} {
		auth, ok := authenticateCallback(table, callbackRun{ID: "x1", OrganizationID: authOrgA, StoredToken: "t"}, "t")
		if !ok {
			t.Fatalf("%s: did not authenticate", table)
		}
		if !strings.Contains(auth.origin, table) {
			t.Errorf("%s: origin = %q, which does not name the table the authority came from", table, auth.origin)
		}
	}
}

// TestCallbackTokenFrom_HeaderWinsAndBlankFallsThrough pins the one piece of
// precedence both callbacks share. A blank header must fall through to the body
// rather than being presented as an empty token — which, on a run whose token
// has already been consumed, is the pair the compare above refuses explicitly.
func TestCallbackTokenFrom_HeaderWinsAndBlankFallsThrough(t *testing.T) {
	for _, tc := range []struct{ header, body, want string }{
		{"h", "b", "h"},
		{"", "b", "b"},
		{"   ", "b", "b"},
		{"h", "", "h"},
		{"", "", ""},
	} {
		if got := callbackTokenFrom(tc.header, tc.body); got != tc.want {
			t.Errorf("callbackTokenFrom(%q, %q) = %q, want %q", tc.header, tc.body, got, tc.want)
		}
	}
}
