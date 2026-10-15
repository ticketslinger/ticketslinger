-- +goose Up
-- Since storefront reads moved to ts_system, ts_public runs one query:
-- resolveLive's lookup of a live event by its organizer and event slugs
-- (organizers.id, slug; events.id, organizer_id, slug). Everything else it
-- could read or run goes. Policies aren't checked against column grants, so
-- events_public still sees status.
DROP POLICY questions_public ON questions;
DROP POLICY question_applies_to_public ON question_applies_to;
DROP POLICY ticket_types_public ON ticket_types;
REVOKE SELECT ON ticket_types, questions, question_applies_to FROM ts_public;
REVOKE EXECUTE ON FUNCTION event_availability(uuid) FROM ts_public;
REVOKE SELECT (id, name, slug) ON organizers FROM ts_public;
GRANT SELECT (id, slug) ON organizers TO ts_public;
REVOKE SELECT ON events FROM ts_public;
GRANT SELECT (id, organizer_id, slug) ON events TO ts_public;

-- +goose Down
REVOKE SELECT (id, organizer_id, slug) ON events FROM ts_public;
GRANT SELECT ON events TO ts_public;
REVOKE SELECT (id, slug) ON organizers FROM ts_public;
GRANT SELECT (id, name, slug) ON organizers TO ts_public;
GRANT EXECUTE ON FUNCTION event_availability(uuid) TO ts_public;
GRANT SELECT ON ticket_types, questions, question_applies_to TO ts_public;
CREATE POLICY ticket_types_public ON ticket_types FOR SELECT TO ts_public
  USING (NOT hidden AND event_id IN (SELECT id FROM events));
CREATE POLICY question_applies_to_public ON question_applies_to FOR SELECT TO ts_public
  USING (ticket_type_id IN (SELECT id FROM ticket_types));
CREATE POLICY questions_public ON questions FOR SELECT TO ts_public
  USING (event_id IN (SELECT id FROM events) AND (applies_to_all OR id IN (SELECT question_id FROM question_applies_to)));
