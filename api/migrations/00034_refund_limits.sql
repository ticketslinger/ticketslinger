-- +goose Up
-- retry_refund, as in 00030, now also clearing returned_by_bank: the row is
-- sent again, so what the bank did with the last try no longer describes it.
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

-- The app's refund rule, kept by the database too: a new refund fits in what
-- the payment has left, counting every refund that hasn't failed, and its fee
-- in what's left of the fee the payment carried. The payment row is locked
-- first, so two inserts at once take turns and the second sees the first.
-- Definer, to lock and read past the inserting role's grants and policies.
-- +goose StatementBegin
CREATE FUNCTION refunds_within_payment() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
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
CREATE TRIGGER refunds_within_payment BEFORE INSERT ON refunds FOR EACH ROW EXECUTE FUNCTION refunds_within_payment();

-- +goose Down
DROP TRIGGER refunds_within_payment ON refunds;
DROP FUNCTION refunds_within_payment();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION retry_refund(p_refund uuid) RETURNS boolean LANGUAGE sql SECURITY DEFINER
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
