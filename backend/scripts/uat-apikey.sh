#!/usr/bin/env bash
# Auth UAT: headless OIDC login (exercises the shared OIDC provider + JWT
# TokenManager) → create an API key → authenticate with it (API key helpers).
# Covers the identity-module auth chain end-to-end.
# Requires the Keycloak UAT stack up (frontend:3001, backend:8081, keycloak:8180).
set -uo pipefail
export LC_ALL=C

R="--resolve keycloak:8180:127.0.0.1"
BACKEND="http://localhost:8081"
FRONTEND="http://localhost:3001"
CJ="$(mktemp)"

# The organization id is resolved after login via the authenticated API (step
# 5b) rather than the DB, so the UAT is schema-agnostic: it always uses the org
# the backend sees, whether identity data lives in the public or identity schema.

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

echo "== 1. /auth/login → Keycloak authorize =="
AUTHZ=$(curl -s $R -c "$CJ" -o /dev/null -w '%{redirect_url}' "$BACKEND/api/v1/auth/login")
[[ "$AUTHZ" == *"keycloak:8180"* ]] || fail "login did not redirect to keycloak (got: $AUTHZ)"
pass "authorize URL: ${AUTHZ:0:70}..."

echo "== 2. fetch login form =="
PAGE=$(curl -s $R -c "$CJ" -b "$CJ" "$AUTHZ")
FORM=$(printf '%s' "$PAGE" | grep -o 'action="[^"]*"' | head -1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
[[ -n "$FORM" ]] || fail "could not find login form action"
pass "form action found"

echo "== 3. POST credentials =="
CB=$(curl -s $R -c "$CJ" -b "$CJ" -o /dev/null -w '%{redirect_url}' \
  --data-urlencode "username=admin.user" \
  --data-urlencode "password=TestPass123!" \
  --data-urlencode "credentialId=" \
  "$FORM")
[[ "$CB" == *"/api/v1/auth/callback?"* ]] || fail "no callback redirect after login (got: $CB)"
pass "callback: ${CB:0:70}..."

echo "== 4. follow callback → backend issues JWT =="
FINAL=$(curl -s -c "$CJ" -b "$CJ" -o /dev/null -w '%{redirect_url}' "$CB")
JWT=$(printf '%s' "$FINAL" | sed -n 's/.*[?&]token=\([^&]*\).*/\1/p')
# URL-decode %XX just in case (token is base64url, usually clean)
[[ -n "$JWT" ]] || fail "no token in final redirect (got: $FINAL)"
pass "JWT acquired (${#JWT} chars)"

echo "== 5. inspect JWT issuer =="
PAYLOAD=$(printf '%s' "$JWT" | cut -d. -f2 | tr '_-' '/+'); while [ $((${#PAYLOAD} % 4)) -ne 0 ]; do PAYLOAD="${PAYLOAD}="; done
ISS=$(printf '%s' "$PAYLOAD" | base64 -d 2>/dev/null | grep -o '"iss":"[^"]*"' | sed 's/.*"iss":"//; s/"$//')
echo "   iss=$ISS"

echo "== 5b. resolve org id via authenticated API =="
ORGS=$(curl -s $R -b "$CJ" "$BACKEND/api/v1/organizations" -H "Authorization: Bearer $JWT")
ORG=$(printf '%s' "$ORGS" | grep -o '"id":"[^"]*"' | head -1 | sed 's/.*"id":"//; s/"$//')
[[ -n "$ORG" ]] || fail "could not resolve an org id from the API (resp: ${ORGS:0:120})"
pass "org id: $ORG"

echo "== 6. create API key (POST /api/v1/apikeys) =="
CREATE=$(curl -s $R -b "$CJ" -X POST "$BACKEND/api/v1/apikeys" \
  -H "Authorization: Bearer $JWT" -H "Content-Type: application/json" \
  -d "{\"name\":\"uat-slice3\",\"organization_id\":\"$ORG\",\"scopes\":[\"sources:read\"]}")
APIKEY=$(printf '%s' "$CREATE" | grep -o '"key":"[^"]*"' | head -1 | sed 's/.*"key":"//; s/"$//')
[[ "$APIKEY" == tsm_* ]] || fail "no tsm_ key returned (resp: $CREATE)"
pass "API key created: ${APIKEY:0:10}..."

echo "== 7. authenticate with the API key =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BACKEND/api/v1/sources" -H "Authorization: Bearer $APIKEY")
[[ "$CODE" == "200" ]] && pass "valid API key → 200" || fail "valid API key → $CODE (expected 200)"

echo "== 8. bogus API key rejected =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BACKEND/api/v1/sources" -H "Authorization: Bearer tsm_bogusbogusbogus")
[[ "$CODE" == "401" ]] && pass "bogus API key → 401" || fail "bogus API key → $CODE (expected 401)"

echo "ALL AUTH UAT CHECKS PASSED"
