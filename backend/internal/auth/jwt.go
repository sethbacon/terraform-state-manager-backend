// Package auth - jwt.go handles JWT token creation, signing, and verification
// using a shared secret, including lazy secret initialization and claims parsing.
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

	"github.com/golang-jwt/jwt/v5"
)

var (
	jwtSecret     string
	jwtSecretOnce sync.Once
	jwtSecretErr  error
)

// Claims represents the JWT claims structure
type Claims struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	Scopes []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

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

// ValidateJWTSecret checks that the JWT secret is properly configured.
// In production, this will fail if TSM_JWT_SECRET is not set.
// In dev mode, it will generate a random secret and log a warning.
func ValidateJWTSecret() error {
	jwtSecretOnce.Do(func() {
		secret := os.Getenv("TSM_JWT_SECRET")

		if secret == "" {
			if isDevMode() {
				jwtSecret = generateRandomSecret()
				log.Printf("WARNING: TSM_JWT_SECRET not set. Using auto-generated secret for development.")
				log.Printf("WARNING: Sessions will not persist across restarts. Set TSM_JWT_SECRET for persistent sessions.")
			} else {
				jwtSecretErr = errors.New("SECURITY ERROR: TSM_JWT_SECRET environment variable is required in production. " +
					"Generate a secure secret with: openssl rand -hex 32")
			}
			return
		}

		if len(secret) < 32 {
			log.Printf("WARNING: TSM_JWT_SECRET is shorter than recommended 32 characters. Consider using a longer secret.")
		}

		jwtSecret = secret
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

// GenerateJWT creates a JWT token for an authenticated user.
func GenerateJWT(userID, email string, scopes []string, expiresIn time.Duration) (string, error) {
	if expiresIn == 0 {
		expiresIn = 1 * time.Hour
	}

	claims := &Claims{
		UserID: userID,
		Email:  email,
		Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiresIn)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "terraform-state-manager",
			Subject:   userID,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	secret := GetJWTSecret()

	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// ValidateJWT parses and validates a JWT token
func ValidateJWT(tokenString string) (*Claims, error) {
	secret := GetJWTSecret()

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, errors.New("invalid token")
	}

	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, errors.New("invalid claims type")
	}

	return claims, nil
}
