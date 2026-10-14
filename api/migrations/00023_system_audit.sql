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

-- +goose Down
DROP POLICY audit_system ON audit_log;
REVOKE INSERT ON audit_log FROM ts_system;
ALTER POLICY audit_insert ON audit_log WITH CHECK (organizer_id = (SELECT app_organizer()));
ALTER TABLE audit_log DROP CONSTRAINT audit_system_alone, DROP COLUMN actor_system;
ALTER TABLE answers DROP CONSTRAINT answers_ticket_order_fkey;
ALTER TABLE tickets DROP CONSTRAINT tickets_id_order_id_key;
