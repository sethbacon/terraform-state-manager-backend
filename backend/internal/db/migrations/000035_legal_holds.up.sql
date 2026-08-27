-- Legal holds: audit rows inside an ACTIVE hold's date range are exempt from
-- the audit retention sweep (#373).
--
-- WHY THIS IS A NUMBERED MIGRATION AND NOT A STARTUP CreateTable.
--
-- The shared module exposes store.LegalHoldTableDDL, and it is tempting to run
-- it at boot so no migration is needed. terraform-registry-backend did exactly
-- that and it was a defect: a table created at startup lands wherever the
-- handle happens to point, so the schema the migration chain describes and the
-- schema the database has are different things. It also means a fresh database
-- and a migrated one can disagree. The DDL below is that helper's output,
-- transcribed, and legal_hold_ddl_test.go fails if the two ever diverge.
--
-- WHY public, AND WHY THE SWEEP IS TOLD SO EXPLICITLY.
--
-- audit_logs is reached through the IDENTITY pool, whose DSN carries
-- search_path=identity,public (cmd/server/main.go). An unqualified legal_holds
-- in the sweep's exemption would therefore resolve to identity.legal_holds if
-- one ever existed, and to public.legal_holds otherwise.
--
-- That is a live hazard, not a hypothetical: an identity.legal_holds appearing
-- later would SHADOW this table on the sweep's connection, so the hold API
-- would write here while the sweep read there. Every hold would look placed,
-- and the sweep would delete the rows anyway and report success.
--
-- The application therefore passes the name SCHEMA-QUALIFIED
-- ("public.legal_holds") to store.WithLegalHolds, which makes the shadowing
-- unreachable rather than merely unlikely. This table must live where that
-- name says.
--
-- On the default deployment the identity database inherits every unset field
-- from the primary, so this is the same physical database the sweep reads. A
-- deployment that splits them puts this table out of the sweep's reach, which
-- is why the retention job verifies reachability at startup and refuses to run
-- rather than sweeping unprotected.

CREATE TABLE IF NOT EXISTS "public"."legal_holds" (
    id           UUID PRIMARY KEY,
    name         TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    start_date   TIMESTAMPTZ NOT NULL,
    end_date     TIMESTAMPTZ NOT NULL,
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    placed_by    UUID,
    placed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_by  UUID,
    released_at  TIMESTAMPTZ,
    CONSTRAINT legal_holds_range CHECK (end_date >= start_date)
);

CREATE INDEX IF NOT EXISTS "idx_legal_holds_active_range" ON "public"."legal_holds" (start_date, end_date) WHERE active;
