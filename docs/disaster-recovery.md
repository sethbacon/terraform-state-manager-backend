# Disaster recovery

## What must be backed up

1. **PostgreSQL** — everything lives here: sources (with encrypted
   credentials), the analysis store + history, drift runs/records, edits +
   state BACKUPS (pre-write copies), transfers, schedules, identity
   (users/orgs/roles/API keys), audit log.
2. **`TSM_ENCRYPTION_KEY`** — see below.
3. (Only if you use `local`-type sources) the state files PVC/directory.

State in REMOTE sources (HCP/S3/Azure/git/…) is the backends' own data; TSM
stores analyses and pre-edit backups of it, not the canonical copies.

## Database backups

- Managed PG: enable the platform's automated backups/PITR (the Terraform
  modules set 14-day retention).
- Chart: `backup.enabled=true` adds a nightly `pg_dump` CronJob (custom
  format, compressed) uploaded to blob/S3/GCS.
- Compose/binary: cron `pg_dump --format=custom --compress=9`.

Restore: `pg_restore --clean --if-exists -d terraform_state_manager dump`;
start the backend (migrations reconcile forward if the dump predates the
binary); verify `/ready`, source Test connection, and that the dashboard
repopulates after a sync cycle.

## Encryption-key custody — read this one

`TSM_ENCRYPTION_KEY` (AES-256-GCM) protects every stored credential. There is
**no re-encryption tooling**: a database restore is only as good as the key
that encrypted it.

- **Losing the key** does not break login or stored analyses, but every
  source credential, CI token, and notification target becomes
  undecryptable — each must be re-entered by hand (sources show decrypt
  failures on use).
- **Escrow the key** in at least two places (e.g. Key Vault + offline vault),
  versioned alongside your DB backups so any restorable dump has its key.
- **Rotating the key** = generate new key → restart with it → re-enter every
  stored credential (source edit keeps non-secret config; paste secrets
  again; same for CI sources/pipeline tokens and notification targets). Plan
  it as a maintenance task, not a config flip.

## Scenario quick-paths

- **Pod/node loss**: stateless; reschedules. Worker gap = missed ticks only.
- **DB loss**: restore dump + same encryption key → full recovery.
- **Region loss**: restore DB in new region, redeploy chart with same
  secrets, repoint DNS, re-run the CI callback preflight (URL unchanged ⇒
  pipelines keep working).
- **Accidental state edit**: not a DR event — every edit/restore/transfer
  wrote a pre-write backup row; use the Backups tab to restore.
