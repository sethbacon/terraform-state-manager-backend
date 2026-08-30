package api

import (
	"reflect"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
)

// TestCounterpartyOrganizations pins who learns about a transfer.
//
// The failure this guards is asymmetric. UNDER-disclosure is the bug being
// fixed: a transfer spans two organizations, the row records one, and the
// other has no record its state was copied. But the obvious fix
// over-discloses, and that is worse -- an audit entry attributed to the empty
// organization is shown to EVERY tenant by ListAuditLogs, so a careless
// implementation turns a missing entry into a leak of one organization's
// source ids and state keys into another's audit log.
func TestCounterpartyOrganizations(t *testing.T) {
	const (
		orgA = "11111111-1111-4111-8111-111111111111"
		orgB = "22222222-2222-4222-8222-222222222222"
	)
	src := func(org string) *repositories.Source { return &repositories.Source{OrganizationID: org} }

	for _, c := range []struct {
		name    string
		acting  string
		ends    []*repositories.Source
		want    []string
		because string
	}{
		{
			name:   "cross-organization transfer tells the other end",
			acting: orgA, ends: []*repositories.Source{src(orgA), src(orgB)},
			want:    []string{orgB},
			because: "B's state was party to this and B must be able to see it",
		},
		{
			name:   "the direction does not matter",
			acting: orgA, ends: []*repositories.Source{src(orgB), src(orgA)},
			want:    []string{orgB},
			because: "B is the counterparty whether it was the source or the target",
		},
		{
			name:   "same-organization transfer tells nobody twice",
			acting: orgA, ends: []*repositories.Source{src(orgA), src(orgA)},
			want:    nil,
			because: "the primary entry already covers it; a second would double every ordinary transfer",
		},
		{
			name:   "an unstamped end is not an organization",
			acting: orgA, ends: []*repositories.Source{src(orgA), src("")},
			want:    nil,
			because: "an entry attributed to no organization is shown to EVERY tenant",
		},
		{
			name:   "a nil end is skipped, not dereferenced",
			acting: orgA, ends: []*repositories.Source{src(orgA), nil},
			want:    nil,
			because: "a failed load must not panic the audit path after the transfer already happened",
		},
		{
			name:   "both ends elsewhere are each told once",
			acting: orgA, ends: []*repositories.Source{src(orgB), src(orgB)},
			want:    []string{orgB},
			because: "de-duplicated: one involvement, one entry",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := counterpartyOrganizations(c.acting, c.ends...)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("counterpartyOrganizations = %v, want %v — %s", got, c.want, c.because)
			}
		})
	}
}
