-- +goose Up
CREATE INDEX ON sessions (expires_at);
CREATE INDEX ON sessions (created_at);
CREATE INDEX ON email_tokens (expires_at);
CREATE INDEX ON webauthn_challenges (expires_at);

-- The sweep purges in batches so one pass never holds a long lock.
DROP FUNCTION purge_expired_checkouts();
-- +goose StatementBegin
CREATE FUNCTION purge_expired_checkouts(p_limit int) RETURNS bigint LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH gone AS (
    DELETE FROM public.checkouts
     WHERE id IN (
       SELECT c.id FROM public.checkouts c
        WHERE c.status <> 'completed' AND c.expires_at < now() - interval '7 days'
          AND NOT EXISTS (SELECT 1 FROM public.orders o WHERE o.checkout_id = c.id AND o.status = 'pending')
        ORDER BY c.expires_at LIMIT p_limit)
    RETURNING 1)
  SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION purge_expired_checkouts(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_expired_checkouts(int) TO ts_app;

-- +goose Down
DROP FUNCTION purge_expired_checkouts(int);
-- +goose StatementBegin
CREATE FUNCTION purge_expired_checkouts() RETURNS bigint LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH gone AS (
    DELETE FROM public.checkouts c
     WHERE c.status <> 'completed' AND c.expires_at < now() - interval '7 days'
       AND NOT EXISTS (SELECT 1 FROM public.orders o WHERE o.checkout_id = c.id AND o.status = 'pending')
    RETURNING 1)
  SELECT count(*) FROM gone
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION purge_expired_checkouts() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION purge_expired_checkouts() TO ts_app;
DROP INDEX sessions_expires_at_idx, sessions_created_at_idx, email_tokens_expires_at_idx, webauthn_challenges_expires_at_idx;
