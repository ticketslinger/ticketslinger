-- +goose Up
-- An organizer's refund runs in its own transaction as ts_app, under the
-- tenant policies, and writes only this: tickets voided and pointed at their
-- refund (or released, when they cost nothing), the order's status, and the
-- refund row itself. "Try again" puts a failed row back to requested; after
-- a refusal Stripe has on record as failed, it moves the row to its next key.
-- Stripe's answers are written by the system (ts_system).
GRANT UPDATE (status, refund_id, released_at) ON tickets TO ts_app;
GRANT UPDATE (status) ON orders TO ts_app;
GRANT INSERT ON refunds TO ts_app;
GRANT UPDATE (status, failure, attempt, stripe_refund_id, updated_at) ON refunds TO ts_app;

-- Whether a refund's stuck fee still needs its audit row: none written yet,
-- and no other job for the refund waiting to run (one that's running now is
-- left to finish on its own). ts_system reads neither the audit log nor the
-- queue, so this answers for the scoped organizer's own refunds only.
-- +goose StatementBegin
CREATE FUNCTION refund_fee_stuck_due(p_refund uuid) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT EXISTS (SELECT 1 FROM public.refunds r WHERE r.id = p_refund AND r.organizer_id = public.app_scope())
     AND NOT EXISTS (SELECT 1 FROM public.audit_log a
                      WHERE a.organizer_id = public.app_scope() AND a.entity_type = 'refund' AND a.entity_id = p_refund
                        AND a.action = 'refund.fee_stuck')
     AND NOT EXISTS (SELECT 1 FROM public.jobs j
                      WHERE j.kind = 'refund' AND j.payload->>'refund_id' = p_refund::text AND j.run_at <> 'infinity'
                        AND (j.locked_until IS NULL OR j.locked_until < now()))
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION refund_fee_stuck_due(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION refund_fee_stuck_due(uuid) TO ts_system;

-- +goose Down
DROP FUNCTION refund_fee_stuck_due(uuid);
REVOKE UPDATE (status, failure, attempt, stripe_refund_id, updated_at) ON refunds FROM ts_app;
REVOKE INSERT ON refunds FROM ts_app;
REVOKE UPDATE (status) ON orders FROM ts_app;
REVOKE UPDATE (status, refund_id, released_at) ON tickets FROM ts_app;
