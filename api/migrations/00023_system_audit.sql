-- +goose Up
-- An answer about a ticket belongs to that ticket's own order.
ALTER TABLE tickets ADD CONSTRAINT tickets_id_order_id_key UNIQUE (id, order_id);
ALTER TABLE answers ADD CONSTRAINT answers_ticket_order_fkey
  FOREIGN KEY (ticket_id, order_id) REFERENCES tickets (id, order_id) ON DELETE CASCADE;

-- The system writes audit rows for what it decides alone (a late payment
-- refunded). They name no user and no key, and say so: a row whose user was
-- deleted (SET NULL) still isn't the system's.
ALTER TABLE audit_log ADD COLUMN actor_system boolean NOT NULL DEFAULT false,
  ADD CONSTRAINT audit_system_alone CHECK (NOT actor_system OR (actor_user_id IS NULL AND actor_api_key_id IS NULL));
-- Members and keys never write a row as the system.
ALTER POLICY audit_insert ON audit_log WITH CHECK (organizer_id = (SELECT app_organizer()) AND NOT actor_system);
GRANT INSERT ON audit_log TO ts_system;
CREATE POLICY audit_system ON audit_log FOR INSERT TO ts_system
  WITH CHECK (organizer_id = (SELECT app_scope()) AND actor_system AND actor_user_id IS NULL AND actor_api_key_id IS NULL);

-- A confirmation locks the questions its answers name (FOR KEY SHARE), so
-- none can be deleted between the check and the insert. Locking needs an
-- UPDATE right; WITH CHECK (false) keeps every real write out.
GRANT UPDATE (position) ON questions TO ts_system;
CREATE POLICY questions_system_lock ON questions FOR UPDATE TO ts_system
  USING (organizer_id = (SELECT app_scope())) WITH CHECK (false);

-- The buyer's order page in one statement: the order, its event and its
-- tickets, only when the code exists and the link's token hash matches. A
-- hit and a miss cost the same round trip, so the answer time says nothing
-- about which codes exist. What's compared is a SHA-256 of the token, so a
-- byte-by-byte comparison leaks nothing an attacker can steer.
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'secret', pg_catalog.encode(t.secret, 'hex')) ORDER BY t.id)
                   FROM public.tickets t JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
                  WHERE t.order_id = o.id), '[]'::jsonb)
    FROM public.orders o
    JOIN public.events e ON e.id = o.event_id
    JOIN public.organizers g ON g.id = o.organizer_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_by_link(text, bytea) TO ts_app;

-- +goose Down
DROP FUNCTION order_by_link(text, bytea);
DROP POLICY questions_system_lock ON questions;
REVOKE UPDATE (position) ON questions FROM ts_system;
DROP POLICY audit_system ON audit_log;
REVOKE INSERT ON audit_log FROM ts_system;
ALTER POLICY audit_insert ON audit_log WITH CHECK (organizer_id = (SELECT app_organizer()));
ALTER TABLE audit_log DROP CONSTRAINT audit_system_alone, DROP COLUMN actor_system;
ALTER TABLE answers DROP CONSTRAINT answers_ticket_order_fkey;
ALTER TABLE tickets DROP CONSTRAINT tickets_id_order_id_key;
