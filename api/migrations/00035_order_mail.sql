-- +goose Up
-- Whether the ticket email reached the buyer. Null: no ticket email yet.
ALTER TABLE orders
  ADD COLUMN mail_status text CHECK (mail_status IN ('queued','sent','failing','gave_up','delivered','bounced','complained')),
  ADD COLUMN mail_problem text CHECK (mail_problem IN ('rate_limited','quota','refused','unreachable','bad_address','config')),
  ADD COLUMN mail_attempts int NOT NULL DEFAULT 0 CHECK (mail_attempts >= 0),
  ADD COLUMN mail_requested_at timestamptz,
  ADD COLUMN mail_updated_at timestamptz;
GRANT UPDATE (mail_status, mail_problem, mail_attempts, mail_requested_at, mail_updated_at) ON orders TO ts_app;

-- Queue the order's ticket email: the one copy of the rules for sending it
-- again, used by the confirmation, the organizer and the buyer alike. Runs
-- as the caller, under its row-level security, so an order the caller can't
-- see is NULL. Otherwise, in this order: on_its_way while one is queued or
-- still failing; too_soon within 10 minutes of the last request; no_tickets
-- unless the order is paid (or partly refunded) with a valid ticket left;
-- else queued, with a fresh order_confirmation job (ids only).
-- +goose StatementBegin
CREATE FUNCTION queue_order_email(p_order uuid) RETURNS text LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  o record;
BEGIN
  SELECT id, organizer_id, status, mail_status, mail_requested_at INTO o FROM public.orders WHERE id = p_order FOR UPDATE;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  IF o.mail_status IN ('queued', 'failing') THEN
    RETURN 'on_its_way';
  END IF;
  IF o.mail_requested_at > now() - interval '10 minutes' THEN
    RETURN 'too_soon';
  END IF;
  IF o.status NOT IN ('paid', 'partially_refunded')
     OR NOT EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = p_order AND t.status = 'valid') THEN
    RETURN 'no_tickets';
  END IF;
  UPDATE public.orders SET mail_status = 'queued', mail_attempts = 0, mail_problem = NULL,
    mail_requested_at = statement_timestamp(), mail_updated_at = statement_timestamp()
   WHERE id = p_order;
  INSERT INTO public.jobs (kind, payload)
    VALUES ('order_confirmation', jsonb_build_object('order_id', o.id, 'organizer_id', o.organizer_id));
  RETURN 'queued';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION queue_order_email(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_order_email(uuid) TO ts_app, ts_system;

-- +goose Down
DROP FUNCTION queue_order_email(uuid);
REVOKE UPDATE (mail_status, mail_problem, mail_attempts, mail_requested_at, mail_updated_at) ON orders FROM ts_app;
ALTER TABLE orders DROP COLUMN mail_updated_at, DROP COLUMN mail_requested_at, DROP COLUMN mail_attempts,
  DROP COLUMN mail_problem, DROP COLUMN mail_status;
