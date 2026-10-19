-- +goose Up
-- Owners add existing, verified accounts to their team, and take non-owners
-- off it. Both run as the schema owner (ts_app can't write memberships) and
-- act only when the scoped user owns the scoped organizer. Neither can make
-- or remove an owner, and an owner can't remove themself, so the last owner
-- always stays. 'no_account' covers an unknown email and an unverified one
-- alike, so the answer says no more than "not yet". Removing someone also
-- revokes the API keys they created for the organizer.
-- search_path ends in pg_temp so a temp table cannot shadow a real one.
-- +goose StatementBegin
CREATE FUNCTION add_member(p_email citext, p_role text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE org uuid := public.app_organizer();
        usr uuid;
BEGIN
  IF p_role IS NULL OR p_role NOT IN ('admin', 'checkin') THEN
    RAISE EXCEPTION 'add_member: role must be admin or checkin' USING ERRCODE = 'check_violation';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM public.memberships
                  WHERE organizer_id = org AND user_id = public.app_user() AND role = 'owner') THEN
    RETURN 'forbidden';
  END IF;
  SELECT id INTO usr FROM public.users WHERE email = p_email AND email_verified_at IS NOT NULL;
  IF usr IS NULL THEN
    RETURN 'no_account';
  END IF;
  INSERT INTO public.memberships(organizer_id, user_id, role) VALUES (org, usr, p_role)
    ON CONFLICT (organizer_id, user_id) DO NOTHING;
  IF NOT FOUND THEN
    RETURN 'already';
  END IF;
  RETURN 'added';
END $$;

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
  -- A key carries its own authority, so the keys they made here go with them.
  UPDATE public.api_keys SET revoked_at = now()
   WHERE organizer_id = org AND created_by = p_user AND revoked_at IS NULL;
  RETURN 'removed';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION add_member(citext, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION add_member(citext, text) TO ts_app;
REVOKE ALL ON FUNCTION remove_member(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION remove_member(uuid) TO ts_app;

-- +goose Down
DROP FUNCTION remove_member(uuid);
DROP FUNCTION add_member(citext, text);
