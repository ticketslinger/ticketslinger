-- +goose Up
-- Staff mode: a TicketSlinger staff member reads one organizer at a time,
-- for at most an hour, read-only, with every request on the record.
-- search_path ends in pg_temp so a temp table cannot shadow a real one.

-- Who is staff. ts_app has no privilege on this table at all: not even
-- SELECT. Only the owner role writes it, from the `staff` CLI command.
CREATE TABLE staff (
  user_id    uuid PRIMARY KEY REFERENCES users ON DELETE CASCADE,
  granted_at timestamptz NOT NULL DEFAULT now(),
  granted_by text NOT NULL,          -- $SUDO_USER on the droplet
  revoked_at timestamptz
);
REVOKE ALL ON staff FROM ts_app, ts_public, ts_system;

-- One row per staff session. Never deleted; ended in place.
CREATE TABLE staff_sessions (
  id             uuid PRIMARY KEY DEFAULT uuidv7(),
  user_id        uuid NOT NULL REFERENCES staff,
  session_id     uuid NOT NULL,      -- sessions.id; no FK, logout deletes sessions
  organizer_id   uuid REFERENCES organizers ON DELETE SET NULL,
  organizer_slug citext NOT NULL,    -- snapshot, survives organizer deletion
  organizer_name text NOT NULL,      -- snapshot, for the staff banner
  reason         text NOT NULL CHECK (length(reason) BETWEEN 10 AND 500),
  ip             inet,
  user_agent     text,
  started_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz NOT NULL,
  ended_at       timestamptz,
  ended_how      text CHECK (ended_how IN ('staff','logout','revoked','expired')),
  end_note       text CHECK (length(end_note) <= 2000),
  CHECK (expires_at <= started_at + interval '60 minutes')
);
CREATE UNIQUE INDEX staff_sessions_one_live ON staff_sessions (session_id) WHERE ended_at IS NULL;

