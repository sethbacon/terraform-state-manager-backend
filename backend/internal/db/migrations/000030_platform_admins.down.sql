-- 000030_platform_admins (down)
--
-- ORDER IS LOAD-BEARING. The trigger goes first: dropping `audit_outbox` while
-- the trigger still reads it leaves every platform-admin mutation failing at
-- COMMIT with a missing relation instead of a clean refusal.
--
-- ROLLING BACK REOPENS THE HOLE. After this runs a platform-admin grant or
-- revoke can once again commit with no audit record. Undelivered intents are
-- destroyed with the table, so drain the backlog first if any of it still
-- matters:
--
--   SELECT count(*), min(occurred_at) FROM audit_outbox WHERE delivered_at IS NULL;
--
-- Delivered intents are safe to lose — their identity.audit_logs rows are the
-- record. The carrier itself is NOT safe to lose: dropping platform_admins
-- discards who administers this deployment, along with the granted_by/granted_at
-- provenance that cannot be reconstructed.
--
-- The first two blocks are the verbatim output of the library's own drop
-- renderers (auditoutbox.TriggerSpec.DropDDL and auditoutbox.OutboxDropDDL);
-- internal/db/migration_ddl_test.go re-renders them and fails on drift. The
-- carrier's DROP is hand-written because the library renders no down DDL for a
-- table it does not own.

-- ===========================================================================
-- auditoutbox.TriggerSpec{...}.DropDDL()
-- ===========================================================================
DROP TRIGGER IF EXISTS "platform_admins_require_audit_intent" ON "platform_admins";
DROP FUNCTION IF EXISTS "platform_admins_require_audit_intent"();

-- ===========================================================================
-- auditoutbox.OutboxDropDDL("audit_outbox")
-- ===========================================================================
DROP FUNCTION IF EXISTS "audit_outbox_assert_intent"(TEXT, TEXT, TEXT);
DROP TABLE IF EXISTS "audit_outbox";

-- The carrier last: the trigger above referenced it.
DROP TABLE IF EXISTS "platform_admins";

