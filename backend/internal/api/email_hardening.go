package api

import (
	"context"
	"errors"
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

func (h *AuthHandlers) guardEmailRebind(ctx context.Context, oidcSub, email string) error {
	if email == "" {
		return nil
	}
	user, err := h.userRepo.GetUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if user == nil {
		return nil
	}
	if user.OIDCSub == nil || *user.OIDCSub == oidcSub {
		return nil
	}
	return errEmailBoundToAnotherIdentity
}
