-- +goose Up
-- Names for the audit log. users is self-only under RLS, so this runs as the
-- schema owner and hands back only the current members of the scoped
-- organizer, and only to a signed-in owner or admin of it (an API key alone gets nothing).
-- search_path ends in pg_temp so a temp table cannot shadow a real one.
CREATE FUNCTION app_member_directory() RETURNS TABLE (user_id uuid, name text, email citext)
LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS
  $$ SELECT u.id, u.name, u.email
       FROM public.memberships m JOIN public.users u ON u.id = m.user_id
      WHERE m.organizer_id = public.app_organizer()
        AND EXISTS (SELECT 1 FROM public.memberships me
                     WHERE me.user_id = public.app_user()
                       AND me.organizer_id = m.organizer_id
                       AND me.role IN ('owner', 'admin')) $$;
REVOKE ALL ON FUNCTION app_member_directory() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_member_directory() TO ts_app;

-- +goose Down
DROP FUNCTION app_member_directory();
