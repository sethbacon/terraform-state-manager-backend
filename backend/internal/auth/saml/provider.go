// Package saml implements SAML 2.0 Service Provider (SP) authentication. For each
// configured IdP it builds a crewjam/saml ServiceProvider that validates signed
// SAML responses and extracts the user's identity + group attributes; those groups
// are then mapped to org/role memberships (same model as OIDC/LDAP).
//
// SECURITY: response/assertion trust is delegated to crewjam/saml, which —
//   - requires the Response or each Assertion to be XML-signed and verifies the
//     signature against the IdP metadata certificate (goxmldsig); unsigned ⇒ reject;
//   - runs mattermost/xml-roundtrip-validator on parse, rejecting documents whose
//     canonical re-serialization differs — the standard defense against SAML
//     signature-wrapping (XSW);
//   - validates Conditions/SubjectConfirmation NotBefore & NotOnOrAfter (with a
//     bounded clock skew), the Audience (== our EntityID), the SubjectConfirmation
//     Recipient and the Response Destination (== our ACS URL) — assertion replay,
//     audience-confusion and endpoint-confusion are rejected.
//
// IdP metadata is fetched HTTPS-only and bounded to 1 MiB; beevik/etree does not
// resolve external entities or DTDs, so XML external-entity (XXE) injection does
// not apply.
//
// Hardening over the registry's port: IdP-initiated SSO is OFF by default
// (config auth.saml.allow_idp_initiated). With it off only SP-initiated logins are
// accepted and the AuthnRequest ID is bound to the response (InResponseTo via
// MakeAuthenticationRequest + ValidateResponse), defeating unsolicited-response
// replay and login CSRF. Group mappings feed the shared, DEPROVISIONING membership
// reconciler (see api.reconcileManagedMemberships) so losing an IdP group revokes
// access. The unused, confusing ParseResponse helper from the registry is dropped.
package saml

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"

	"github.com/terraform-state-manager/terraform-state-manager/internal/config"
)

// Provider wraps a crewjam/saml ServiceProvider for a single configured IdP.
type Provider struct {
	sp   saml.ServiceProvider
	name string
}

// UserInfo holds the attributes extracted from a validated SAML assertion.
type UserInfo struct {
	NameID string
	Email  string
	Name   string
	Groups []string
}

// NewProvider creates a SAML Service Provider for the given IdP. It loads the
// optional SP signing cert/key and resolves IdP metadata (HTTPS URL or inline XML).
func NewProvider(cfg config.SAMLConfig, idp config.SAMLIdPConfig) (*Provider, error) {
	if cfg.ACSURL == "" {
		return nil, fmt.Errorf("saml: acs_url is required")
	}
	acsURL, err := url.Parse(cfg.ACSURL)
	if err != nil {
		return nil, fmt.Errorf("saml: invalid acs_url: %w", err)
	}

	entityID := cfg.EntityID
	if entityID == "" {
		entityID = strings.TrimSuffix(cfg.ACSURL, "/saml/acs")
	}

	sp := saml.ServiceProvider{
		EntityID: entityID,
		AcsURL:   *acsURL,
		// Default false: only SP-initiated flows (with InResponseTo binding) are
		// accepted unless an operator explicitly opts into IdP-initiated SSO.
		AllowIDPInitiated: cfg.AllowIDPInitiated,
	}

	// Load the SP signing cert/key when provided (used to sign AuthnRequests).
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		keyPair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("saml: failed to load SP cert/key: %w", err)
		}
		rsaKey, ok := keyPair.PrivateKey.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("saml: SP key is not an RSA private key")
		}
		sp.Key = rsaKey
		sp.Certificate = keyPair.Leaf
		if sp.Certificate == nil {
			// tls.LoadX509KeyPair does not always populate Leaf; parse manually.
			cert, err := x509.ParseCertificate(keyPair.Certificate[0])
			if err != nil {
				return nil, fmt.Errorf("saml: failed to parse SP certificate: %w", err)
			}
			sp.Certificate = cert
		}
	}

	idpMetadata, err := resolveIdPMetadata(idp)
	if err != nil {
		return nil, fmt.Errorf("saml: IdP %q: %w", idp.Name, err)
	}
	sp.IDPMetadata = idpMetadata

	return &Provider{sp: sp, name: idp.Name}, nil
}

// Name returns the display name of the IdP this provider serves.
func (p *Provider) Name() string { return p.name }

// GetMetadata returns the SP metadata descriptor for publishing to the IdP.
func (p *Provider) GetMetadata() *saml.EntityDescriptor { return p.sp.Metadata() }

// MakeAuthenticationRequest builds a SAML AuthnRequest redirect URL for the
// SP-initiated flow. It returns the redirect URL and the AuthnRequest ID; the
// caller persists the ID in session state and passes it back to ValidateResponse
// as a possible request ID so crewjam validates InResponseTo (binding the IdP's
// response to the request we issued).
func (p *Provider) MakeAuthenticationRequest(relayState string) (*url.URL, string, error) {
	authReq, err := p.sp.MakeAuthenticationRequest(
		p.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding),
		saml.HTTPRedirectBinding,
		saml.HTTPPostBinding,
	)
	if err != nil {
		return nil, "", fmt.Errorf("saml: failed to create AuthnRequest: %w", err)
	}
	redirectURL, err := authReq.Redirect(relayState, &p.sp)
	if err != nil {
		return nil, "", fmt.Errorf("saml: failed to build redirect URL: %w", err)
	}
	return redirectURL, authReq.ID, nil
}

