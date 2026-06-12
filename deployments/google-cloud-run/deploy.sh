#!/usr/bin/env bash
# Deploy Terraform State Manager to Cloud Run.
# Prereqs: gcloud authed; Cloud SQL PostgreSQL; Secret Manager secrets
# tsm-jwt-secret, tsm-encryption-key, tsm-database-password; images in
# Artifact Registry. See docs/deployment/google-cloud-run.md.
set -euo pipefail

PROJECT_ID="${PROJECT_ID:?}"
REGION="${REGION:?}"
DIR="$(dirname "$0")"

render() { sed -e "s/REGION/${REGION}/g" -e "s/PROJECT_ID/${PROJECT_ID}/g" "$1"; }

echo "── Deploying backend…"
render "$DIR/backend-service.yaml" | gcloud run services replace - --region="$REGION"

echo "── Deploying worker…"
render "$DIR/worker-service.yaml" | gcloud run services replace - --region="$REGION"

BACKEND_URL=$(gcloud run services describe tsm-backend --region="$REGION" --format='value(status.url)')
BACKEND_HOST="${BACKEND_URL#https://}"
echo "Backend: $BACKEND_URL"

echo "── Generating frontend nginx config (upstream: $BACKEND_HOST)…"
NGINX_CONF=$(cat <<CONF
server {
    listen 80;
    server_name _;
    root /usr/share/nginx/html;
    index index.html;
    client_max_body_size 100m;
    location / { try_files \$uri \$uri/ /index.html; }
    location /api/ {
        proxy_pass ${BACKEND_URL};
        proxy_ssl_server_name on;
        proxy_set_header Host ${BACKEND_HOST};
        proxy_set_header X-Forwarded-Proto https;
    }
    location /scim/ {
        proxy_pass ${BACKEND_URL};
        proxy_ssl_server_name on;
        proxy_set_header Host ${BACKEND_HOST};
    }
    location = /health { proxy_pass ${BACKEND_URL}; proxy_ssl_server_name on; proxy_set_header Host ${BACKEND_HOST}; }
    location = /ready  { proxy_pass ${BACKEND_URL}; proxy_ssl_server_name on; proxy_set_header Host ${BACKEND_HOST}; }
    location ~ ^/swagger { proxy_pass ${BACKEND_URL}; proxy_ssl_server_name on; proxy_set_header Host ${BACKEND_HOST}; }
}
CONF
)
if gcloud secrets describe tsm-frontend-nginx --project="$PROJECT_ID" >/dev/null 2>&1; then
  printf '%s' "$NGINX_CONF" | gcloud secrets versions add tsm-frontend-nginx --data-file=- --project="$PROJECT_ID"
else
  printf '%s' "$NGINX_CONF" | gcloud secrets create tsm-frontend-nginx --data-file=- --project="$PROJECT_ID"
fi

echo "── Deploying frontend…"
render "$DIR/frontend-service.yaml" | gcloud run services replace - --region="$REGION"

echo
echo "Frontend URL:"
gcloud run services describe tsm-frontend --region="$REGION" --format='value(status.url)'
echo
echo "NOTE: point TSM_SERVER_*_URL placeholders at your final public hostname"
echo "(custom domain or the frontend URL above) and re-run if they changed."
