-- 000033_organization_partition
--
-- PHASE 1 OF FOUR of sethbacon/terraform-state-manager-backend#393: give TSM's
-- domain tables an organization, so a later phase can make an organization see
-- only its own data. Refs #393; does not close it.
--
-- ISOLATION, NOT ATTRIBUTION. This column is not a reporting dimension. It is
-- the partition key a tenant boundary will be built on, and every decision below
-- is made so that Phase 4 can make it NOT NULL and re-key the uniqueness
-- constraints onto it.
--
-- WHAT IS BROKEN TODAY, stated plainly because this migration is the first step
-- of the fix and not the fix. TSM's session scopes are a UNION across every
-- organization the principal belongs to — internal/approles/reads.go:177-193
-- collects `mem.RoleTemplateScopes` into one flat set and discards which
-- membership each scope came from — and middleware.RequireScope
-- (internal/middleware/rbac.go:20) tests that flat set with no organization
-- dimension at all. No repository in internal/db/repositories carries an
-- organization predicate. So a caller holding `state:read` in ANY ONE
-- organization can read EVERY source, state file, drift record, schedule, lock
-- and CI credential in the deployment.
--
-- NOTHING READS THIS COLUMN YET, AND THAT IS THE POINT. Phase 2 dual-reads
-- behind a flag and proves equivalence, Phase 3 flips reads, Phase 4 is the
-- breaking one (NOT NULL, per-organization unique indexes, flag removed). A
-- migration that added the column AND started filtering on it would be a partial
-- cutover, which is how a deployment ends up half-isolated and nobody can say
-- which half.
--
-- ============================================================================
-- WHY THE BACKFILL IS NOT IN THIS FILE
-- ============================================================================
--
-- The obvious `UPDATE ... SET organization_id = (SELECT id FROM organizations
-- WHERE name = 'default')` cannot work here, for three independent reasons, any
-- one of which is sufficient:
--
--   ORDERING. cmd/server/main.go runs THIS migration at line 220. It runs the
--   identity schema's migrations at line 256 and bootstrap.Run — which is what
--   calls ensureDefaultOrg (internal/bootstrap/bootstrap.go:140) — after that.
--   On a fresh install the organizations table does not exist when this file
--   runs, and on the very first boot the default organization's ROW does not
--   exist either. A backfill here would read from a table that is not there yet.
--
--   TOPOLOGY. Identity may live in a separate DATABASE entirely
--   (TSM_IDENTITY_DATABASE_*, config.go:25-29). This migration runs on the
--   APPLICATION pool; the identity pool is a different connection to a possibly
--   different server. Postgres cannot express the read, never mind the FK. This
--   is the same conclusion 000030 and 000032 reached, for the same reason.
--
--   000032 ACTIVELY FORBIDS IT. That migration's routing pre-check (lines 64-72)
--   RAISES if identity's tables are reachable unqualified from this connection.
--   So on every deployment where this file can run at all, unqualified
--   `organizations` is guaranteed NOT to resolve. A backfill written that way
--   would fail on exactly the deployments that reached this migration.
--
-- The backfill therefore runs in Go, at startup, on the app connection, AFTER
-- bootstrap.Run has ensured the default organization and can hand its id across
-- the connection boundary: internal/tenancy.Backfill, called from
-- internal/bootstrap.Run. It is idempotent and re-runs on every boot, which is
-- also what stamps rows written by a replica still running the previous build.
--
-- ============================================================================
-- NO FOREIGN KEY TO organizations
-- ============================================================================
--
-- Same reason 000030 and 000032 declined one, restated because it is the first
-- question a reviewer asks: organization_id names an identity.organizations row,
-- identity may be another database, and a cross-database foreign key is not
-- expressible. What the FK would have bought is paid for above the database —
-- the id is a uuid, uuids are never reused, and every value written here comes
-- from a row bootstrap.Run has just confirmed exists.

