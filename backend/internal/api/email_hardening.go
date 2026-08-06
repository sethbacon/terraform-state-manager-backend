package api

import (
	"context"
	"errors"

	idstore "github.com/sethbacon/terraform-suite-identity/identity/store"
)

var (
	errEmailNotVerified            = errors.New("oidc email is not verified")
	errEmailVerifiedMissing        = errors.New("oidc email_verified claim is required but absent")
	errEmailBoundToAnotherIdentity = errors.New("email is already bound to a different identity")
)

func enforceEmailVerified(verified *bool, require bool) error {
	if verified == nil {
		if require {
			return errEmailVerifiedMissing
		}
		return nil
	}
	if !*verified {
		return errEmailNotVerified
	}
	return nil
}

type claimReader interface{ Claims(v interface{}) error }

func emailVerifiedClaim(r claimReader) *bool {
	var c struct {
		EmailVerified *bool `json:"email_verified"`
	}
	if err := r.Claims(&c); err != nil {
		return nil
	}
	return c.EmailVerified
}

// guardEmailRebind refuses a login whose IdP-asserted email is already bound to
// a DIFFERENT identity, so an IdP that lets a subject change its email cannot
// take over an existing account by claiming its address.
//
// NOT-FOUND IS THE HAPPY PATH HERE. "No user holds this email" is the ordinary
// answer for every first login and every user who changes address to an unused
// one; it is not a failure and must never deny the login. Since
// terraform-suite-identity v0.24.0 the store reports that as an error wrapping
// store.ErrNotFound rather than (nil, nil), so the sentinel is matched FIRST
// and absorbed. Written as a switch rather than a bare `if err != nil` because
// the bare form — which is what this was — turned every login with an
// unclaimed email into a hard failure while still compiling.
//
// Any other error is returned (fails closed): the guard must not be skipped
// because the database was briefly unavailable.
func (h *AuthHandlers) guardEmailRebind(ctx context.Context, oidcSub, email string) error {
	if email == "" {
		return nil
	}
	user, err := h.userRepo.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, idstore.ErrNotFound):
		return nil // email is unclaimed — the ordinary first-login path
	case err != nil:
		return err
	case user == nil:
		// Defensive: a (nil, nil) return is no longer part of the contract, but
		// absorbing it keeps the guard forward- and backward-compatible across
		// the identity bump rather than nil-dereferencing below.
		return nil
	}
	if user.OIDCSub == nil || *user.OIDCSub == oidcSub {
		return nil
	}
	return errEmailBoundToAnotherIdentity
}
