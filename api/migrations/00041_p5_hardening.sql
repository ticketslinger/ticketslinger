-- +goose Up
-- Lets ops and tests find a refund's job by refund id (payload->>'refund_id')
-- without scanning the whole jobs table.
CREATE INDEX jobs_refund_id_idx ON jobs ((payload->>'refund_id')) WHERE kind = 'refund';

-- refunds_within_payment, as in 00034, now naming itself as the constraint,
-- so the app can tell this rule from every other check that fails.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION refunds_within_payment() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  v_amount bigint;
  v_fee bigint;
BEGIN
  SELECT p.amount_cents, p.application_fee_cents INTO v_amount, v_fee
    FROM public.payments p WHERE p.id = NEW.payment_id FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN NEW; -- the foreign key refuses it
  END IF;
  SELECT v_amount - coalesce(sum(r.amount_cents), 0), v_fee - coalesce(sum(r.fee_cents), 0) INTO v_amount, v_fee
    FROM public.refunds r WHERE r.payment_id = NEW.payment_id AND r.status <> 'failed';
  IF NEW.amount_cents > v_amount OR NEW.fee_cents > v_fee THEN
    RAISE EXCEPTION 'refunds: more than the payment has left' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'refunds_within_payment';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- retry_refund, as in 00034, now held to the same rule: a failed refund
-- comes back only if it still fits in what the payment has left, counting
-- every other refund that hasn't failed. The payment is locked first, as the
-- trigger does, so a retry and an insert at once take turns.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION retry_refund(p_refund uuid) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  v_payment uuid;
  v_amount bigint;
  v_fee bigint;
BEGIN
  SELECT r.payment_id INTO v_payment FROM public.refunds r
   WHERE r.id = p_refund AND r.status = 'failed'
     AND r.organizer_id = public.app_organizer()
     AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.user_id = public.app_user()
                   AND m.organizer_id = public.app_organizer() AND m.role IN ('owner', 'admin'));
  IF NOT FOUND THEN
    RETURN false;
  END IF;
  SELECT p.amount_cents, p.application_fee_cents INTO v_amount, v_fee
    FROM public.payments p WHERE p.id = v_payment FOR NO KEY UPDATE;
  SELECT v_amount - coalesce(sum(r.amount_cents), 0), v_fee - coalesce(sum(r.fee_cents), 0) INTO v_amount, v_fee
    FROM public.refunds r WHERE r.payment_id = v_payment AND r.status <> 'failed' AND r.id <> p_refund;
  UPDATE public.refunds
     SET status = 'requested', failure = NULL, returned_by_bank = false, updated_at = pg_catalog.statement_timestamp(),
         attempt = attempt + CASE WHEN failure = 'stripe_failed' THEN 1 ELSE 0 END,
         stripe_refund_id = CASE WHEN failure = 'stripe_failed' THEN NULL ELSE stripe_refund_id END
   WHERE id = p_refund AND status = 'failed' AND organizer_id = public.app_organizer()
     AND amount_cents <= v_amount AND fee_cents <= v_fee;
  RETURN FOUND;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION retry_refund(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION retry_refund(uuid) TO ts_app;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION retry_refund(p_refund uuid) RETURNS boolean LANGUAGE sql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
  WITH r AS (
    UPDATE public.refunds
       SET status = 'requested', failure = NULL, returned_by_bank = false, updated_at = pg_catalog.statement_timestamp(),
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION refunds_within_payment() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  v_amount bigint;
  v_fee bigint;
BEGIN
  SELECT p.amount_cents, p.application_fee_cents INTO v_amount, v_fee
    FROM public.payments p WHERE p.id = NEW.payment_id FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN NEW; -- the foreign key refuses it
  END IF;
  SELECT v_amount - coalesce(sum(r.amount_cents), 0), v_fee - coalesce(sum(r.fee_cents), 0) INTO v_amount, v_fee
    FROM public.refunds r WHERE r.payment_id = NEW.payment_id AND r.status <> 'failed';
  IF NEW.amount_cents > v_amount OR NEW.fee_cents > v_fee THEN
    RAISE EXCEPTION 'refunds: more than the payment has left' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION refunds_within_payment() FROM PUBLIC;
DROP INDEX jobs_refund_id_idx;
