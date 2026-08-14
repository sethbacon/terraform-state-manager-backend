-- 000030_platform_admins
--
-- TSM's own platform-admin carrier, plus the transactional audit outbox and the
-- constraint trigger that binds one to the other.
-- Refs sethbacon/terraform-suite-identity#206 (Phase 2, state-manager side).
--
-- WHAT THIS ADDS THAT DID NOT EXIST. Until now TSM had no way to express "who
-- administers this deployment" at all: the `admin` wildcard came only from an
-- admin-bearing role template joined through organization membership, which is
-- SHARED identity state. Under the agreed model identity is shared and
-- authorization is per-app, so "who administers THIS app" is a row in THIS
-- app's schema. That is `platform_admins`.
--
-- NON-BREAKING BY CONSTRUCTION. Effective admin during this phase is
-- `carrier OR the existing scope union` (internal/platformadmin/service.go,
-- SessionScopes). The carrier can only ADD authority here; a deployment that
-- never populates it behaves exactly as it does today. Switching reads to the
-- carrier alone is a later phase and a separate, breaking change.
--
-- NO FOREIGN KEYS, DELIBERATELY. `user_id` and `granted_by` name
-- `identity.users` rows and carry no constraint, because identity may live in
-- this database's `identity` schema OR in a separate database entirely
-- (TSM_IDENTITY_DATABASE_*), where Postgres cannot express a foreign key at
-- all. This migration runs on the APP connection; the identity connection is a
-- different pool. Registry's 000046 and 000051 reached the same conclusion for
-- the same reason. What the FK would have bought is paid for in code: user ids
-- are UUIDs and are never reused, every elevation path loads the principal
-- before consulting the carrier, and the never-zero floor counts only grants
-- that still RESOLVE (platformadmin.RequireAnotherExercisableAdmin), so an
-- orphaned row elevates nobody and is not counted as a remaining administrator.
--
-- NO BACKFILL. Registry's 000051 backfilled from admin-bearing role templates
-- because it had to: it was already partway to deriving authority from the
-- carrier alone. TSM is not, and a backfill here would silently mint platform
-- administrators from a role assignment nobody made with this table in mind.
-- The first administrator arrives through the setup wizard
-- (internal/api/setup/admin.go), and an existing deployment's current admins —
-- who still hold `admin` through the scope union — populate the carrier through
-- POST /api/v1/admin/platform-admins.
--
-- THE SQL BELOW IS RENDERED, NOT HAND-WRITTEN. Every statement is the verbatim
-- output of the shared library's own DDL renderers, which are the definitions
-- its statements and its constraint trigger were written against.
-- internal/db/migration_ddl_test.go re-renders all five blocks and fails if this
-- file has drifted from them. Do not edit the rendered blocks by hand: three
-- registry migrations each had to rediscover the trigger contract, and this is
-- how that stops happening.

-- ===========================================================================
-- platformadmin.TableDDL("platform_admins")
-- ===========================================================================
CREATE TABLE IF NOT EXISTS "platform_admins" (
    user_id     UUID        PRIMARY KEY,
    granted_by  UUID,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    note        TEXT
);