-- Every request made in staff mode. Append-only for ts_app.
CREATE TABLE staff_access_log (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  staff_session_id uuid NOT NULL REFERENCES staff_sessions,
  organizer_id     uuid,             -- copied from the session, for the organizer's view
  method           text NOT NULL CHECK (length(method) <= 10),
  route            text NOT NULL CHECK (length(route) <= 200), -- the OpenAPI operation id and path params, not the query string
  at               timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON staff_access_log (staff_session_id);

-- ts_app reads both, never writes either: every write goes through the
-- SECURITY DEFINER functions below.
REVOKE ALL ON staff_sessions, staff_access_log FROM ts_app, ts_public, ts_system;
GRANT SELECT ON staff_sessions, staff_access_log TO ts_app;

-- Live staff, by the scoped user. Runs as the owner: ts_app can't read staff.
CREATE FUNCTION app_is_staff() RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT EXISTS (SELECT 1 FROM public.staff s WHERE s.user_id = public.app_user() AND s.revoked_at IS NULL) $$;
REVOKE ALL ON FUNCTION app_is_staff() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_is_staff() TO ts_app;

-- Staff see every staff session and request (their own list); an organizer
-- sees the ones on itself. The HTTP layer narrows that to owners and admins.
ALTER TABLE staff_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE staff_access_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY staff_sessions_read ON staff_sessions FOR SELECT TO ts_app
  USING ((SELECT app_is_staff()) OR organizer_id = (SELECT app_organizer()));
CREATE POLICY staff_access_log_read ON staff_access_log FOR SELECT TO ts_app
  USING ((SELECT app_is_staff()) OR organizer_id = (SELECT app_organizer()));

-- The organizer a live staff session may read. NULL unless every condition
-- holds, re-checked on every statement. Only db.WithStaff sets
-- app.staff_session_id; no other code path does. Called only from
-- app_organizer(), which runs as the owner, so no role gets EXECUTE.
CREATE FUNCTION app_staff_organizer() RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT ss.organizer_id
    FROM public.staff_sessions ss
    JOIN public.staff s     ON s.user_id = ss.user_id AND s.revoked_at IS NULL
    JOIN public.sessions se ON se.id = ss.session_id AND se.user_id = ss.user_id
                            AND se.mfa AND se.expires_at > pg_catalog.now()
   WHERE ss.id = NULLIF(pg_catalog.current_setting('app.staff_session_id', true), '')::uuid
     AND ss.user_id = public.app_user()
     AND ss.organizer_id = NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid
     AND ss.ended_at IS NULL
     AND ss.expires_at > pg_catalog.clock_timestamp()
     AND pg_catalog.current_setting('transaction_read_only') = 'on' $$;
REVOKE ALL ON FUNCTION app_staff_organizer() FROM PUBLIC;

-- 00006's rule, plus a third branch: a live, read-only staff session on
-- this organizer. Never alongside a key.
CREATE OR REPLACE FUNCTION app_organizer() RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT o FROM (SELECT NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid AS o) s
      WHERE (public.app_api_key() IS NULL AND EXISTS (
               SELECT 1 FROM public.memberships m WHERE m.user_id = public.app_user() AND m.organizer_id = s.o))
         OR (public.app_user() IS NULL AND EXISTS (
               SELECT 1 FROM public.api_keys k WHERE k.id = public.app_api_key()
                 AND k.organizer_id = s.o AND k.revoked_at IS NULL))
         OR (public.app_api_key() IS NULL AND public.app_staff_organizer() = s.o) $$;

-- Staff rows in audit_log name the staff user and the staff session, never
-- a key or the system. Only the definer functions below write them: members
-- and the system role can't set the column.
ALTER TABLE audit_log ADD COLUMN actor_staff_session_id uuid REFERENCES staff_sessions,
  ADD CONSTRAINT audit_staff_is_a_user
  CHECK (actor_staff_session_id IS NULL OR (actor_user_id IS NOT NULL AND actor_api_key_id IS NULL AND NOT actor_system));
ALTER POLICY audit_insert ON audit_log
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND NOT actor_system AND actor_staff_session_id IS NULL);
ALTER POLICY audit_system ON audit_log
  WITH CHECK (organizer_id = (SELECT app_scope()) AND actor_system AND actor_user_id IS NULL AND actor_api_key_id IS NULL
              AND actor_staff_session_id IS NULL);

-- Passkey challenges for starting staff mode and for staff actions, tied to
-- the login session that asked and what it asked for.
ALTER TABLE webauthn_challenges DROP CONSTRAINT webauthn_challenges_kind_check,
  ADD CONSTRAINT webauthn_challenges_kind_check CHECK (kind IN ('register', 'login', 'staff', 'staff_action')),
  ADD COLUMN session_id uuid,
  ADD COLUMN purpose text;

