# Upgrade guide

## How upgrades work

The backend embeds its migrations and applies them on boot (app schema, then
the shared identity schema) under PostgreSQL advisory locks — concurrent
replicas can't race. Old replicas keep serving during a rolling upgrade;
migrations to date are additive.

Imperative control when you want it:

```bash
terraform-state-manager migrate up      # apply before rolling images
terraform-state-manager migrate down    # one step back (verify the matching .down.sql first)
```

## Procedure (any platform)

1. Take/verify a DB backup ([disaster-recovery.md](disaster-recovery.md)).
2. Read release notes for env-var or values changes.
3. Bump image tags (helm `--set backend.image.tag=… frontend.image.tag=…`,
   kustomize `images:`, compose `.env.production`).
4. Roll backend first, frontend second (the SPA is tolerant of a newer API;
   the reverse direction may reference endpoints an old backend lacks).
5. Verify `/health`, `/ready`, `/api/v1/version`, dashboard data, and that
   the worker logs a sync cycle.

Rollback = redeploy the previous tag. Schema rollbacks are rarely needed
(additive migrations); if a release notes otherwise, it ships explicit steps.

## Version pinning

Always pin image tags in production (`v1.0.0`, never `latest`); the chart's
`appVersion` is only the default tag.
