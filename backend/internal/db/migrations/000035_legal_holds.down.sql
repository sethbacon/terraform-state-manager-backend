-- Reversing this DROPS EVERY RECORDED HOLD.
--
-- A hold is the record of a decision to preserve evidence, so losing it does
-- not just re-expose the audit rows to the next sweep -- it erases the fact
-- that someone decided they should not be swept. Reverse only on a deployment
-- where retention was never enabled.
DROP INDEX IF EXISTS "public"."idx_legal_holds_active_range";
DROP TABLE IF EXISTS "public"."legal_holds";