-- ===========================================================================
-- THE DEFAULT-ORGANIZATION CARRIER
--
-- The app connection cannot ask identity which organization is the default, so
-- the answer is cached HERE, on the app side, by the startup backfill. Then a
-- column DEFAULT can reference it and every INSERT is stamped without a single
-- repository having to learn about tenancy.
--
-- WHY A DEFAULT RATHER THAN NINE REPOSITORY EDITS. Phase 4 makes this column
-- NOT NULL. That step fails if ANY write path was missed — and "missed one
-- INSERT" is invisible until the phase that cannot tolerate it. A column default
-- is applied by the database to every INSERT that does not name the column,
-- including ones in code paths a reviewer never opened, including ones added
-- between now and Phase 4 by someone who has never heard of #393. It is the only
-- mechanism here that is complete by construction rather than by inspection.
--
-- system_settings rather than a new table: it is already the app schema's
-- singleton settings row (000017, with the id = 1 CHECK and the row seeded), and
-- 000022 and 000027 already extend it exactly this way.
-- ===========================================================================
ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS default_organization_id UUID;

-- STABLE, not VOLATILE: it reads one row and the planner may cache it within a
-- statement. Not SECURITY DEFINER — it reads a table the caller can already read,
-- and a definer-rights function here would be a privilege boundary nobody asked
-- for.
--
-- Returns NULL until the startup backfill populates the carrier. A NULL stamp is
-- the correct behaviour for a server that has not yet completed bootstrap: the
-- column is nullable in this phase precisely so that state is representable, and
-- the next successful boot backfills it.
CREATE OR REPLACE FUNCTION tsm_default_organization_id() RETURNS uuid
    LANGUAGE sql
    STABLE
AS $$
    SELECT default_organization_id FROM system_settings WHERE id = 1
$$;

-- ===========================================================================
-- THE PARTITION ROOTS — nine tables that get their OWN organization_id.
--
-- ADD COLUMN and SET DEFAULT are deliberately two statements. `ADD COLUMN ...
-- DEFAULT <expression>` rewrites the whole table, because PostgreSQL's
-- no-rewrite fast path applies only to a CONSTANT default and a function call is
-- not one. That rewrite holds ACCESS EXCLUSIVE for its duration, which on a
-- deployment with a large drift_runs or health_runs is an outage. Splitting it
-- makes both halves catalog-only and effectively instant: the ADD COLUMN writes
-- no rows because it has no default to write, and SET DEFAULT touches no
-- existing rows by definition. The existing rows are handled by the Go backfill,
-- which is interruptible and does not hold a table lock.
-- ===========================================================================

