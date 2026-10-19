-- +goose Up
-- An order still pending an hour after its hold ended is settled by a job:
-- Stripe is asked what happened to its payment, and an unpaid one is
-- cancelled there so it can never be charged later. settle_queued_at marks
-- the order queued, once.
ALTER TABLE orders ADD COLUMN settle_queued_at timestamptz;
ALTER TABLE payments DROP CONSTRAINT payments_status_check,
  ADD CONSTRAINT payments_status_check CHECK (status IN ('pending', 'succeeded', 'failed', 'cancelled'));

-- +goose StatementBegin
CREATE FUNCTION queue_abandoned_orders(p_limit int) RETURNS bigint LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH due AS (
    SELECT o.id, o.organizer_id FROM public.orders o JOIN public.checkouts c ON c.id = o.checkout_id
     WHERE o.status = 'pending' AND o.settle_queued_at IS NULL AND c.expires_at < now() - interval '1 hour'
     ORDER BY c.expires_at LIMIT p_limit FOR UPDATE OF o SKIP LOCKED),
  marked AS (
    UPDATE public.orders o SET settle_queued_at = now() FROM due WHERE o.id = due.id RETURNING o.id, o.organizer_id),
  queued AS (
    INSERT INTO public.jobs (kind, payload)
    SELECT 'settle_order', pg_catalog.jsonb_build_object('organizer_id', organizer_id, 'order_id', id) FROM marked RETURNING 1)
  SELECT count(*) FROM queued
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION queue_abandoned_orders(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_abandoned_orders(int) TO ts_app;

-- Pay locks its event FOR SHARE while it stores an intent, so a cancellation
-- can't slip in between. A row lock needs UPDATE rights; WITH CHECK (false)
-- means no row can actually be written.
CREATE POLICY events_system_lock ON events FOR UPDATE TO ts_system
  USING (organizer_id = (SELECT app_scope())) WITH CHECK (false);
GRANT UPDATE (updated_at) ON events TO ts_system;

-- +goose Down
REVOKE UPDATE (updated_at) ON events FROM ts_system;
DROP POLICY events_system_lock ON events;
DROP FUNCTION queue_abandoned_orders(int);
-- The narrower check has no 'cancelled': an intent stopped unpaid is failed.
ALTER TABLE payments DROP CONSTRAINT payments_status_check;
UPDATE payments SET status = 'failed' WHERE status = 'cancelled';
ALTER TABLE payments
  ADD CONSTRAINT payments_status_check CHECK (status IN ('pending', 'succeeded', 'failed'));
ALTER TABLE orders DROP COLUMN settle_queued_at;