// ValidateResponse validates a SAML response carried by the ACS request and
// returns the user info. possibleRequestIDs binds the response to a prior
// AuthnRequest (InResponseTo) for SP-initiated logins; pass nil for the
// IdP-initiated flow (only honored when AllowIDPInitiated is set).
func (p *Provider) ValidateResponse(r *http.Request, possibleRequestIDs []string, groupAttr string) (*UserInfo, error) {
	assertion, err := p.sp.ParseResponse(r, possibleRequestIDs)
	if err != nil {
		return nil, fmt.Errorf("saml: failed to validate response: %w", err)
	}
	return extractUserInfo(assertion, groupAttr), nil
}

// extractUserInfo pulls user attributes from a validated SAML assertion.
func extractUserInfo(assertion *saml.Assertion, groupAttr string) *UserInfo {
	info := &UserInfo{}

	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		info.NameID = assertion.Subject.NameID.Value
	}

	for _, stmt := range assertion.AttributeStatements {
		for _, attr := range stmt.Attributes {
			values := attrValues(attr)
			switch {
			case isEmailAttr(attr.Name, attr.FriendlyName):
				if len(values) > 0 {
					info.Email = values[0]
				}
			case isNameAttr(attr.Name, attr.FriendlyName):
				if len(values) > 0 {
					info.Name = values[0]
				}
			case groupAttr != "" && (attr.Name == groupAttr || attr.FriendlyName == groupAttr):
				info.Groups = values
			}
		}
	}

	// Fall back to NameID as the email when no explicit email attribute is present.
	if info.Email == "" && strings.Contains(info.NameID, "@") {
		info.Email = info.NameID
	}

	return info
}

// ResolveSAMLGroupMappings computes the desired role per organization (last
// matching mapping wins) and the set of SAML-managed organizations from a user's
// SAML group-attribute values and the admin-configured mappings. Group values are
// matched exactly. Pure and side-effect-free for unit testing; the caller feeds
// the result into the shared membership reconciler (which also deprovisions).
func ResolveSAMLGroupMappings(groups []string, mappings []config.SAMLGroupMapping) (desired map[string]string, managed map[string]struct{}) {
	desired = make(map[string]string)
	managed = make(map[string]struct{})
	groupSet := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		groupSet[g] = struct{}{}
	}
	for _, m := range mappings {
		managed[m.Organization] = struct{}{}
		if _, ok := groupSet[m.Group]; ok {
			desired[m.Organization] = m.Role
		}
	}
	return desired, managed
}

// resolveIdPMetadata loads IdP metadata from inline XML or a metadata URL.
func resolveIdPMetadata(idp config.SAMLIdPConfig) (*saml.EntityDescriptor, error) {
	if idp.MetadataXML != "" {
		metadata := &saml.EntityDescriptor{}
		if err := xml.Unmarshal([]byte(idp.MetadataXML), metadata); err != nil {
			return nil, fmt.Errorf("failed to parse metadata XML: %w", err)
		}
		return metadata, nil
	}
	if idp.MetadataURL != "" {
		return fetchIdPMetadata(idp.MetadataURL)
	}
	return nil, fmt.Errorf("either metadata_url or metadata_xml must be provided")
}

// fetchIdPMetadata retrieves and parses IdP metadata from an HTTPS URL. The read
// is bounded to 1 MiB to prevent resource exhaustion.
func fetchIdPMetadata(metadataURL string) (*saml.EntityDescriptor, error) {
	parsedURL, err := url.Parse(metadataURL)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata URL: %w", err)
	}
	if parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("metadata URL must use HTTPS: %s", metadataURL)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	// #nosec G107 -- metadata_url is admin-configured (never user-supplied),
	// enforced HTTPS, and the response body is read-bounded to 1 MiB below.
	resp, err := client.Get(metadataURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metadata from %s: %w", metadataURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata URL returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata response: %w", err)
	}

	metadata, err := samlsp.ParseMetadata(data)
	if err != nil {
		return nil, fmt.Errorf("failed to parse IdP metadata: %w", err)
	}

	slog.Info("fetched SAML IdP metadata", "url", metadataURL, "entity_id", metadata.EntityID)
	return metadata, nil
}

// isEmailAttr reports whether a SAML attribute name/friendly-name is an email.
func isEmailAttr(name, friendlyName string) bool {
	switch {
	case friendlyName == "email" || friendlyName == "mail":
		return true
	case name == "urn:oid:0.9.2342.19200300.100.1.3": // mail
		return true
	case name == "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress":
		return true
	case strings.EqualFold(name, "email") || strings.EqualFold(name, "mail"):
		return true
	}
	return false
}

// isNameAttr reports whether a SAML attribute name/friendly-name is a display name.
func isNameAttr(name, friendlyName string) bool {
	switch {
	case friendlyName == "displayName" || friendlyName == "cn":
		return true
	case name == "urn:oid:2.16.840.1.113730.3.1.241": // displayName
		return true
	case name == "urn:oid:2.5.4.3": // cn
		return true
	case name == "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name":
		return true
	case strings.EqualFold(name, "displayName") || strings.EqualFold(name, "cn"):
		return true
	}
	return false
}

// attrValues extracts the non-empty string values of a SAML attribute.
func attrValues(attr saml.Attribute) []string {
	vals := make([]string, 0, len(attr.Values))
	for _, v := range attr.Values {
		if v.Value != "" {
			vals = append(vals, v.Value)
		}
	}
	return vals
}
