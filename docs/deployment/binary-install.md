# Binary install (systemd)

`deployments/binary` — for VMs without containers. One host runs everything
(workers stay in-process: `TSM_WORKERS_ENABLED=true`).

```bash
# Build
(cd backend && go build -o terraform-state-manager ./cmd/server)
(cd ../terraform-state-manager-frontend/frontend && npm ci && npm run build)

# Install (as root)
sudo deployments/binary/install.sh backend/terraform-state-manager \
  ../terraform-state-manager-frontend/frontend/dist

sudo vi /etc/terraform-state-manager/environment   # secrets + URLs
sudo vi /etc/nginx/conf.d/tsm.conf                 # server_name + TLS paths
sudo systemctl start terraform-state-manager && sudo systemctl reload nginx
curl -s http://127.0.0.1:8080/health
```

The environment file is seeded from
[`deployments/binary/environment.example`](../../deployments/binary/environment.example);
the **mandatory** keys are `TSM_DATABASE_HOST`/`_PASSWORD`, `TSM_JWT_SECRET`, and
`TSM_ENCRYPTION_KEY` (plus the `TSM_SERVER_*` URLs for a real hostname) — see
[configuration.md](../configuration.md) for the full set.

The systemd unit runs the binary's `serve` subcommand, which **applies database
migrations on start** (under advisory locks). For a manual pre-step you can run
`terraform-state-manager migrate up` (also `migrate down`); `version` prints the
build.

Requirements: PostgreSQL 14+, nginx, **git** (the git state connector shells
out to it), a TLS certificate (certbot works with the provided nginx conf).
The unit is hardened (`ProtectSystem=strict`, `NoNewPrivileges`,
`PrivateTmp`); the app writes only under `/var/lib/terraform-state-manager`.
Logs: `journalctl -u terraform-state-manager -f`.
