#!/usr/bin/env bash
# Combined identity-surface UAT: headless OIDC login → JWT → verify the
# /auth/me envelope (the shape the ported registry AuthContext reads) →
# exercise the identity admin endpoints the frontend pages call.
# Requires the Keycloak UAT stack up (frontend:3001, backend:8081, keycloak:8180).
set -uo pipefail
export LC_ALL=C

R="--resolve keycloak:8180:127.0.0.1"
BACKEND="http://localhost:8081"
CJ="$(mktemp)"
fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails+1)); }

echo "== 1. /auth/providers advertises OIDC =="
PROV=$(curl -s "$BACKEND/api/v1/auth/providers")
echo "   $PROV"
echo "$PROV" | grep -q '"type":"oidc"' && pass "OIDC advertised" || fail "OIDC not advertised"

echo "== 2. headless OIDC login → JWT =="
AUTHZ=$(curl -s $R -c "$CJ" -o /dev/null -w '%{redirect_url}' "$BACKEND/api/v1/auth/login")
PAGE=$(curl -s $R -c "$CJ" -b "$CJ" "$AUTHZ")
FORM=$(printf '%s' "$PAGE" | grep -o 'action="[^"]*"' | head -1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
CB=$(curl -s $R -c "$CJ" -b "$CJ" -o /dev/null -w '%{redirect_url}' \
  --data-urlencode "username=admin.user" --data-urlencode "password=TestPass123!" \
  --data-urlencode "credentialId=" "$FORM")
FINAL=$(curl -s -c "$CJ" -b "$CJ" -o /dev/null -w '%{redirect_url}' "$CB")
JWT=$(printf '%s' "$FINAL" | sed -n 's/.*[?&]token=\([^&]*\).*/\1/p')
if [[ -n "$JWT" ]]; then pass "JWT acquired (${#JWT} chars)"; else fail "no JWT (final: $FINAL)"; echo "ABORT"; exit 1; fi

echo "== 3. /auth/me envelope (registry shape the AuthContext reads) =="
ME=$(curl -s "$BACKEND/api/v1/auth/me" -H "Authorization: Bearer $JWT")
echo "   $ME"
echo "$ME" | grep -q '"user":{' && pass "user{} is nested" || fail "user not nested"
echo "$ME" | grep -q '"email":"admin@example.com"' && pass "user.email = admin@example.com" || fail "admin email missing"
echo "$ME" | grep -q '"allowed_scopes":' && pass "allowed_scopes present" || fail "allowed_scopes missing"
echo "$ME" | grep -q '"role_template":' && pass "top-level role_template present" || fail "role_template missing"
echo "$ME" | grep -q '"session_expires_at":' && pass "session_expires_at present" || fail "session_expires_at missing"

echo "== 4. identity admin endpoints (admin JWT → 200) =="
for ep in "/users" "/organizations" "/apikeys" "/admin/role-templates" "/admin/oidc/config" "/admin/audit-logs"; do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BACKEND/api/v1$ep" -H "Authorization: Bearer $JWT")
  [[ "$CODE" == "200" ]] && pass "GET $ep → 200" || fail "GET $ep → $CODE"
done

echo "== 5. unauthenticated admin endpoint → 401 =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BACKEND/api/v1/users")
[[ "$CODE" == "401" ]] && pass "no-token /users → 401" || fail "no-token → $CODE"

echo "============================================"
[[ $fails -eq 0 ]] && echo "ALL COMBINED UAT CHECKS PASSED" || echo "UAT FAILURES: $fails"
exit $fails
