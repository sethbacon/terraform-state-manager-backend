// Package ldap implements LDAP / Active Directory search-bind authentication:
// bind a service account, search for the user, bind as the user to verify the
// password, then look up group memberships for role mapping.
//
// SECURITY: all user-influenced values (username, user DN) are escaped with
// goldap.EscapeFilter before being placed into an LDAP filter, so a crafted
// username cannot alter the filter (LDAP injection). The user DN is found by
// search — never constructed from input. Empty usernames/passwords are rejected
// up front to defeat LDAP "unauthenticated bind", where a bind with a valid DN
// and an empty password succeeds on many servers without authenticating.
package ldap

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"

	goldap "github.com/go-ldap/ldap/v3"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// Provider performs LDAP search-bind authentication.
type Provider struct {
	cfg config.LDAPConfig
}

// UserInfo holds the attributes extracted from LDAP for an authenticated user.
type UserInfo struct {
	DN     string
	Email  string
	Name   string
	Groups []string // group DNs
}

// NewProvider validates configuration and returns a provider. Connections are
// opened lazily per Authenticate call.
func NewProvider(cfg config.LDAPConfig) (*Provider, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("ldap: not enabled")
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("ldap: host is required")
	}
	if cfg.BaseDN == "" {
		return nil, fmt.Errorf("ldap: base_dn is required")
	}
	if cfg.BindDN == "" {
		return nil, fmt.Errorf("ldap: bind_dn is required for search-bind")
	}
	if cfg.UserFilter == "" {
		return nil, fmt.Errorf("ldap: user_filter is required")
	}
	if !strings.Contains(cfg.UserFilter, "%s") {
		return nil, fmt.Errorf("ldap: user_filter must contain %%s for the username")
	}

	c := cfg
	if c.Port == 0 {
		if c.UseTLS {
			c.Port = 636
		} else {
			c.Port = 389
		}
	}
	if c.UserAttrEmail == "" {
		c.UserAttrEmail = "mail"
	}
	if c.UserAttrName == "" {
		c.UserAttrName = "displayName"
	}
	if c.GroupMemberAttr == "" {
		c.GroupMemberAttr = "member"
	}
	return &Provider{cfg: c}, nil
}

// Authenticate performs search-bind authentication and returns the user's info.
func (p *Provider) Authenticate(username, password string) (*UserInfo, error) {
	// Harden against LDAP unauthenticated/anonymous bind: a non-empty password is
	// required, and an empty username can never identify a user.
	if strings.TrimSpace(username) == "" || password == "" {
		return nil, fmt.Errorf("ldap: username and password are required")
	}

	conn, err := p.dial()
	if err != nil {
		return nil, fmt.Errorf("ldap: connect failed: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Bind service account, find the user, then bind as the user to verify the password.
	if err := conn.Bind(p.cfg.BindDN, p.cfg.BindPassword); err != nil {
		return nil, fmt.Errorf("ldap: service account bind failed: %w", err)
	}
	userDN, email, name, err := p.searchUser(conn, username)
	if err != nil {
		return nil, err
	}
	if err := conn.Bind(userDN, password); err != nil {
		return nil, fmt.Errorf("ldap: authentication failed for user %q", username)
	}

	// Re-bind service account to look up groups (best-effort).
	if err := conn.Bind(p.cfg.BindDN, p.cfg.BindPassword); err != nil {
		return nil, fmt.Errorf("ldap: re-bind as service account failed: %w", err)
	}
	groups, err := p.lookupGroups(conn, userDN)
	if err != nil {
		slog.Warn("ldap: group lookup failed; continuing without groups", "user", username, "error", err)
		groups = nil
	}

	return &UserInfo{DN: userDN, Email: email, Name: name, Groups: groups}, nil
}

func (p *Provider) dial() (*goldap.Conn, error) {
	addr := fmt.Sprintf("%s:%d", p.cfg.Host, p.cfg.Port)
	tlsConfig := &tls.Config{
		ServerName:         p.cfg.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: p.cfg.InsecureSkipVerify, // #nosec G402 -- admin opt-in, dev only; defaults false
	}
	if p.cfg.UseTLS {
		conn, err := goldap.DialURL("ldaps://"+addr, goldap.DialWithTLSConfig(tlsConfig))
		if err != nil {
			return nil, fmt.Errorf("ldaps dial failed: %w", err)
		}
		return conn, nil
	}
	conn, err := goldap.DialURL("ldap://" + addr)
	if err != nil {
		return nil, fmt.Errorf("ldap dial failed: %w", err)
	}
	if p.cfg.StartTLS {
		if err := conn.StartTLS(tlsConfig); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("StartTLS failed: %w", err)
		}
	}
	return conn, nil
}

func (p *Provider) searchUser(conn *goldap.Conn, username string) (dn, email, name string, err error) {
	// Escape the username before substituting it into the admin-configured filter.
	filter := fmt.Sprintf(p.cfg.UserFilter, goldap.EscapeFilter(username))
	req := goldap.NewSearchRequest(
		p.cfg.BaseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		2, 10, false, // size limit 2 (so we can detect ambiguity), time limit 10s
		filter, []string{"dn", p.cfg.UserAttrEmail, p.cfg.UserAttrName}, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return "", "", "", fmt.Errorf("ldap: user search failed: %w", err)
	}
	if len(res.Entries) == 0 {
		return "", "", "", fmt.Errorf("ldap: user %q not found", username)
	}
	if len(res.Entries) > 1 {
		return "", "", "", fmt.Errorf("ldap: multiple entries matched user %q", username)
	}
	e := res.Entries[0]
	return e.DN, e.GetAttributeValue(p.cfg.UserAttrEmail), e.GetAttributeValue(p.cfg.UserAttrName), nil
}

func (p *Provider) lookupGroups(conn *goldap.Conn, userDN string) ([]string, error) {
	baseDN := p.cfg.GroupBaseDN
	if baseDN == "" {
		baseDN = p.cfg.BaseDN
	}
	var filter string
	if p.cfg.GroupFilter == "" {
		filter = fmt.Sprintf("(%s=%s)", p.cfg.GroupMemberAttr, goldap.EscapeFilter(userDN))
	} else {
		filter = fmt.Sprintf(p.cfg.GroupFilter, goldap.EscapeFilter(userDN))
	}
	req := goldap.NewSearchRequest(
		baseDN, goldap.ScopeWholeSubtree, goldap.NeverDerefAliases,
		0, 10, false, filter, []string{"dn", "cn"}, nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("ldap: group search failed: %w", err)
	}
	groups := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		groups = append(groups, e.DN)
	}
	return groups, nil
}

// ResolveLDAPGroupMappings computes the desired role per organization (last
// matching mapping wins) and the set of LDAP-managed organizations from a user's
// group DNs and the admin-configured mappings. Group DN comparison is
// case-insensitive. Pure and side-effect-free for unit testing; the caller feeds
// the result into the shared membership reconciler (which also deprovisions).
func ResolveLDAPGroupMappings(groupDNs []string, mappings []config.LDAPGroupMapping) (desired map[string]string, managed map[string]struct{}) {
	desired = make(map[string]string)
	managed = make(map[string]struct{})
	groupSet := make(map[string]struct{}, len(groupDNs))
	for _, g := range groupDNs {
		groupSet[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
	}
	for _, m := range mappings {
		managed[m.Organization] = struct{}{}
		if _, ok := groupSet[strings.ToLower(strings.TrimSpace(m.GroupDN))]; ok {
			desired[m.Organization] = m.Role
		}
	}
	return desired, managed
}
