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

## One-time notice: state sources are confined to configured roots

The release that introduces `statesource.local_roots` confines the two connector
types that name a path on the **server's own filesystem** — `local` (`base_path`)
and `kubernetes` when it is given a `kubeconfig` file — to directories the
operator lists. The lists **fail closed**: empty (the default) permits nothing,
so an existing `local` source stops resolving the moment the new binary boots
unless its directory is listed.

Before rolling this release, if you use `local` sources (or a kubeconfig-based
`kubernetes` source):

- Note each source's `base_path` (`GET /api/v1/sources`).
- Set `TSM_STATESOURCE_LOCAL_ROOTS` to the directory (or directories) that
  contain them — the mount point is usually the right entry, e.g.
  `/data/states`, not each individual sub-path. Helm sets this automatically
  from `localStates.mountPath` when `localStates.enabled=true`.
- Set `TSM_STATESOURCE_KUBECONFIG_ROOTS` likewise for a mounted kubeconfig, or
  switch that source to `config.server` + `credentials.token`.
- Roll, then confirm the source lists states (`POST /api/v1/sources/{id}/test`).

Nothing is deleted and no source record changes — a source whose root is missing
simply reports that its `base_path` is outside the permitted roots until you add
it. Deployments that read state only from remote backends (HCP, S3, Azure Blob,
GCS, Consul, PG, HTTP, Git) need no action. See
[configuration.md](configuration.md#state-sources).

## One-time notice: automatic state-backup retention

Installs upgrading **to the release that introduced `backup_retention`** get the
sweep **enabled by default**, and its first worker cycle after the upgrade will
delete existing backups that fall outside the policy — older than 90 days *and*
not among the newest 20 for their state. On an install that has been accumulating
backups since day one, that first sweep can remove a large number of rows.

Deletion is permanent; the sweep has no undo. Before rolling this release:

- Confirm your DB backup (step 1 of the procedure above) is current — that is the
  only recovery path for pruned rows.
- If you have retention obligations that forbid automatic deletion, set
  `TSM_BACKUP_RETENTION_ENABLED=false` **before** rolling the worker replica.
- To keep the sweep but start gentler, raise `TSM_BACKUP_RETENTION_KEEP` or
  `TSM_BACKUP_RETENTION_MAX_AGE` for the first cycle, then tighten.

The sweep runs only on the worker leader (`workers.enabled=true`), so an
API-only replica rolling first changes nothing. See
[configuration.md](configuration.md#workers) and
[capacity-planning.md](capacity-planning.md).

## Version pinning

Always pin image tags in production (`v1.0.0`, never `latest`); the chart's
`appVersion` is only the default tag.
