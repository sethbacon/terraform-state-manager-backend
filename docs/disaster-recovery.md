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

`TSM_ENCRYPTION_KEY` (AES-256-GCM) protects every stored credential. A database
restore is only as good as the key that encrypted it. Re-encryption tooling
exists for **one** column only — notification-channel targets, via
`rekey-targets` ([secrets-rotation.md](secrets-rotation.md#rotating-tsm_encryption_key)) —
and every other credential is still key-or-nothing.

- **Losing the key** does not break login or stored analyses, but every
  source credential, CI token, and notification target becomes
  undecryptable — each must be re-entered by hand (sources show decrypt
  failures on use).
- **Escrow the key** in at least two places (e.g. Key Vault + offline vault),
  versioned alongside your DB backups so any restorable dump has its key.
- **Rotating the key** = set the new key **and** `TSM_ENCRYPTION_KEY_PREVIOUS`
  to the old one → restart → re-enter every stored credential except
  notification targets (source edit keeps non-secret config; paste secrets
  again; same for CI sources and pipeline tokens) → run `rekey-targets`, then
  `rekey-targets verify`, and only drop the previous key once verify exits
  zero. Full runbook:
  [secrets-rotation.md](secrets-rotation.md#rotating-tsm_encryption_key). Plan
  it as a maintenance task, not a config flip.

## Scenario quick-paths

- **Pod/node loss**: stateless; reschedules. Worker gap = missed ticks only.
- **DB loss**: restore dump + same encryption key → full recovery.
- **Region loss**: restore DB in new region, redeploy chart with same
  secrets, repoint DNS, re-run the CI callback preflight (URL unchanged ⇒
  pipelines keep working).
- **Accidental state edit**: not a DR event — every edit/restore/transfer
  wrote a pre-write backup row; use the Backups tab to restore.

## RTO / RPO targets

TSM's recovery profile is simpler than the registry's: there is **no object
store** — recovery is a PostgreSQL restore plus the escrowed
`TSM_ENCRYPTION_KEY`.

| Tier | RPO (data loss) | RTO (time to recover) | Driven by |
|---|---|---|---|
| Managed PG + PITR | seconds–minutes | restore time + redeploy | platform PITR / 14-day retention |
| Nightly `pg_dump` | up to 24h | restore time + redeploy | dump cadence |

The encryption key is **not** part of RPO/RTO arithmetic — it is a constant
prerequisite: a restored dump is unusable without the matching key, so escrow it
versioned alongside every backup.

## DR drill checklist

Run periodically (e.g. quarterly) against a non-production environment:

1. Restore the latest dump (`pg_restore --clean --if-exists`) and start the
   backend with the **matching** `TSM_ENCRYPTION_KEY`.
2. Confirm `/ready` returns 200 (DB reachable, migrations reconciled).
3. Run **Test connection** on a state source — confirms reachability and config.
4. Confirm the dashboard repopulates after one sync cycle.
5. Confirm an **encrypted credential decrypts** (open a source that has stored
   secrets and use it — no decrypt error = the right key was escrowed).
