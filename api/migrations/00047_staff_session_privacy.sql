-- +goose Up
-- Where staff mode was started from (ip, user_agent) is for the server's
-- owner role only: ts_app, which organizers and staff both read through,
-- keeps every other column of staff_sessions.
REVOKE SELECT ON staff_sessions FROM ts_app;
GRANT SELECT (id, user_id, session_id, organizer_id, organizer_slug, organizer_name, reason,
              started_at, expires_at, ended_at, ended_how, end_note) ON staff_sessions TO ts_app;

-- The current slug of a staff session's organizer, for the staff member
-- who owns the session: staff mode follows the organizer through a rename,
-- and ts_app can't read an organizer it isn't scoped to.
CREATE FUNCTION staff_session_slug(p_staff_session uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.slug::text
    FROM public.staff_sessions ss
    JOIN public.organizers o ON o.id = ss.organizer_id
   WHERE ss.id = p_staff_session AND ss.user_id = public.app_user() AND public.app_is_staff()
$$;
REVOKE ALL ON FUNCTION staff_session_slug(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION staff_session_slug(uuid) TO ts_app;

-- +goose Down
DROP FUNCTION staff_session_slug(uuid);
REVOKE SELECT (id, user_id, session_id, organizer_id, organizer_slug, organizer_name, reason,
               started_at, expires_at, ended_at, ended_how, end_note) ON staff_sessions FROM ts_app;
GRANT SELECT ON staff_sessions TO ts_app;
