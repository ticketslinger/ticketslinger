-- +goose Up
-- queue_order_email (00035) answered on_its_way for any queued or failing
-- order. A job the queue gave up on whose status write was lost left the
-- order there for good, with nothing on its way and no way to send again.
-- Now on_its_way needs a live order_confirmation job for the order too (not
-- parked: run_at 'infinity'); without one the normal rules decide. Only
-- ts_app and the definer of queue_order_email_by_link can read jobs:
-- ts_system calls this from confirmOrder for a just-paid order, whose
-- mail_status is still null, so it never reaches the jobs check.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION queue_order_email(p_order uuid) RETURNS text LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  o record;
BEGIN
  SELECT id, organizer_id, status, mail_status, mail_requested_at INTO o FROM public.orders WHERE id = p_order FOR UPDATE;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  -- Nested, not AND: a statement naming jobs needs SELECT on it even when
  -- it never reads a row, and ts_system has none.
  IF o.mail_status IN ('queued', 'failing') THEN
    IF EXISTS (SELECT 1 FROM public.jobs j WHERE j.kind = 'order_confirmation'
                AND j.payload->>'order_id' = p_order::text AND j.run_at <> 'infinity') THEN
      RETURN 'on_its_way';
    END IF;
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
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION queue_order_email(p_order uuid) RETURNS text LANGUAGE plpgsql
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
