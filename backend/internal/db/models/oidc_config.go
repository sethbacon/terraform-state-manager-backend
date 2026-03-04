package models

import (
	"strings"
	"time"
)

type OIDCConfig struct {
	ID                    string    `db:"id" json:"id"`
	IssuerURL             string    `db:"issuer_url" json:"issuer_url"`
	ClientID              string    `db:"client_id" json:"client_id"`
	ClientSecretEncrypted string    `db:"client_secret_encrypted" json:"-"`
	RedirectURL           string    `db:"redirect_url" json:"redirect_url"`
	ScopesJSON            string    `db:"scopes" json:"-"`
	IsActive              bool      `db:"is_active" json:"is_active"`
	CreatedAt             time.Time `db:"created_at" json:"created_at"`
	UpdatedAt             time.Time `db:"updated_at" json:"updated_at"`
}

func (c *OIDCConfig) GetScopes() []string {
	if c.ScopesJSON == "" {
		return []string{"openid", "email", "profile"}
	}
	// Simple comma-separated parsing
	scopes := []string{}
	current := ""
	for _, ch := range c.ScopesJSON {
		if ch == ',' {
			if trimmed := strings.TrimSpace(current); trimmed != "" {
				scopes = append(scopes, trimmed)
			}
			current = ""
		} else {
			current += string(ch)
		}
	}
	if trimmed := strings.TrimSpace(current); trimmed != "" {
		scopes = append(scopes, trimmed)
	}
	return scopes
}
