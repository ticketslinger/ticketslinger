-- +goose Up
-- +goose StatementBegin
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ts_public')
     OR NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ts_system') THEN
    RAISE EXCEPTION 'create roles ts_public and ts_system first (deploy/initdb/01-roles.sql)';
  END IF;
END $$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO ts_public, ts_system;

-- The organizer a system transaction acts for. No membership check: system
-- code names its organizer explicitly; this keeps one transaction in one tenant.
CREATE FUNCTION app_scope() RETURNS uuid LANGUAGE sql STABLE
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid $$;

-- Existing policies are TO PUBLIC and call app_organizer(); the new roles
-- evaluate them too (they return NULL there, matching nothing), so they need EXECUTE.
GRANT EXECUTE ON FUNCTION app_organizer() TO ts_public, ts_system;
-- org_reads (00002) has a subquery on memberships, which runs with the
-- caller's rights. member_reads shows these roles no rows.
GRANT SELECT ON memberships TO ts_public, ts_system;

-- Storefront reads.
GRANT SELECT (id, name, slug) ON organizers TO ts_public;
GRANT SELECT ON events, ticket_types, questions, question_applies_to TO ts_public;
CREATE POLICY events_public ON events FOR SELECT TO ts_public USING (status = 'live');
-- Only organizers with something on sale.
CREATE POLICY organizers_public ON organizers FOR SELECT TO ts_public
  USING (id IN (SELECT organizer_id FROM events));
CREATE POLICY ticket_types_public ON ticket_types FOR SELECT TO ts_public
  USING (NOT hidden AND event_id IN (SELECT id FROM events));
-- A question asked only of hidden ticket types stays hidden with them.
CREATE POLICY question_applies_to_public ON question_applies_to FOR SELECT TO ts_public
  USING (ticket_type_id IN (SELECT id FROM ticket_types));
CREATE POLICY questions_public ON questions FOR SELECT TO ts_public
  USING (event_id IN (SELECT id FROM events) AND (applies_to_all OR id IN (SELECT question_id FROM question_applies_to)));

-- System: one organizer at a time.
GRANT SELECT ON organizers, events, ticket_types, quotas, quota_ticket_types, promo_codes,
  promo_applies_to, promo_unlocks, questions, question_applies_to TO ts_system;
GRANT UPDATE (used_count) ON promo_codes TO ts_system;
CREATE POLICY organizers_system ON organizers FOR SELECT TO ts_system USING (id = (SELECT app_scope()));
CREATE POLICY events_system ON events FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY ticket_types_system ON ticket_types FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY quotas_system ON quotas FOR ALL TO ts_system
  USING (organizer_id = (SELECT app_scope())) WITH CHECK (false);
CREATE POLICY qtt_system ON quota_ticket_types FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY promo_codes_system ON promo_codes FOR ALL TO ts_system
  USING (organizer_id = (SELECT app_scope())) WITH CHECK (organizer_id = (SELECT app_scope()));
CREATE POLICY promo_applies_to_system ON promo_applies_to FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY promo_unlocks_system ON promo_unlocks FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY questions_system ON questions FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
CREATE POLICY question_applies_to_system ON question_applies_to FOR SELECT TO ts_system USING (organizer_id = (SELECT app_scope()));
-- SELECT ... FOR UPDATE on quotas needs UPDATE rights; WITH CHECK (false) above
-- means no row can actually be written.
GRANT UPDATE (size) ON quotas TO ts_system;

-- +goose Down
REVOKE UPDATE (size) ON quotas FROM ts_system;
DROP POLICY question_applies_to_system ON question_applies_to;
DROP POLICY questions_system ON questions;
DROP POLICY promo_unlocks_system ON promo_unlocks;
DROP POLICY promo_applies_to_system ON promo_applies_to;
DROP POLICY promo_codes_system ON promo_codes;
DROP POLICY qtt_system ON quota_ticket_types;
DROP POLICY quotas_system ON quotas;
DROP POLICY ticket_types_system ON ticket_types;
DROP POLICY events_system ON events;
DROP POLICY organizers_system ON organizers;
REVOKE UPDATE (used_count) ON promo_codes FROM ts_system;
REVOKE SELECT ON organizers, events, ticket_types, quotas, quota_ticket_types, promo_codes,
  promo_applies_to, promo_unlocks, questions, question_applies_to FROM ts_system;
DROP POLICY questions_public ON questions;
DROP POLICY question_applies_to_public ON question_applies_to;
DROP POLICY ticket_types_public ON ticket_types;
DROP POLICY organizers_public ON organizers;
DROP POLICY events_public ON events;
REVOKE SELECT ON events, ticket_types, questions, question_applies_to FROM ts_public;
REVOKE SELECT (id, name, slug) ON organizers FROM ts_public;
REVOKE SELECT ON memberships FROM ts_public, ts_system;
REVOKE EXECUTE ON FUNCTION app_organizer() FROM ts_public, ts_system;
DROP FUNCTION app_scope();
REVOKE USAGE ON SCHEMA public FROM ts_public, ts_system;
