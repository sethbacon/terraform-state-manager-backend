# Docker Compose (production)

`deployments/docker-compose.prod.yml` — single-host production: backend API
(workers off) + dedicated worker + frontend, with PostgreSQL either managed
(recommended) or bundled via the `bundled-db` profile.

```bash
cd deployments
cp .env.production.example .env.production   # fill in EVERYTHING; chmod 600
docker compose -f docker-compose.prod.yml --env-file .env.production up -d
# or, with the bundled database:
docker compose -f docker-compose.prod.yml --env-file .env.production \
  --profile bundled-db up -d
```

- **TLS**: terminate at a reverse proxy (Caddy/Traefik/nginx) in front of
  `:80`; `deployments/binary/nginx-tsm.conf` is a ready nginx shape if you
  prefer host nginx in front of the containers.
- **Metrics** (`:9090`) are intentionally unpublished; scrape from the
  compose network (see `deployments/observability/prometheus.yml`) or add a
  firewalled host binding.
- The frontend service MUST keep the name `backend` for its API upstream —
  the image's nginx config targets `http://backend:8080`.
- First boot on an empty database runs all migrations and seeds roles + the
  default org; then follow [initial-setup.md](../initial-setup.md).
- Known quirk: `up` can report the backend unhealthy while it waits for the
  database on first boot; re-run the same `up -d` once `curl
  localhost:8080/ready` returns 200.
