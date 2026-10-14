-- +goose Up
ALTER TABLE organizers ADD COLUMN stripe_disconnected_at timestamptz,
  -- When the current account went live here (created, linked or switched
  -- back on). A deauthorize Stripe made before then is about an older link.
  ADD COLUMN stripe_connected_at timestamptz,
  -- How the account arrived: created through Account Links or linked through
  -- OAuth. Only linked accounts are deauthorized on disconnect.
  ADD COLUMN stripe_link_method text CHECK (stripe_link_method IN ('created', 'linked')),
  -- Part of the account-create idempotency key. It moves on only after Stripe
  -- refuses a create outright, since Stripe replays a refusal for 24 hours.
  ADD COLUMN stripe_create_attempt int NOT NULL DEFAULT 0 CHECK (stripe_create_attempt >= 0);
UPDATE organizers SET stripe_link_method = 'created' WHERE stripe_account_id IS NOT NULL;
ALTER TABLE organizers ADD CONSTRAINT organizers_link_method_with_account
  CHECK ((stripe_account_id IS NULL) = (stripe_link_method IS NULL));
-- One live link per account; a disconnected organizer may keep the old id.
ALTER TABLE organizers DROP CONSTRAINT organizers_stripe_account_id_key;
CREATE UNIQUE INDEX organizers_stripe_account_live ON organizers (stripe_account_id)
  WHERE stripe_account_id IS NOT NULL AND stripe_disconnected_at IS NULL;
-- account.application.deauthorized marks the organizer disconnected.
GRANT UPDATE (stripe_disconnected_at) ON organizers TO ts_system;

-- A link someone started: single use, ten minutes, tied to the session and
-- the organizer that started it. Only the hash of the state is kept.
CREATE TABLE stripe_link_states (
  state_hash   bytea PRIMARY KEY CHECK (length(state_hash) = 32),
  organizer_id uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
  session_id   uuid NOT NULL REFERENCES sessions ON DELETE CASCADE,
  expires_at   timestamptz NOT NULL DEFAULT now() + interval '10 minutes'
);
CREATE INDEX ON stripe_link_states (expires_at);
ALTER TABLE stripe_link_states ENABLE ROW LEVEL SECURITY;
CREATE POLICY stripe_link_states_scope ON stripe_link_states
  USING (organizer_id = (SELECT app_organizer())) WITH CHECK (organizer_id = (SELECT app_organizer()));

-- The callback knows only the state; this says which organizer to scope to.
CREATE FUNCTION stripe_link_scope(p_state_hash bytea) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT organizer_id FROM public.stripe_link_states WHERE state_hash = p_state_hash AND expires_at > now() $$;
REVOKE ALL ON FUNCTION stripe_link_scope(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stripe_link_scope(bytea) TO ts_app;

-- The sweep has no organizer to scope to, so it purges through this.
-- +goose StatementBegin
CREATE FUNCTION purge_expired_link_states(p_limit int) RETURNS bigint LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH gone AS (
    DELETE FROM public.stripe_link_states
     WHERE state_hash IN (SELECT state_hash FROM public.stripe_link_states WHERE expires_at < now() ORDER BY expires_at LIMIT p_limit)
    RETURNING 1)
  SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION purge_expired_link_states(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_expired_link_states(int) TO ts_app;

-- The organizer the account is live for, else the one that disconnected it
-- most recently, so a just-disconnected account's late webhooks still land.
CREATE OR REPLACE FUNCTION organizer_for_stripe_account(p_account text) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT id FROM public.organizers WHERE stripe_account_id = p_account
      ORDER BY stripe_disconnected_at IS NOT NULL, stripe_disconnected_at DESC LIMIT 1 $$;

-- +goose Down
CREATE OR REPLACE FUNCTION organizer_for_stripe_account(p_account text) RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT id FROM public.organizers WHERE stripe_account_id = p_account $$;
DROP FUNCTION purge_expired_link_states(int);
DROP FUNCTION stripe_link_scope(bytea);
DROP TABLE stripe_link_states;
REVOKE UPDATE (stripe_disconnected_at) ON organizers FROM ts_system;
DROP INDEX organizers_stripe_account_live;
ALTER TABLE organizers ADD CONSTRAINT organizers_stripe_account_id_key UNIQUE (stripe_account_id);
ALTER TABLE organizers DROP CONSTRAINT organizers_link_method_with_account;
ALTER TABLE organizers DROP COLUMN stripe_create_attempt, DROP COLUMN stripe_link_method, DROP COLUMN stripe_connected_at, DROP COLUMN stripe_disconnected_at;
