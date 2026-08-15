-- 000032_app_role_authorization (down)
--
-- ORDER IS LOAD-BEARING: organization_member_roles carries the foreign key to
-- role_templates, so the assignments go first.
--
-- ROLLING BACK IS SAFE IN THIS PHASE, AND ONLY IN THIS PHASE. Reads still come
-- from identity.organization_members joined to identity.role_templates, so these
-- two tables are a mirror and nothing authorizes against them yet: dropping them
-- withdraws no authority and denies nobody. Everything in them is reconstructed
-- from identity by the startup reconcile (internal/approles.Reconcile) the next
-- time the migration is applied.
--
-- That stops being true the moment reads move here (Phase 3b). From then on,
-- dropping these tables IS dropping this deployment's authorization.
--
-- What is NOT reconstructed is anything an operator wrote into role_templates by
-- hand that identity does not also have — the reconcile copies identity's rows,
-- it does not invent them. In this phase there is no supported way to create
-- such a row (TSM exposes no role-template write API), so there should be none;
-- if this deployment has some, capture them first:
--
--   SELECT * FROM role_templates ORDER BY name;

DROP INDEX IF EXISTS organization_member_roles_mirrored_at_idx;
DROP INDEX IF EXISTS organization_member_roles_user_idx;
DROP TABLE IF EXISTS organization_member_roles;
DROP TABLE IF EXISTS role_templates;
