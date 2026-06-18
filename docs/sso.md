<!-- markdownlint-disable MD013 -->
# Single Sign-On & Identity Providers

This guide covers configuring authentication for the Terraform State Manager
(TSM) backend. TSM supports **five enterprise identity integrations** in addition
to API keys:

| Provider | Style | Use |
| --- | --- | --- |
| **OIDC** | OpenID Connect (OAuth 2.0) | Interactive browser SSO (Entra ID, Okta, Keycloak, Google, …) |
| **SAML 2.0** | SP-initiated assertion flow | Interactive browser SSO for SAML-only IdPs |
| **LDAP / AD** | Search-bind | Username/password against a directory |
| **mTLS** | Client certificate | Machine-to-machine authentication |
| **SCIM 2.0** | RFC 7644 provisioning | IdP-driven user/group lifecycle |

All session-based providers (OIDC, SAML, LDAP) establish the **same kind of
session**: an HMAC-signed JWT delivered as an **HttpOnly, CSRF-protected
cookie** — the token is never exposed to page JavaScript. mTLS and SCIM are
machine-facing (Bearer-token / client-cert) and take no cookie.

## Table of Contents

1. [Session model & common config](#session-model--common-config)
2. [Group-to-role mapping](#group-to-role-mapping)
3. [OIDC](#oidc)
4. [SAML 2.0](#saml-20)
5. [LDAP / Active Directory](#ldap--active-directory)
6. [mTLS (client certificates)](#mtls-client-certificates)
7. [SCIM 2.0 provisioning](#scim-20-provisioning)
8. [Testing & troubleshooting](#testing--troubleshooting)
9. [Security best practices](#security-best-practices)

---

## Session model & common config

Configuration is layered: built-in defaults → optional YAML file (`CONFIG_PATH`)
→ `TSM_`-prefixed environment variables (which always win). Auth keys live under
`auth.*`, mapping to env keys like `TSM_AUTH_OIDC_ENABLED`. Secret values must
come from a secret store, never config files or Helm values.

### The JWT signing secret

All session JWTs are signed with `TSM_JWT_SECRET` (HMAC):

| Variable | Required | Notes |
| --- | --- | --- |
| `TSM_JWT_SECRET` | **yes (prod)** | Min 32 chars; cryptographically random. Generate with `openssl rand -hex 32`. Boot fails without it unless `DEV_MODE=true`. |

- It must be **consistent across all replicas** (a different secret per replica
  invalidates sessions issued by another).
- Rotating it invalidates every existing session — users re-authenticate.
- In `DEV_MODE=true`, an **ephemeral** secret is generated and sessions reset on
  restart. Never use dev mode in production.

Sessions (and their cookies) live for **24 hours**; the SPA refreshes silently
before expiry. The cookie's `Secure` attribute is derived from
`TSM_SERVER_PUBLIC_URL`: HTTPS yields `Secure`, and only `http://localhost` dev
yields a non-Secure cookie so it is still sent.

### The login surface

The SPA discovers configured providers from:

```http
GET /api/v1/auth/providers
```

which returns the enabled provider list (OIDC, each named SAML IdP, LDAP) plus a
`dev_mode` flag. The browser flows are:

| Flow | Endpoint |
| --- | --- |
| OIDC login (authorization-code) | `GET /api/v1/auth/login` |
| SAML login (SP-initiated) | `GET /api/v1/auth/login?provider=saml` (or `saml:<idp-name>`) |
| OIDC callback | `GET /api/v1/auth/callback` |
| SAML ACS | `POST /api/v1/auth/saml/acs` |
| SAML SP metadata | `GET /api/v1/auth/saml/metadata` |
| LDAP login | `POST /api/v1/auth/ldap/login` |
| Current user | `GET /api/v1/auth/me` |
| Refresh | `POST /api/v1/auth/refresh` |
| Logout | `GET /api/v1/auth/logout` |

> **There is no local-password login.** Identity always comes from a configured
> provider. The only built-in credential is the bootstrap admin created by the
> [setup wizard](initial-setup.md), and machine API keys (`tsm_…`).

---

## Group-to-role mapping

OIDC, SAML, and LDAP all share one model: a group claim/attribute/membership from
the IdP maps to **organization membership + a role template**. The role
templates TSM owns are `admin`, `editor`, `operator`, and `viewer`.

Three config keys per provider control mapping:

- **group claim/attribute** — the source of the user's groups
  (`group_claim_name` for OIDC, `group_attribute_name` for SAML, the group
  search for LDAP).
- **`group_mappings`** — a list of `{group, organization, role}` entries.
- **`default_role`** — the role assigned (in the `default` organization) when no
  mapping matches. Empty means no role template is granted automatically.

Mapping is **IdP-authoritative and reconciled on every login**: a user removed
from a mapped IdP group is **deprovisioned** from the corresponding organization
on their next login. This means the IdP is the single source of truth — do not
hand-edit memberships that a mapping owns.

> For OIDC specifically, group mappings can also be edited at runtime in the UI
> (**Administration → OIDC groups**) as an overlay; SAML/LDAP mappings are
> config-driven.

---

## OIDC

TSM uses generic OIDC discovery (the shared identity module wraps
`coreos/go-oidc`), so **any OIDC-compliant provider works**: Microsoft Entra ID
(Azure AD), Okta, Keycloak, Auth0, Google Workspace, PingIdentity, and others.
TSM requires `sub` and `email` claims (plus `name`, optional).

### Configuration keys

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_AUTH_OIDC_ENABLED` | `false` | Enable OIDC |
| `TSM_AUTH_OIDC_ISSUER_URL` | | Issuer base, e.g. `https://login.microsoftonline.com/<tenant>/v2.0` |
| `TSM_AUTH_OIDC_CLIENT_ID` | | OAuth client ID |
| `TSM_AUTH_OIDC_CLIENT_SECRET` | | **secret** |
| `TSM_AUTH_OIDC_REDIRECT_URL` | | `https://<host>/api/v1/auth/callback` |
| `TSM_AUTH_OIDC_SCOPES` | `openid,email,profile` | Add `groups` for group-based RBAC |
| `TSM_AUTH_OIDC_REQUIRE_VERIFIED_EMAIL` | `true` | Require a verified email claim |
| `TSM_AUTH_OIDC_GROUP_CLAIM_NAME` | `groups` | ID-token claim carrying group memberships |
| `TSM_AUTH_OIDC_DEFAULT_ROLE` | (empty) | Role on first login when no mapping matches |

> **Redirect URL must match exactly** what you register in the IdP — protocol,
> host, port, and the exact path `/api/v1/auth/callback`, no trailing slash.

### Two ways to configure OIDC

1. **Setup wizard (recommended for new deployments).** The first-run wizard at
   `/setup` configures OIDC and stores it **encrypted in the database**. A
   DB-stored config is loaded at boot and **takes precedence** over file/env
   values, and can be activated at runtime with no restart.
2. **Config file / environment variables.** The keys above. This is fully
   supported and is the right choice for GitOps/declarative deployments.

### YAML example

```yaml
server:
  base_url: "https://tsm.example.com"
  public_url: "https://tsm.example.com"

auth:
  oidc:
    enabled: true
    issuer_url: "https://login.microsoftonline.com/<tenant-id>/v2.0"
    client_id: "your_client_id"
    client_secret: "${TSM_AUTH_OIDC_CLIENT_SECRET}"
    redirect_url: "https://tsm.example.com/api/v1/auth/callback"
    scopes: ["openid", "email", "profile", "groups"]
    require_verified_email: true
    group_claim_name: "groups"
    default_role: "viewer"
    group_mappings:
      - group: "tsm-admins"
        organization: "default"
        role: "admin"
      - group: "platform-team"
        organization: "default"
        role: "editor"
```

### Provider notes

- **Entra ID / Azure AD.** Register a Web app with redirect
  `https://tsm.example.com/api/v1/auth/callback`; issuer is
  `https://login.microsoftonline.com/<tenant-id>/v2.0` (or `…/common/v2.0` for
  multi-tenant). For groups, add the optional `groups` claim in **Token
  configuration**.
- **Okta.** Issuer `https://<org>.okta.com` (or a custom authorization server);
  add a `groups` claim on the authorization server to use group RBAC.
- **Keycloak.** Issuer `https://keycloak.example.com/auth/realms/<realm>`;
  confidential client; copy the client secret from **Credentials**.
- **Google Workspace.** Issuer `https://accounts.google.com`; restrict to your
  Workspace domain on the OAuth consent screen.

> **Local development note.** TSM's OIDC provider runs with `RequireHTTPS`
> disabled so a local `http://` Keycloak issuer works. In production use an
> `https://` issuer.

---

## SAML 2.0

TSM is a SAML **Service Provider (SP)**. For each configured IdP it validates
XML-signed assertions and maps a group attribute to org/role memberships (the
same reconciling model as OIDC/LDAP). The ACS endpoint is
`POST /api/v1/auth/saml/acs`; SP metadata is at `GET /api/v1/auth/saml/metadata`.

### Configuration keys

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_AUTH_SAML_ENABLED` | `false` | Enable SAML |
| `TSM_AUTH_SAML_ENTITY_ID` | (derived) | SP entity ID; defaults to the ACS URL minus `/saml/acs` |
| `TSM_AUTH_SAML_ACS_URL` | | Public ACS URL, e.g. `https://<host>/api/v1/auth/saml/acs` |
| `TSM_AUTH_SAML_CERT_FILE` | (empty) | Optional SP signing cert (PEM, file path) |
| `TSM_AUTH_SAML_KEY_FILE` | (empty) | Optional SP signing key (PEM, file path) |
| `TSM_AUTH_SAML_ALLOW_IDP_INITIATED` | `false` | Keep `false` — see below |
| `TSM_AUTH_SAML_GROUP_ATTRIBUTE_NAME` | (empty) | Assertion attribute carrying the user's groups |
| `TSM_AUTH_SAML_DEFAULT_ROLE` | (empty) | Role on first login when no mapping matches |

IdPs are a structured list (so configure them in `config.yaml`); provide either a
metadata URL or inline XML per IdP:

```yaml
auth:
  saml:
    enabled: true
    acs_url: "https://tsm.example.com/api/v1/auth/saml/acs"
    allow_idp_initiated: false
    group_attribute_name: "groups"
    default_role: "viewer"
    idps:
      - name: "corp-okta"
        metadata_url: "https://corp.okta.com/app/abc/sso/saml/metadata"
    group_mappings:
      - group: "tsm-admins"
        organization: "default"
        role: "admin"
```

### Security posture

Trust is delegated to `crewjam/saml`, which requires the response or each
assertion to be XML-signed (unsigned ⇒ reject), runs a roundtrip validator that
defeats signature-wrapping (XSW), and validates `NotBefore`/`NotOnOrAfter`,
audience (== our EntityID), the SubjectConfirmation Recipient, and the response
Destination (== our ACS URL). IdP metadata is fetched HTTPS-only and bounded.

> **Keep `allow_idp_initiated: false`** (the default and a hardening over the
> registry's port). With it off, only SP-initiated flows are accepted and the
> AuthnRequest ID is bound to the response (`InResponseTo`), defeating
> stolen-assertion replay and login CSRF. Enable IdP-initiated SSO only if a
> specific IdP integration requires it and you understand the replay surface.

---

## LDAP / Active Directory

LDAP uses **search-bind**: bind a service account, search for the user, bind as
the user to verify the password, then resolve group memberships for role
mapping. The login endpoint is `POST /api/v1/auth/ldap/login`.

### Configuration keys

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_AUTH_LDAP_ENABLED` | `false` | Enable LDAP |
| `TSM_AUTH_LDAP_HOST` | | Directory host |
| `TSM_AUTH_LDAP_PORT` | `0` | `0` auto-selects 636 (LDAPS) or 389 (StartTLS/plain) |
| `TSM_AUTH_LDAP_USE_TLS` | `false` | LDAPS (TLS from connect) |
| `TSM_AUTH_LDAP_START_TLS` | `false` | Upgrade plain LDAP to TLS |
| `TSM_AUTH_LDAP_INSECURE_SKIP_VERIFY` | `false` | Dev only — never in production |
| `TSM_AUTH_LDAP_BASE_DN` | | User search base |
| `TSM_AUTH_LDAP_BIND_DN` | | Service-account DN for search |
| `TSM_AUTH_LDAP_BIND_PASSWORD` | | **secret** |
| `TSM_AUTH_LDAP_USER_FILTER` | | Must contain `%s` for the escaped username, e.g. `(sAMAccountName=%s)` |
| `TSM_AUTH_LDAP_USER_ATTR_EMAIL` | `mail` | Email attribute |
| `TSM_AUTH_LDAP_USER_ATTR_NAME` | `displayName` | Display-name attribute |
| `TSM_AUTH_LDAP_GROUP_BASE_DN` | (empty) | Set to enable group resolution |
| `TSM_AUTH_LDAP_GROUP_FILTER` | (empty) | Optional; `%s` = escaped user DN |
| `TSM_AUTH_LDAP_GROUP_MEMBER_ATTR` | `member` | Membership attribute |
| `TSM_AUTH_LDAP_DEFAULT_ROLE` | (empty) | Role on first login when no group mapping matches |

```yaml
auth:
  ldap:
    enabled: true
    host: "ad.example.com"
    use_tls: true
    base_dn: "OU=Users,DC=example,DC=com"
    bind_dn: "CN=svc-tsm,OU=Service,DC=example,DC=com"
    bind_password: "${TSM_AUTH_LDAP_BIND_PASSWORD}"
    user_filter: "(sAMAccountName=%s)"
    group_base_dn: "OU=Groups,DC=example,DC=com"
    default_role: "viewer"
    group_mappings:
      - group_dn: "CN=tsm-admins,OU=Groups,DC=example,DC=com"
        organization: "default"
        role: "admin"
```

### Security posture

All user-influenced values (username, user DN) are escaped before being placed
into an LDAP filter, defeating LDAP injection; the user DN is found by **search**,
never constructed from input. Empty usernames/passwords are rejected up front to
defeat the LDAP "unauthenticated bind" weakness. Always use `use_tls` or
`start_tls` in production and never enable `insecure_skip_verify` there.

---

## mTLS (client certificates)

mTLS is **additive, machine-to-machine** authentication: a client presenting a
certificate that the **TLS layer has verified** against the configured client CA
is authenticated and granted the scopes mapped to its subject. It runs before JWT
auth and is a no-op when not enabled or when the TLS layer did not verify a client
cert.

### Configuration keys

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_AUTH_MTLS_ENABLED` | `false` | Enable mTLS |
| `TSM_AUTH_MTLS_CLIENT_CA_FILE` | | PEM bundle of trusted client CAs (required when enabled) |

mTLS requires the backend to **terminate TLS itself** (set `TSM_SERVER_TLS_CERT_FILE`
/ `TSM_SERVER_TLS_KEY_FILE`); it does not work behind a proxy that terminates TLS.
Subject→scope mappings are a structured list:

```yaml
server:
  tls_cert_file: /etc/tsm/tls/server.crt
  tls_key_file: /etc/tsm/tls/server.key

auth:
  mtls:
    enabled: true
    client_ca_file: /etc/tsm/tls/client-ca.pem
    mappings:
      - subject: "CN=ci-runner"
        scopes: ["state:read", "state:drift"]
      - subject: "dns:bot.example.com"
        scopes: ["state:read"]
```

### Matching rules

For a verified certificate, TSM tries, in order: the CN (`CN=<cn>`), each DNS SAN
(`dns:<san>` — a stronger identity than CN), then the full Distinguished Name.
Subject matching is case-insensitive. This package never decides whether a
certificate is trusted — that is the TLS handshake's job
(`tls.ConnectionState.VerifiedChains`); a merely-presented certificate can never
authenticate. Mappings are admin-configured, never user-supplied.

> Grant the **minimum scopes** each certificate needs. A CI runner that only
> ingests drift needs `state:drift` (and `state:read`), not `admin`.

---

## SCIM 2.0 provisioning

SCIM 2.0 (RFC 7644) lets an external IdP drive **user and group lifecycle** —
provisioning, updating, and deactivating — against TSM's shared identity store.

### Configuration keys

| Variable | Default | Description |
| --- | --- | --- |
| `TSM_AUTH_SCIM_ENABLED` | `false` | Mounts `/scim/v2` (RFC 7644) |

When **disabled (the default), the routes are not mounted at all** — the surface
does not exist unless an operator opts in. When enabled, every endpoint is
**Bearer-token authenticated and gated by the `scim:provision` scope** (admin
satisfies it). The endpoints take no cookies, so they are not CSRF-eligible. IdP
clients present `Authorization: Bearer <token>` (a `tsm_…` API key with the
`scim:provision` scope).

Endpoints mounted under `/scim/v2`: `Users` (list/get/create/put/patch/delete)
and `Groups` (list/get). Deletes/deactivations are **soft** (membership removal),
never hard-deletes.

> **Rate-limit SCIM at the proxy.** As with the other token endpoints, request
> rate-limiting belongs at the gateway/proxy in front of the API.

---

## Testing & troubleshooting

### Verify an OIDC issuer

```bash
curl -s https://your-issuer-url/.well-known/openid-configuration | jq .
# Should return authorization_endpoint, token_endpoint, jwks_uri, …
```

### Verify a session end-to-end

```bash
# After completing a browser login, call /me with the session cookie:
curl -s https://tsm.example.com/api/v1/auth/me -b cookies.txt
```

### Common issues

| Symptom | Likely cause |
| --- | --- |
| OIDC: `redirect_uri_mismatch` | `TSM_AUTH_OIDC_REDIRECT_URL` does not match the IdP registration exactly (scheme/host/port/path) |
| OIDC: "failed to create OIDC provider" | Issuer URL wrong/unreachable, or trailing slash — test the `.well-known` URL |
| OIDC: missing `email` | Add `email`/`profile` scopes; confirm the account has an email; check `require_verified_email` |
| Sessions drop after restart | `DEV_MODE` ephemeral secret, or `TSM_JWT_SECRET` differs across replicas |
| SAML: assertion rejected | Unsigned response/assertion, audience/Destination mismatch, or clock skew beyond tolerance |
| LDAP: binds but no groups | `group_base_dn` unset, or wrong `group_member_attr`/`group_filter` |
| mTLS: cert ignored | TLS terminated by a proxy (backend must terminate TLS), or no subject mapping matches |
| SCIM: `404` on `/scim/v2/*` | `TSM_AUTH_SCIM_ENABLED` not set — routes are unmounted by default |

### Debug logging

```bash
export TSM_LOGGING_LEVEL=debug
export TSM_LOGGING_FORMAT=json
```

Auth provider initialization, token exchange, and claim verification log under
the auth components. Note that `warn` suppresses boot/info lines.

---

## Security best practices

1. **JWT secret.** ≥32 random chars, identical across replicas, stored in a
   secret manager, rotated per policy (rotation forces re-login).
2. **HTTPS only in production.** It drives the `Secure` cookie flag and protects
   bearer tokens and assertions in transit.
3. **Least-privilege scopes.** Map IdP groups to the narrowest role
   (`viewer`/`operator` before `editor`/`admin`); give mTLS certs and API keys
   only the scopes they need.
4. **Keep SAML `allow_idp_initiated: false`** unless a specific IdP requires
   otherwise.
5. **Lock down LDAP transport.** Always `use_tls`/`start_tls`; never
   `insecure_skip_verify` in production.
6. **Treat group mappings as IdP-authoritative.** They reconcile (and revoke) on
   every login — manage access in the IdP, not by hand.
7. **Rate-limit token endpoints** (LDAP login, SCIM) at the proxy.
8. **Audit.** Logins, logouts, and API-key lifecycle are recorded in the audit
   trail (**Administration → Audit logs**); review them.
