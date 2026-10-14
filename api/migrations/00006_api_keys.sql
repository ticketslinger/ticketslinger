-- +goose Up
CREATE TABLE api_keys (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  organizer_id uuid NOT NULL REFERENCES organizers ON DELETE CASCADE,
  name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
  key_hash     bytea NOT NULL UNIQUE,   -- SHA-256 of the whole token
  prefix       text NOT NULL,           -- first 8 chars after tsk_, for display
  scope        text NOT NULL CHECK (scope IN ('read', 'write')),
  created_by   uuid REFERENCES users ON DELETE SET NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at   timestamptz
);
CREATE INDEX ON api_keys (organizer_id);

ALTER TABLE audit_log ADD COLUMN actor_api_key_id uuid REFERENCES api_keys ON DELETE SET NULL;
ALTER TABLE audit_log ADD CONSTRAINT audit_one_actor
  CHECK (actor_user_id IS NULL OR actor_api_key_id IS NULL);

CREATE FUNCTION app_api_key() RETURNS uuid LANGUAGE sql STABLE
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT NULLIF(pg_catalog.current_setting('app.api_key_id', true), '')::uuid $$;

-- A scope counts if the user is a member, or the API key is live and belongs
-- to that organizer. Never both: a transaction scopes one principal. The user
-- branch is 00002's rule unchanged. Revocation bites on the next statement.
CREATE OR REPLACE FUNCTION app_organizer() RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT o FROM (SELECT NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid AS o) s
      WHERE (public.app_api_key() IS NULL AND EXISTS (
               SELECT 1 FROM public.memberships m WHERE m.user_id = public.app_user() AND m.organizer_id = s.o))
         OR (public.app_user() IS NULL AND EXISTS (
               SELECT 1 FROM public.api_keys k WHERE k.id = public.app_api_key()
                 AND k.organizer_id = s.o AND k.revoked_at IS NULL)) $$;

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
-- Any principal in scope may read its organizer's keys; only a user may
-- create, change or delete one. Keys never manage keys, even with raw SQL.
CREATE POLICY api_keys_read ON api_keys FOR SELECT
  USING (organizer_id = (SELECT app_organizer()));
CREATE POLICY api_keys_insert ON api_keys FOR INSERT
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND (SELECT app_user()) IS NOT NULL);
CREATE POLICY api_keys_update ON api_keys FOR UPDATE
  USING (organizer_id = (SELECT app_organizer()) AND (SELECT app_user()) IS NOT NULL)
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND (SELECT app_user()) IS NOT NULL);
CREATE POLICY api_keys_delete ON api_keys FOR DELETE
  USING (organizer_id = (SELECT app_organizer()) AND (SELECT app_user()) IS NOT NULL);

-- Key lookup by hash before any scope exists: owner-run, returns only what
-- authentication needs, never the hash.
-- +goose StatementBegin
CREATE FUNCTION lookup_api_key(p_hash bytea) RETURNS TABLE(id uuid, organizer_id uuid, scope text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT k.id, k.organizer_id, k.scope FROM public.api_keys k
   WHERE k.key_hash = p_hash AND k.revoked_at IS NULL
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION lookup_api_key(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lookup_api_key(bytea) TO ts_app;

-- Records a key's use, at most once a minute. Owner-run because a key has
-- no scope yet when it authenticates; touches last_used_at and nothing else.
-- +goose StatementBegin
CREATE FUNCTION touch_api_key(p_id uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  UPDATE public.api_keys SET last_used_at = pg_catalog.now()
   WHERE id = p_id AND revoked_at IS NULL
     AND (last_used_at IS NULL OR last_used_at < pg_catalog.now() - interval '1 minute')
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION touch_api_key(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION touch_api_key(uuid) TO ts_app;

-- +goose Down
DROP FUNCTION touch_api_key(uuid);
DROP FUNCTION lookup_api_key(bytea);
DROP POLICY api_keys_delete ON api_keys;
DROP POLICY api_keys_update ON api_keys;
DROP POLICY api_keys_insert ON api_keys;
DROP POLICY api_keys_read ON api_keys;
CREATE OR REPLACE FUNCTION app_organizer() RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT m.organizer_id FROM public.memberships m
      WHERE m.user_id = public.app_user()
        AND m.organizer_id = NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid $$;
DROP FUNCTION app_api_key();
ALTER TABLE audit_log DROP CONSTRAINT audit_one_actor, DROP COLUMN actor_api_key_id;
DROP TABLE api_keys;