-- ===========================================================================
-- auditoutbox.OutboxDDL("audit_outbox")
--
-- The outbox lives on the APP connection, beside the carrier, so an audit
-- intent commits in the SAME transaction as the mutation it describes. The
-- destination — identity.audit_logs — is on the identity connection, which may
-- be another schema or another database; those two cannot share a transaction,
-- which is exactly why the intent is written here and delivered afterwards by
-- the relay (internal/platformadmin, auditoutbox.Relay).
-- ===========================================================================
-- Rendered by identity/auditoutbox.OutboxDDL('audit_outbox').
-- The transactional audit outbox: an audit INTENT written in the same
-- transaction as the privileged mutation it describes, delivered afterwards.
CREATE TABLE IF NOT EXISTS "audit_outbox" (
    -- Chosen by the writer BEFORE the mutation commits, and reused verbatim as
    -- the destination row's id. That is what makes redelivery idempotent: the
    -- second attempt conflicts on the destination's primary key.
    event_id        UUID         PRIMARY KEY,
    -- The transaction that wrote this intent. Read only by the trigger.
    txid            xid8         NOT NULL DEFAULT pg_current_xact_id(),
    -- When the audited event happened, not when it was delivered. Delivery may
    -- be minutes later; the audit trail must say the former.
    occurred_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    action          VARCHAR(500) NOT NULL,
    actor_user_id   UUID,
    -- The actor's address as it stood at the time, denormalised so the entry
    -- stays attributable after the user row is gone. This package never
    -- resolves it from a users table: identity may be another database.
    actor_email     VARCHAR(255),
    organization_id UUID,
    resource_type   VARCHAR(100),
    resource_id     VARCHAR(255),
    ip_address      VARCHAR(45),
    metadata        JSONB,
    -- Delivery bookkeeping. delivered_at IS NULL is the backlog.
    delivered_at    TIMESTAMPTZ,
    attempts        INTEGER      NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- The relay's claim scan. Partial, on the undelivered rows only, so it stays
-- small no matter how much delivered history has yet to be pruned.
CREATE INDEX IF NOT EXISTS "audit_outbox_pending_idx"
    ON "audit_outbox" (occurred_at, event_id) WHERE delivered_at IS NULL;

-- The pruner's scan over delivered history.
CREATE INDEX IF NOT EXISTS "audit_outbox_delivered_idx"
    ON "audit_outbox" (delivered_at) WHERE delivered_at IS NOT NULL;

-- The trigger's same-transaction lookup. Every commit that touches a guarded
-- table runs it, so it is not optional.
CREATE INDEX IF NOT EXISTS "audit_outbox_txid_idx"
    ON "audit_outbox" (txid, resource_type, resource_id);

-- Raises unless the CURRENT transaction has already written an intent naming
-- this subject with this action.
--
-- resource_id is compared case-insensitively: uuid::text is canonical lower
-- case, but an operator writing an intent by hand may not be.
CREATE OR REPLACE FUNCTION "audit_outbox_assert_intent"(
    subject TEXT, resource TEXT, expected_action TEXT
) RETURNS void AS $$
BEGIN
    IF subject IS NULL THEN
        RAISE EXCEPTION 'audit outbox: refusing a % mutation with no subject to audit', resource
            USING ERRCODE = '23514';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM "audit_outbox" o
         WHERE o.txid = pg_current_xact_id()
           AND o.resource_type = resource
           AND lower(o.resource_id) = lower(subject)
           AND o.action = expected_action
    ) THEN
        RAISE EXCEPTION 'audit outbox: % on % has no audit intent in this transaction (expected a audit_outbox row with action=%, resource_type=%, resource_id=%)',
            expected_action, subject, expected_action, resource, subject
            USING ERRCODE = '23514',
                  HINT = 'Write the audit intent in the same transaction as the mutation: identity/auditoutbox, Outbox.Enqueue.';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- ===========================================================================
-- auditoutbox.TriggerSpec{Outbox: "audit_outbox", Table: "platform_admins",
--   SubjectColumn: "user_id", ResourceType: platformadmin.AuditResourceType,
--   OnInsert: platformadmin.AuditActionGranted,
--   OnUpdate: "platform_admin.updated",
--   OnDelete: platformadmin.AuditActionRevoked}.DDL()
--
-- The action strings are pinned in the database on purpose: an intent that
-- merely mentioned the subject would let a revocation commit under a grant's
-- record. Insert and delete take the library's own constants, so a rename in Go
-- that is not re-rendered here fails the COMMIT rather than passing unaudited.
--
-- UPDATE has no library constant because the library has no update path —
-- Grant is ON CONFLICT (user_id) DO NOTHING, which preserves the original
-- provenance, and Revoke deletes. It is guarded all the same, with a literal:
-- leaving an operation unguarded is a way to change the row with no record, and
-- since nothing writes 'platform_admin.updated', any UPDATE that appears fails
-- at commit. That is the correct direction to fail.
-- ===========================================================================
-- Rendered by identity/auditoutbox.TriggerSpec.DDL for platform_admins.
CREATE OR REPLACE FUNCTION "platform_admins_require_audit_intent"() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM "audit_outbox_assert_intent"(NEW."user_id"::text, 'platform_admin', 'platform_admin.granted');
    END IF;
    IF TG_OP = 'UPDATE' THEN
        PERFORM "audit_outbox_assert_intent"(OLD."user_id"::text, 'platform_admin', 'platform_admin.updated');
        IF NEW."user_id" IS DISTINCT FROM OLD."user_id" THEN
            PERFORM "audit_outbox_assert_intent"(NEW."user_id"::text, 'platform_admin', 'platform_admin.updated');
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN
        PERFORM "audit_outbox_assert_intent"(OLD."user_id"::text, 'platform_admin', 'platform_admin.revoked');
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- DEFERRABLE INITIALLY DEFERRED: the check runs at COMMIT, so the mutation and
-- its intent may be written in either order within the transaction, and the
-- failure aborts the commit rather than one statement.
DROP TRIGGER IF EXISTS "platform_admins_require_audit_intent" ON "platform_admins";
CREATE CONSTRAINT TRIGGER "platform_admins_require_audit_intent"
    AFTER INSERT OR UPDATE OR DELETE ON "platform_admins"
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION "platform_admins_require_audit_intent"();
