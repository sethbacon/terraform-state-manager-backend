package auth

import "testing"

// ReadWritePairs feeds store.OrganizationRepository.OrgScopeForUser, which
// resolves a caller's tenancy. If it ever returned the live map instead of a
// copy, a caller mutating the result would silently widen THIS APP'S
// authorization table for every subsequent request in the process.
func TestReadWritePairs_ReturnsACopyTheCallerCannotWiden(t *testing.T) {
	first := ReadWritePairs()
	original := len(first)
	if original == 0 {
		t.Fatal("no write-implies-read pairs; OrgScopeForUser would resolve every caller as if no write scope implied its read sibling")
	}

	// Mutating the returned table must not reach the package's own.
	first["totally:invented"] = "totally:invented"
	for read := range first {
		delete(first, read)
		break
	}

	second := ReadWritePairs()
	if _, leaked := second["totally:invented"]; leaked {
		t.Fatal("an injected pair survived into a later call; the caller widened this app's authorization table")
	}
	if len(second) != original {
		t.Fatalf("table size changed across calls (%d then %d); a caller's mutation reached the shared table", original, len(second))
	}
}

// The pairs must actually express write-implies-read, in that direction only —
// a table that mapped read to itself would satisfy a length check while
// granting nothing, and one that mapped read->write would grant too much.
func TestReadWritePairs_MapsReadToItsWriteSiblingNotTheReverse(t *testing.T) {
	for read, write := range ReadWritePairs() {
		if read == write {
			t.Fatalf("scope %q maps to itself; a write scope would stop satisfying its read sibling", read)
		}
		if _, isAlsoAKey := ReadWritePairs()[write]; isAlsoAKey {
			t.Fatalf("write scope %q is also used as a read key; the direction is ambiguous", write)
		}
	}
}
