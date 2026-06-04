// Package auth - jwt.go resolves the JWT signing secret from the environment and
// delegates token creation/validation to the shared identity TokenManager.
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

	identityauth "github.com/sethbacon/terraform-suite-identity/identity/auth"
)

// jwtIssuer stamps the iss claim on tokens this service generates.
const jwtIssuer = "terraform-state-manager"

// Claims is the suite identity JWT claims type, re-exported so existing call
// sites keep referring to auth.Claims.
type Claims = identityauth.Claims

var (
	jwtSecret     string
	jwtSecretOnce sync.Once
	jwtSecretErr  error
	tokenManager  *identityauth.TokenManager
)

// isDevMode checks if we're in development mode
func isDevMode() bool {
	devMode := os.Getenv("DEV_MODE")
	nodeEnv := os.Getenv("NODE_ENV")
	ginMode := os.Getenv("GIN_MODE")

	return devMode == "true" || devMode == "1" ||
		nodeEnv == "development" ||
		ginMode == "debug"
}

// generateRandomSecret creates a cryptographically secure random secret
func generateRandomSecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("dev-fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

func configuredJWTSecret() string {
	if shared := os.Getenv("TSM_AUTH_JWT_SECRET"); shared != "" {
		return shared
	}
	return os.Getenv("TSM_JWT_SECRET")
}

// ValidateJWTSecret resolves and caches the JWT signing secret and constructs the
// shared TokenManager. In production it fails if neither TSM_AUTH_JWT_SECRET nor
// TSM_JWT_SECRET is set; in dev mode it generates an ephemeral secret and warns.
func ValidateJWTSecret() error {
	jwtSecretOnce.Do(func() {
		secret := configuredJWTSecret()

		if secret == "" {
			if isDevMode() {
				jwtSecret = generateRandomSecret()
				log.Printf("WARNING: TSM_AUTH_JWT_SECRET/TSM_JWT_SECRET not set. Using auto-generated secret for development.")
				log.Printf("WARNING: Sessions will not persist across restarts. Set TSM_AUTH_JWT_SECRET (preferred) for persistent sessions.")
			} else {
				jwtSecretErr = errors.New("SECURITY ERROR: TSM_AUTH_JWT_SECRET (or legacy TSM_JWT_SECRET) environment variable is required in production. " +
					"Generate a secure secret with: openssl rand -hex 32")
				return
			}
		} else {
			if len(secret) < 32 {
				log.Printf("WARNING: JWT secret is shorter than recommended 32 characters. Consider using a longer secret.")
			}
			jwtSecret = secret
		}

		tokenManager = identityauth.NewTokenManager(jwtSecret, jwtIssuer)
	})

	return jwtSecretErr
}

// GetJWTSecret retrieves the validated JWT secret.
func GetJWTSecret() string {
	if jwtSecret == "" {
		if err := ValidateJWTSecret(); err != nil {
			panic(err)
		}
	}
	return jwtSecret
}

// GenerateJWT creates a JWT token for an authenticated user, delegating to the
// shared identity TokenManager.
func GenerateJWT(userID, email string, scopes []string, expiresIn time.Duration) (string, error) {
	_ = GetJWTSecret() // ensure the secret is validated and the TokenManager is initialised
	return tokenManager.Generate(userID, email, scopes, expiresIn)
}

// ValidateJWT parses and validates a JWT token via the shared identity TokenManager.
func ValidateJWT(tokenString string) (*Claims, error) {
	_ = GetJWTSecret()
	return tokenManager.Validate(tokenString)
}
