-- +goose Up
-- An admin making an API key while an owner removes them could keep it:
-- remove_member revoked the keys it could see, and a key still being made
-- wasn't one of them. Key creation now holds the creator's membership row
-- (hold_membership), and remove_member locks that row FOR UPDATE before it
-- deletes it, so one waits for the other. Either the key lands first and
-- the removal revokes it, or the removal lands first and the key is refused.
-- remove_member now also says which keys it revoked, so the audit log
-- records exactly those, the late one included.
-- search_path ends in pg_temp so a temp table cannot shadow a real one.

-- ts_app has no UPDATE policy on memberships, so RLS hides every row from
-- its own SELECT ... FOR KEY SHARE. This runs as the schema owner instead,
-- and only ever locks the scoped user's row in the scoped organizer. False
-- once they're no longer a member there.
-- +goose StatementBegin
CREATE FUNCTION hold_membership() RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  PERFORM 1 FROM public.memberships
    WHERE organizer_id = NULLIF(pg_catalog.current_setting('app.organizer_id', true), '')::uuid
      AND user_id = public.app_user()
    FOR KEY SHARE;
  RETURN FOUND;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION hold_membership() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION hold_membership() TO ts_app;

-- remove_member, as in 00040, now returning the ids of the keys it revoked.
DROP FUNCTION remove_member(uuid);
-- +goose StatementBegin
CREATE FUNCTION remove_member(p_user uuid, OUT result text, OUT revoked uuid[])
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE org uuid := public.app_organizer();
        r   text;
BEGIN
  revoked := '{}';
  IF NOT EXISTS (SELECT 1 FROM public.memberships
                  WHERE organizer_id = org AND user_id = public.app_user() AND role = 'owner') THEN
    result := 'forbidden';
    RETURN;
  END IF;
  -- Waits for any key they're making here (hold_membership) to land first.
  SELECT role INTO r FROM public.memberships WHERE organizer_id = org AND user_id = p_user FOR UPDATE;
  IF r IS NULL THEN
    result := 'not_found';
    RETURN;
  END IF;
  IF r = 'owner' OR p_user = public.app_user() THEN
    result := 'cannot_remove';
    RETURN;
  END IF;
  DELETE FROM public.memberships WHERE organizer_id = org AND user_id = p_user;
  -- A key carries its own authority, so the keys they made here go with them.
  WITH k AS (
    UPDATE public.api_keys SET revoked_at = now()
     WHERE organizer_id = org AND created_by = p_user AND revoked_at IS NULL
    RETURNING id)
  SELECT coalesce(array_agg(k.id ORDER BY k.id), '{}') INTO revoked FROM k;
  result := 'removed';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION remove_member(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION remove_member(uuid) TO ts_app;

-- +goose Down
DROP FUNCTION remove_member(uuid);
-- +goose StatementBegin
CREATE FUNCTION remove_member(p_user uuid) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE org uuid := public.app_organizer();
        r   text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM public.memberships
                  WHERE organizer_id = org AND user_id = public.app_user() AND role = 'owner') THEN
    RETURN 'forbidden';
  END IF;
  SELECT role INTO r FROM public.memberships WHERE organizer_id = org AND user_id = p_user FOR UPDATE;
  IF r IS NULL THEN
    RETURN 'not_found';
  END IF;
  IF r = 'owner' OR p_user = public.app_user() THEN
    RETURN 'cannot_remove';
  END IF;
  DELETE FROM public.memberships WHERE organizer_id = org AND user_id = p_user;
  UPDATE public.api_keys SET revoked_at = now()
   WHERE organizer_id = org AND created_by = p_user AND revoked_at IS NULL;
  RETURN 'removed';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION remove_member(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION remove_member(uuid) TO ts_app;
DROP FUNCTION hold_membership();
