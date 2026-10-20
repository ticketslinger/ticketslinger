-- +goose Up
-- The staff log also shows what a refund_event did (staff_actions.result).

DROP FUNCTION staff_session_actions(uuid);
-- +goose StatementBegin
CREATE FUNCTION staff_session_actions(p_staff_session uuid)
RETURNS TABLE(id uuid, kind text, event_id uuid, reason text, at timestamptz, finished_at timestamptz, result jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT a.id, a.kind, a.event_id, a.reason, a.at, a.finished_at, a.result
    FROM public.staff_actions a
   WHERE a.staff_session_id = p_staff_session AND public.app_is_staff()
   ORDER BY a.at, a.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION staff_session_actions(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION staff_session_actions(uuid) TO ts_app;

-- +goose Down
DROP FUNCTION staff_session_actions(uuid);
-- +goose StatementBegin
CREATE FUNCTION staff_session_actions(p_staff_session uuid)
RETURNS TABLE(id uuid, kind text, event_id uuid, reason text, at timestamptz, finished_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT a.id, a.kind, a.event_id, a.reason, a.at, a.finished_at
    FROM public.staff_actions a
   WHERE a.staff_session_id = p_staff_session AND public.app_is_staff()
   ORDER BY a.at, a.id
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION staff_session_actions(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION staff_session_actions(uuid) TO ts_app;
