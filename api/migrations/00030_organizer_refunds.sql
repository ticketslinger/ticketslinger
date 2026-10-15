-- +goose Up
-- An organizer's refund runs in its own transaction as ts_app, under the
-- tenant policies, and writes only this: tickets voided and pointed at their
-- refund (or released, when they cost nothing), the order's status, and a
-- requested refund row. Everything after that (Stripe's answers, a retry)
-- goes through the system (ts_system) or retry_refund below, so a member,
-- even with raw SQL, can't mark money as refunded.
GRANT UPDATE (status, refund_id, released_at) ON tickets TO ts_app;
GRANT UPDATE (status) ON orders TO ts_app;
GRANT INSERT (organizer_id, payment_id, amount_cents, fee_refunded, fee_cents, status, reason, actor_user_id) ON refunds TO ts_app;
-- Restrictive, so it narrows refunds_scope rather than widening it: a person
-- (not a key) asks for a refund, and it starts out requested.
CREATE POLICY refunds_app_insert ON refunds AS RESTRICTIVE FOR INSERT TO ts_app
  WITH CHECK (organizer_id = (SELECT app_organizer()) AND (SELECT app_user()) IS NOT NULL
    AND status = 'requested' AND reason IN ('organizer', 'event_cancelled'));

-- When the job first asks Stripe for this refund. A row sent before that has
-- no Stripe id may exist at Stripe already, so the job looks before sending again.
ALTER TABLE refunds ADD COLUMN sent_at timestamptz;

-- ts_app moves tickets one way only: valid to void, pointed at a refund once,
-- released once. Nothing it does brings a refunded ticket back.
-- +goose StatementBegin
CREATE FUNCTION tickets_app_one_way() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF current_user = 'ts_app' AND (
       (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'valid' AND NEW.status = 'void'))
    OR (NEW.refund_id IS DISTINCT FROM OLD.refund_id AND OLD.refund_id IS NOT NULL)
    OR (NEW.released_at IS DISTINCT FROM OLD.released_at AND OLD.released_at IS NOT NULL)) THEN
    RAISE EXCEPTION 'tickets: only valid to void, once' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER tickets_app_one_way BEFORE UPDATE ON tickets FOR EACH ROW EXECUTE FUNCTION tickets_app_one_way();

-- "Try again" on a failed refund: the same row back to requested. After a
-- refusal Stripe has on record as failed, the row moves to its next key (the
-- old one would replay the failure); otherwise it keeps its key, so a first
-- try Stripe made after all is found rather than made twice. Only an owner or
-- admin of the scoped organizer, as a person; true when the row was retried.
-- +goose StatementBegin
CREATE FUNCTION retry_refund(p_refund uuid) RETURNS boolean LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH r AS (
    UPDATE public.refunds
       SET status = 'requested', failure = NULL, updated_at = pg_catalog.statement_timestamp(),
           attempt = attempt + CASE WHEN failure = 'stripe_failed' THEN 1 ELSE 0 END,
           stripe_refund_id = CASE WHEN failure = 'stripe_failed' THEN NULL ELSE stripe_refund_id END
     WHERE id = p_refund AND status = 'failed'
       AND organizer_id = public.app_organizer()
       AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.user_id = public.app_user()
                     AND m.organizer_id = public.app_organizer() AND m.role IN ('owner', 'admin'))
    RETURNING 1)
  SELECT EXISTS (SELECT 1 FROM r)
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION retry_refund(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION retry_refund(uuid) TO ts_app;

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
DROP FUNCTION retry_refund(uuid);
DROP TRIGGER tickets_app_one_way ON tickets;
DROP FUNCTION tickets_app_one_way();
ALTER TABLE refunds DROP COLUMN sent_at;
DROP POLICY refunds_app_insert ON refunds;
REVOKE INSERT ON refunds FROM ts_app;
REVOKE UPDATE (status) ON orders FROM ts_app;
REVOKE UPDATE (status, refund_id, released_at) ON tickets FROM ts_app;