-- state_sources — THE root. Every state file, backup, edit, lock, analysis and
-- module reference in the deployment is reachable from a row of this table, so
-- this is the column the whole partition hangs from.
--
-- Its name is GLOBALLY unique today (000001:17, idx_state_sources_name). Under
-- isolation that is wrong twice over: it lets one organization's naming choices
-- collide with another's, and the resulting error discloses that a source of
-- that name exists somewhere in the deployment. Phase 4 replaces it with
-- UNIQUE (organization_id, name). That is a breaking change and is NOT made
-- here — but it is why this column exists on this table rather than being
-- inherited from anywhere, and the plain index below is deliberately a prefix of
-- that future unique index.
ALTER TABLE state_sources ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE state_sources ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- pipeline_connections — a root: no source_id, nothing above it to inherit
-- from, and it holds an encrypted CI token (000004:10). Its name is globally
-- unique too (000004:14) and Phase 4 re-keys it the same way.
ALTER TABLE pipeline_connections ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE pipeline_connections ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- ci_sources — a root holding a credential for a whole CI organization
-- (000011:11, plus the app-auth and GitHub-App secrets added by 000019/000020).
--
-- NAME COLLISION WARNING, and it is a real one: ci_sources ALREADY has a column
-- called `organization` (000011:9). That is the AZURE DEVOPS organization or
-- GITHUB owner — a remote coordinate, a string, nothing to do with tenancy. The
-- new column is `organization_id`, a uuid naming an identity.organizations row.
-- Two different concepts, one letter apart in the same table. Anybody writing
-- the Phase 3 predicate must not reach for the wrong one.
ALTER TABLE ci_sources ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE ci_sources ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- notification_channels — a root whose encrypted_target is a capability-bearing
-- secret (000009:8): a Slack or generic webhook URL that anyone holding it can
-- post to. Globally-unique name (000009:16).
ALTER TABLE notification_channels ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE notification_channels ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- schedules — a root, and this one CANNOT inherit even though it looks like it
-- should. It refers to a source through target_config JSONB (000008:9), with no
-- foreign key and no column: `{ pipeline_connection_id, source_id, ... }`. There
-- is nothing for a join to follow, so an inherited answer would have to be
-- computed by parsing JSON on every read. Its own column is both cheaper and the
-- only one that stays correct when the referenced source is deleted. Globally
-- unique name (000008:18).
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE schedules ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- state_transfers — has TWO NOT NULL source references, source_id AND
-- target_source_id (000003:5,7). Inheriting is not merely awkward here, it is
-- AMBIGUOUS: a transfer would have two answers to "whose is this", and under
-- isolation the interesting fact is not either one — it is that they must AGREE.
-- A state transfer whose two ends sit in different organizations is a supported
-- way to move a state file ACROSS the tenant boundary this phase exists to draw.
-- Giving the transfer its own column is what lets Phase 4 express the invariant
-- (a composite reference to each end's (id, organization_id)) instead of leaving
-- it as an unwritten assumption.
ALTER TABLE state_transfers ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE state_transfers ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- drift_runs — cannot inherit: BOTH its parent references are nullable and
-- ON DELETE SET NULL (000004:20-21). Delete the source and the run survives with
-- source_id NULL, still holding its state_key, its plan summary and its
-- callback_token — a bearer credential the CI job authenticates its result POST
-- with. An inherited answer would be NULL for exactly those rows, which is to say
-- unpartitioned, which is to say readable by everyone. That is the failure mode
-- this whole issue is about.
ALTER TABLE drift_runs ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE drift_runs ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- drift_records — same nullable-parent argument (000013:10, ON DELETE SET NULL),
-- and the highest-value table in the set: it is the durable, acknowledgeable
-- record of what is currently wrong with somebody's infrastructure, including
-- the resource addresses being destroyed. Its live-record and external_ref
-- unique indexes are keyed on (source_id, ...) (000013:32,37), so they follow
-- the source and do not need re-keying in Phase 4 — the column here is for the
-- read predicate and for the rows whose source_id has gone NULL.
ALTER TABLE drift_records ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE drift_records ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- health_runs — has no source_id AT ALL (000005), only a nullable
-- ON DELETE SET NULL pipeline_connection_id. There is no ownership edge to
-- inherit along even in principle, and it too carries a callback_token.
ALTER TABLE health_runs ADD COLUMN IF NOT EXISTS organization_id UUID;
ALTER TABLE health_runs ALTER COLUMN organization_id SET DEFAULT tsm_default_organization_id();

-- ===========================================================================
-- WHAT DELIBERATELY DOES NOT GET A COLUMN
--
-- INHERITED THROUGH state_sources. These seven each carry source_id NOT NULL
-- with ON DELETE CASCADE, so every row has exactly one living parent and the
-- parent's organization is the row's organization, derivable by a join that
-- cannot return NULL:
--
--   state_backups (000002:7)          state_edits (000002:21)
--   state_locks (000007:7)            state_analyses (000012:7)
--   source_sync_status (000012:26)    state_analysis_history (000014:8)
--   state_module_refs (000015:11)
--
-- Adding a redundant column to each is a DIFFERENT AND WORSE DESIGN, not a
-- belt-and-braces version of this one. Two answers to "whose is this row" can
-- disagree, and the copy is the one that goes stale — so the failure mode it
-- introduces is a row that reads as one organization's through the join and
-- another's through the column, in a predicate that decides who may read a
-- Terraform state file. state_analysis_history is also the largest table in the
-- schema; widening it buys nothing a join does not already give.
--
-- NOT DOMAIN DATA. These are deployment-level or identity-level and an
-- organization column would be meaningless or actively harmful:
--
--   system_settings (000017)  singleton; setup-wizard state for the DEPLOYMENT.
--   sso_settings (000010)     singleton; the deployment's OIDC group mapping.
--   oidc_configs (000018)     the deployment's auth provider.
--   login_states (000026)     pre-authentication transient. There is no
--                             principal yet, so there is no organization yet.
--   user_token_revocations    a per-USER watermark, and a user spans
--     (000028)                organizations. Partitioning it would let a
--                             revocation raised in one organization fail to
--                             retire sessions whose authority came from
--                             another — a revocation that does not revoke.
--   platform_admins (000030)  administers the deployment; above organizations
--                             by definition.
--   audit_outbox (000030:88)  ALREADY has organization_id, as denormalised
--                             ATTRIBUTION on an audit intent. It is an outbox,
--                             not tenant data, and its column is not this one.
--   role_templates (000032)   TSM's own role -> scope definitions.
--                             Deployment-wide on purpose; 000032:92 makes the
--                             point that its uniqueness is already per-APP.
--   organization_member_roles ALREADY keyed by organization_id, as half its
--     (000032:124)            primary key. It is the authorization mirror that
--                             will ANSWER the tenancy question, not a table
--                             that has one.
--
-- ONE TABLE IS UNDECIDED AND IS LEFT ALONE ON PURPOSE: workflow_templates
-- (000021). It is operator-authored CI YAML keyed UNIQUE (provider, kind,
-- profile) and seeded with deployment-wide built-ins (api.SeedWorkflowTemplates),
-- so the only sensible column here would use NULL to mean "the shared built-in".
-- That is a column which can never become NOT NULL, and Phase 4's whole shape is
-- NOT NULL — so adding it now would guarantee a carve-out in the phase that is
-- supposed to close the gaps. Whether operator templates are per-organization
-- content or deployment configuration is a product decision, and it is raised in
-- #393 rather than settled by a migration.
-- ===========================================================================

-- ===========================================================================
-- INDEXES
--
-- Created NOW rather than in the phase that starts filtering, because the phase
-- that starts filtering is the one under time pressure and a sequential scan of
-- drift_records on every list request is how a correctness change becomes a
-- performance incident. They are useless until then, and cheap: the column is
-- entirely NULL until the first backfill and single-valued after it, so each is
-- a handful of pages.
--
-- state_sources' index is deliberately a PREFIX of the Phase 4
-- UNIQUE (organization_id, name), so Phase 4 drops it as redundant rather than
-- leaving two overlapping indexes behind.
-- ===========================================================================
CREATE INDEX IF NOT EXISTS idx_state_sources_org          ON state_sources (organization_id);
CREATE INDEX IF NOT EXISTS idx_pipeline_connections_org   ON pipeline_connections (organization_id);
CREATE INDEX IF NOT EXISTS idx_ci_sources_org             ON ci_sources (organization_id);
CREATE INDEX IF NOT EXISTS idx_notification_channels_org  ON notification_channels (organization_id);
CREATE INDEX IF NOT EXISTS idx_schedules_org              ON schedules (organization_id);
CREATE INDEX IF NOT EXISTS idx_state_transfers_org        ON state_transfers (organization_id);
CREATE INDEX IF NOT EXISTS idx_drift_runs_org             ON drift_runs (organization_id);
CREATE INDEX IF NOT EXISTS idx_drift_records_org          ON drift_records (organization_id);
CREATE INDEX IF NOT EXISTS idx_health_runs_org            ON health_runs (organization_id);
