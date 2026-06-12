#!/usr/bin/env bash
# Installs the Terraform State Manager backend as a systemd service plus the
# frontend dist behind nginx. Run as root on the target host.
# Prereqs: PostgreSQL 14+ reachable; nginx; git (required by the git state
# connector); the backend binary and frontend dist built:
#   backend:  cd backend && go build -o terraform-state-manager ./cmd/server
#   frontend: cd frontend && npm ci && npm run build   (produces dist/)
set -euo pipefail

BINARY="${1:?usage: install.sh <path-to-backend-binary> <path-to-frontend-dist>}"
DIST="${2:?usage: install.sh <path-to-backend-binary> <path-to-frontend-dist>}"
DIR="$(cd "$(dirname "$0")" && pwd)"

id -u tsm >/dev/null 2>&1 || useradd --system --home /var/lib/terraform-state-manager --shell /usr/sbin/nologin tsm

install -d -o tsm -g tsm /var/lib/terraform-state-manager
install -d /opt/terraform-state-manager
install -m 0755 "$BINARY" /opt/terraform-state-manager/terraform-state-manager
rm -rf /opt/terraform-state-manager/frontend
cp -r "$DIST" /opt/terraform-state-manager/frontend

install -d /etc/terraform-state-manager
if [ ! -f /etc/terraform-state-manager/environment ]; then
  install -m 0640 -g tsm "$DIR/environment.example" /etc/terraform-state-manager/environment
  echo ">> EDIT /etc/terraform-state-manager/environment (secrets are placeholders!)"
fi

install -m 0644 "$DIR/terraform-state-manager.service" /etc/systemd/system/terraform-state-manager.service
install -m 0644 "$DIR/nginx-tsm.conf" /etc/nginx/conf.d/tsm.conf
echo ">> EDIT /etc/nginx/conf.d/tsm.conf (server_name + TLS paths)"

command -v git >/dev/null || echo "WARNING: git not found — the git state connector needs it (apt install git)"

systemctl daemon-reload
systemctl enable terraform-state-manager
echo
echo "Next: edit the environment file, then:"
echo "  systemctl start terraform-state-manager && systemctl reload nginx"
echo "  curl -s http://127.0.0.1:8080/health"
