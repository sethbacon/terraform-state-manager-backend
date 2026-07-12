// Package auth wires the Terraform State Manager's authentication on top of the
// shared terraform-suite-identity module: JWT signing/validation (TokenManager),
// the OIDC provider adapter, and the app-owned scope/role definitions.
//
// jwt.go resolves the signing secret (env or dev-ephemeral) and delegates token
// creation/validation to the identity TokenManager.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	idauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
)

// jwtIssuer stamps the iss claim on tokens this service generates.
const jwtIssuer = "terraform-state-manager"

// siblingIssuer is terraform-registry-backend's own jwtIssuer value. In the
// shared-secret coupled suite (ADR/suite-coupling.md) both backends sign with
// the same secret, so without an issuer pin a token minted by the sibling
// would validate here unchanged. This is the same trusted-sibling issuer
// string terraform-registry-backend already expects from this app (see its
// own jwt.go / SetTrustedIssuers) and that this repo's suite integration
// already hardcodes (internal/api/suite.go, internal/api/suite_test.go).
const siblingIssuer = "terraform-registry"

// Claims is the shared identity JWT claims type, re-exported so call sites refer
// to auth.Claims. It carries a JTI used for revocation.
type Claims = idauth.Claims

var (
	jwtSecretOnce sync.Once
	jwtSecretErr  error
	tokenManager  *idauth.TokenManager
)

// IsDevMode reports whether DEV_MODE is explicitly enabled.
func IsDevMode() bool {
	d := os.Getenv("DEV_MODE")
	return d == "true" || d == "1"
}

func generateRandomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random JWT secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ValidateJWTSecret resolves the signing secret and constructs the TokenManager.
// In production TSM_JWT_SECRET is required; in dev mode an ephemeral secret is
// generated (sessions reset on restart). Call once at startup.
func ValidateJWTSecret() error {
	jwtSecretOnce.Do(func() {
		secret := os.Getenv("TSM_JWT_SECRET")
		if secret == "" {
			if IsDevMode() {
				rnd, err := generateRandomSecret()
				if err != nil {
					jwtSecretErr = err
					return
				}
				secret = rnd
				log.Println("WARNING: TSM_JWT_SECRET not set; using an ephemeral dev secret (sessions reset on restart).")
			} else {
				jwtSecretErr = errors.New("TSM_JWT_SECRET is required in production (generate with: openssl rand -hex 32)")
				return
			}
		} else if len(secret) < 32 {
			log.Println("WARNING: TSM_JWT_SECRET is shorter than the recommended 32 characters.")
		}
		tokenManager = idauth.NewTokenManager(secret, jwtIssuer)
		// Pin validation to the trusted issuer set (this app plus the sibling
		// terraform-registry-backend). Without this, Validate accepts a token
		// bearing ANY iss claim as long as it is signed with the shared secret —
		// in the coupled suite deployment this app is the more exposed of the
		// two backends (terraform-suite-identity audit #51 / issue #178), since
		// terraform-registry-backend already pins its own allowed issuers.
		tokenManager.SetAllowedIssuers([]string{jwtIssuer, siblingIssuer})
		// Stamp/require this app's own identity as the audience (issue #178,
		// completed now that terraform-suite-identity v0.17.0 is adopted). An
		// issuer pin alone still lets a trusted sibling's token through
		// unchanged; SetAudience closes that gap by additionally requiring a
		// token — even one from a trusted sibling issuer — to have been minted
		// FOR this app specifically. Safe to enable unconditionally: Validate
		// only enforces the check once set, so a standalone (non-coupled)
		// deployment is unaffected beyond every token now also carrying/
		// requiring its own aud claim.
		tokenManager.SetAudience(jwtIssuer)
	})
	return jwtSecretErr
}

// GenerateJWT issues a signed JWT embedding the user's scopes.
func GenerateJWT(userID, email string, scopes []string, expiresIn time.Duration) (string, error) {
	if err := ValidateJWTSecret(); err != nil {
		return "", err
	}
	return tokenManager.Generate(userID, email, scopes, expiresIn)
}

// ValidateJWT parses and validates a JWT, returning its claims.
func ValidateJWT(tokenString string) (*Claims, error) {
	if err := ValidateJWTSecret(); err != nil {
		return nil, err
	}
	return tokenManager.Validate(tokenString)
}