-- Starts staff mode on one organizer for the scoped user's own MFA login
-- session, ending any staff session that login session already had. The
-- passkey check happens in Go first. Every unknown slug is the same miss.
-- +goose StatementBegin
CREATE FUNCTION start_staff_session(p_session uuid, p_org_slug citext, p_reason text, p_ip inet, p_ua text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE usr  uuid := public.app_user();
        org  public.organizers%ROWTYPE;
        sid  uuid;
BEGIN
  IF usr IS NULL OR NOT public.app_is_staff() OR NOT EXISTS (
       SELECT 1 FROM public.sessions se
        WHERE se.id = p_session AND se.user_id = usr AND se.mfa AND se.expires_at > pg_catalog.now()) THEN
    RAISE EXCEPTION 'start_staff_session: not allowed' USING ERRCODE = '42501';
  END IF;
  SELECT * INTO org FROM public.organizers o WHERE o.slug = p_org_slug;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'start_staff_session: no such organizer' USING ERRCODE = 'no_data_found';
  END IF;
  -- One that ran out before the sweeper reached it ended at its expiry.
  WITH ended AS (
    UPDATE public.staff_sessions
       SET ended_at = LEAST(pg_catalog.clock_timestamp(), expires_at),
           ended_how = CASE WHEN expires_at <= pg_catalog.clock_timestamp() THEN 'expired' ELSE 'staff' END
     WHERE session_id = p_session AND ended_at IS NULL
    RETURNING id, user_id, organizer_id, ended_how)
  INSERT INTO public.audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
  SELECT e.organizer_id, e.user_id, e.id, 'staff.session_ended', 'staff_session', e.id, pg_catalog.jsonb_build_object('how', e.ended_how)
    FROM ended e WHERE e.organizer_id IS NOT NULL;
  INSERT INTO public.staff_sessions(user_id, session_id, organizer_id, organizer_slug, organizer_name, reason, ip, user_agent, expires_at)
  VALUES (usr, p_session, org.id, org.slug, org.name, p_reason, p_ip, p_ua, pg_catalog.now() + interval '60 minutes')
  RETURNING id INTO sid;
  INSERT INTO public.audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
  VALUES (org.id, usr, sid, 'staff.session_started', 'staff_session', sid, pg_catalog.jsonb_build_object('reason', p_reason));
  RETURN sid;
END $$;

-- Ends one of the scoped user's own staff sessions, once. Revocation and
-- expiry have their own paths (the CLI, the sweeper), so ts_app can't claim
-- either.
CREATE FUNCTION end_staff_session(p_id uuid, p_how text, p_note text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF p_how IS NULL OR p_how NOT IN ('staff', 'logout') THEN
    RAISE EXCEPTION 'end_staff_session: how must be staff or logout' USING ERRCODE = 'check_violation';
  END IF;
  WITH ended AS (
    UPDATE public.staff_sessions SET ended_at = pg_catalog.now(), ended_how = p_how, end_note = p_note
     WHERE id = p_id AND user_id = public.app_user() AND ended_at IS NULL
    RETURNING id, user_id, organizer_id)
  INSERT INTO public.audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
  SELECT e.organizer_id, e.user_id, e.id, 'staff.session_ended', 'staff_session', e.id,
         pg_catalog.jsonb_build_object('how', p_how, 'note', p_note)
    FROM ended e WHERE e.organizer_id IS NOT NULL;
END $$;

-- One row per staff-mode request, written before the request runs. Refuses
-- a session that isn't live or isn't the scoped user's.
CREATE FUNCTION log_staff_access(p_staff_session uuid, p_method text, p_route text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  INSERT INTO public.staff_access_log(staff_session_id, organizer_id, method, route)
  SELECT ss.id, ss.organizer_id, p_method, p_route
    FROM public.staff_sessions ss
    JOIN public.staff s     ON s.user_id = ss.user_id AND s.revoked_at IS NULL
    JOIN public.sessions se ON se.id = ss.session_id AND se.user_id = ss.user_id
                            AND se.mfa AND se.expires_at > pg_catalog.now()
   WHERE ss.id = p_staff_session
     AND ss.user_id = public.app_user()
     AND ss.ended_at IS NULL
     AND ss.expires_at > pg_catalog.clock_timestamp();
  IF NOT FOUND THEN
    RAISE EXCEPTION 'log_staff_access: no live staff session' USING ERRCODE = '42501';
  END IF;
END $$;

-- The sweeper: closes staff sessions past their hour that nobody ended. The
-- audit row names the staff user and session, not the system.
CREATE FUNCTION close_expired_staff_sessions(p_limit int) RETURNS int
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  WITH due AS (
    SELECT id FROM public.staff_sessions
     WHERE ended_at IS NULL AND expires_at <= pg_catalog.clock_timestamp()
     ORDER BY expires_at LIMIT p_limit
     FOR UPDATE SKIP LOCKED),
  ended AS (
    UPDATE public.staff_sessions ss SET ended_at = ss.expires_at, ended_how = 'expired'
      FROM due WHERE ss.id = due.id
    RETURNING ss.id, ss.user_id, ss.organizer_id),
  logged AS (
    INSERT INTO public.audit_log(organizer_id, actor_user_id, actor_staff_session_id, action, entity_type, entity_id, after)
    SELECT e.organizer_id, e.user_id, e.id, 'staff.session_ended', 'staff_session', e.id, '{"how": "expired"}'
      FROM ended e WHERE e.organizer_id IS NOT NULL)
  SELECT count(*)::int FROM ended
$$;

-- Staff find an organizer by name or slug. Empty for anyone else. The
-- caller escapes LIKE's wildcards.
CREATE FUNCTION staff_search_organizers(p_q text) RETURNS TABLE(name text, slug text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.name, o.slug::text FROM public.organizers o
   WHERE public.app_is_staff()
     AND (o.name ILIKE '%' || p_q || '%' OR o.slug::text ILIKE '%' || p_q || '%')
   ORDER BY o.slug LIMIT 20
$$;

-- Names (never emails) of the staff who have had a staff session on the scoped organizer,
-- and nobody else, for the organizer's view of staff access.
CREATE FUNCTION app_staff_directory() RETURNS TABLE(user_id uuid, name text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT u.id, u.name FROM public.users u
   WHERE EXISTS (SELECT 1 FROM public.staff_sessions ss
                  WHERE ss.user_id = u.id AND ss.organizer_id = public.app_organizer())
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION start_staff_session(uuid, citext, text, inet, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION start_staff_session(uuid, citext, text, inet, text) TO ts_app;
REVOKE ALL ON FUNCTION end_staff_session(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION end_staff_session(uuid, text, text) TO ts_app;
REVOKE ALL ON FUNCTION log_staff_access(uuid, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION log_staff_access(uuid, text, text) TO ts_app;
REVOKE ALL ON FUNCTION close_expired_staff_sessions(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION close_expired_staff_sessions(int) TO ts_app;
REVOKE ALL ON FUNCTION staff_search_organizers(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION staff_search_organizers(text) TO ts_app;
REVOKE ALL ON FUNCTION app_staff_directory() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_staff_directory() TO ts_app;

-- +goose Down
DROP FUNCTION app_staff_directory();
DROP FUNCTION staff_search_organizers(text);
DROP FUNCTION close_expired_staff_sessions(int);
DROP FUNCTION log_staff_access(uuid, text, text);
DROP FUNCTION end_staff_session(uuid, text, text);
DROP FUNCTION start_staff_session(uuid, citext, text, inet, text);
DELETE FROM webauthn_challenges WHERE kind IN ('staff', 'staff_action');
ALTER TABLE webauthn_challenges DROP COLUMN purpose, DROP COLUMN session_id,
  DROP CONSTRAINT webauthn_challenges_kind_check,
  ADD CONSTRAINT webauthn_challenges_kind_check CHECK (kind IN ('register', 'login'));
ALTER POLICY audit_system ON audit_log
  WITH CHECK (organizer_id = (SELECT app_scope()) AND actor_system AND actor_user_id IS NULL AND actor_api_key_id IS NULL);
ALTER POLICY audit_insert ON audit_log WITH CHECK (organizer_id = (SELECT app_organizer()) AND NOT actor_system);
-- Without the column a staff row would read as a non-member's action, and
-- the sessions it points at are dropped below anyway.
DELETE FROM audit_log WHERE actor_staff_session_id IS NOT NULL;
ALTER TABLE audit_log DROP CONSTRAINT audit_staff_is_a_user, DROP COLUMN actor_staff_session_id;
CREATE OR REPLACE FUNCTION app_organizer() RETURNS uuid LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT o FROM (SELECT NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid AS o) s
      WHERE (public.app_api_key() IS NULL AND EXISTS (
               SELECT 1 FROM public.memberships m WHERE m.user_id = public.app_user() AND m.organizer_id = s.o))
         OR (public.app_user() IS NULL AND EXISTS (
               SELECT 1 FROM public.api_keys k WHERE k.id = public.app_api_key()
                 AND k.organizer_id = s.o AND k.revoked_at IS NULL)) $$;
DROP FUNCTION app_staff_organizer();
DROP TABLE staff_access_log, staff_sessions;
DROP FUNCTION app_is_staff();
DROP TABLE staff;
