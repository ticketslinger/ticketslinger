-- +goose Up
-- A block holds a valid ticket back at the door: the seat stays sold, the
-- QR stays off every buyer path, and an owner or admin can lift it.
ALTER TABLE tickets
  ADD COLUMN blocked_at timestamptz,
  ADD COLUMN blocked_reason text CHECK (char_length(blocked_reason) <= 200),
  ADD CONSTRAINT tickets_block_reason CHECK (blocked_reason IS NULL OR blocked_at IS NOT NULL);
GRANT UPDATE (blocked_at, blocked_reason) ON tickets TO ts_app;

-- ts_app moves tickets one way only: valid to void, pointed at a refund once,
-- released once, and only as it voids them; checked in once, from nothing to
-- the time of the check-in itself (never back- or forward-dated), and only
-- while valid. Nothing it does brings a refunded ticket back or un-checks one.
-- A block is set (at the time of the statement) or lifted only on a valid
-- ticket, and only by a signed-in owner or admin: no key, no check-in member.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_app_one_way() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF current_user = 'ts_app' AND (
       (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'valid' AND NEW.status = 'void'))
    OR (NEW.refund_id IS DISTINCT FROM OLD.refund_id AND OLD.refund_id IS NOT NULL)
    OR (NEW.released_at IS DISTINCT FROM OLD.released_at AND OLD.released_at IS NOT NULL)
    OR ((NEW.refund_id IS DISTINCT FROM OLD.refund_id OR NEW.released_at IS DISTINCT FROM OLD.released_at)
        AND NEW.status IS DISTINCT FROM 'void')
    OR (NEW.checked_in_at IS DISTINCT FROM OLD.checked_in_at
        AND (OLD.checked_in_at IS NOT NULL OR NEW.checked_in_at IS NULL
             OR NEW.checked_in_at <> statement_timestamp() OR OLD.status <> 'valid'))
    OR ((NEW.blocked_at IS DISTINCT FROM OLD.blocked_at OR NEW.blocked_reason IS DISTINCT FROM OLD.blocked_reason)
        AND (OLD.status <> 'valid' OR NEW.status <> 'valid'
             OR NEW.blocked_at <> statement_timestamp()
             OR public.app_user() IS NULL
             OR NOT EXISTS (SELECT 1 FROM public.memberships m WHERE m.organizer_id = public.app_organizer()
                              AND m.user_id = public.app_user() AND m.role IN ('owner', 'admin'))))) THEN
    RAISE EXCEPTION 'tickets: one way only: valid to void, checked in once, blocked while valid' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

ALTER TABLE checkins DROP CONSTRAINT checkins_result_check,
  ADD CONSTRAINT checkins_result_check CHECK (result IN ('accepted', 'duplicate', 'invalid', 'void', 'blocked'));

-- The three writes ts_app has no column grant for. Each is a definer that
-- checks again that the caller is a signed-in owner or admin of the scoped
-- organizer, and that the row is that organizer's. Refusals the dashboard
-- shows are 22023 with a short code as the message.

-- Move a ticket to another type of its event at the same face price (any
-- type, for a comp order). Quotas are the caller's: it holds their locks.
-- +goose StatementBegin
CREATE FUNCTION set_ticket_type(p_ticket uuid, p_type uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  k record;
  new_price bigint;
BEGIN
  IF public.app_user() IS NULL OR NOT EXISTS (SELECT 1 FROM public.memberships m WHERE m.organizer_id = public.app_organizer()
                                                AND m.user_id = public.app_user() AND m.role IN ('owner', 'admin')) THEN
    RAISE EXCEPTION '%: owners and admins only', 'set_ticket_type' USING ERRCODE = 'insufficient_privilege';
  END IF;
  SELECT t.event_id, t.ticket_type_id, t.status, t.checked_in_at, o.source, tt.price_cents INTO k
    FROM public.tickets t
    JOIN public.orders o ON o.id = t.order_id
    JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
   WHERE t.id = p_ticket AND t.organizer_id = public.app_organizer()
     FOR UPDATE OF t;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'set_ticket_type: not this organizer''s ticket' USING ERRCODE = 'insufficient_privilege';
  END IF;
  IF k.status <> 'valid' THEN
    RAISE EXCEPTION 'ticket_void' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF k.checked_in_at IS NOT NULL THEN
    RAISE EXCEPTION 'checked_in' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF k.ticket_type_id = p_type THEN
    RAISE EXCEPTION 'same_type' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT tt.price_cents INTO new_price FROM public.ticket_types tt WHERE tt.id = p_type AND tt.event_id = k.event_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'unknown_ticket_type' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF new_price <> k.price_cents AND k.source <> 'comp' THEN
    RAISE EXCEPTION 'price_differs' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  UPDATE public.tickets SET ticket_type_id = p_type WHERE id = p_ticket;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION set_ticket_type(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION set_ticket_type(uuid, uuid) TO ts_app;

-- Give a ticket a new QR: the caller's 16 random bytes replace the old ones,
-- which then scan as unknown.
-- +goose StatementBegin
CREATE FUNCTION set_ticket_secret(p_ticket uuid, p_secret bytea) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  k record;
BEGIN
  IF public.app_user() IS NULL OR NOT EXISTS (SELECT 1 FROM public.memberships m WHERE m.organizer_id = public.app_organizer()
                                                AND m.user_id = public.app_user() AND m.role IN ('owner', 'admin')) THEN
    RAISE EXCEPTION '%: owners and admins only', 'set_ticket_secret' USING ERRCODE = 'insufficient_privilege';
  END IF;
  IF p_secret IS NULL OR length(p_secret) <> 16 THEN
    RAISE EXCEPTION 'bad_secret' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT t.status, t.checked_in_at INTO k FROM public.tickets t
   WHERE t.id = p_ticket AND t.organizer_id = public.app_organizer() FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'set_ticket_secret: not this organizer''s ticket' USING ERRCODE = 'insufficient_privilege';
  END IF;
  IF k.status <> 'valid' THEN
    RAISE EXCEPTION 'ticket_void' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF k.checked_in_at IS NOT NULL THEN
    RAISE EXCEPTION 'checked_in' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  -- secret is UNIQUE, and a unique_violation's detail would quote the bytes.
  BEGIN
    UPDATE public.tickets SET secret = p_secret WHERE id = p_ticket;
  EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'bad_secret' USING ERRCODE = 'invalid_parameter_value';
  END;
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION set_ticket_secret(uuid, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION set_ticket_secret(uuid, bytea) TO ts_app;

-- Send the tickets again now, skipping queue_order_email's ten-minute
-- throttle (after a new QR, the buyer needs it). A waiting job hasn't read
-- the tickets yet, so it's on its way; a running one has (the job reads
-- them, then sends), so a fresh job follows it. A failing order's waiting
-- job is brought forward with its attempts reset, never doubled.
-- +goose StatementBegin
CREATE FUNCTION force_order_email(p_order uuid) RETURNS text LANGUAGE plpgsql SECURITY DEFINER
  SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  o record;
  waiting uuid;
BEGIN
  IF public.app_user() IS NULL OR NOT EXISTS (SELECT 1 FROM public.memberships m WHERE m.organizer_id = public.app_organizer()
                                                AND m.user_id = public.app_user() AND m.role IN ('owner', 'admin')) THEN
    RAISE EXCEPTION '%: owners and admins only', 'force_order_email' USING ERRCODE = 'insufficient_privilege';
  END IF;
  SELECT id, organizer_id, status, mail_status INTO o FROM public.orders
   WHERE id = p_order AND organizer_id = public.app_organizer() FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'force_order_email: not this organizer''s order' USING ERRCODE = 'insufficient_privilege';
  END IF;
  -- Whatever mail_status says: a running job's send can mark the order
  -- 'sent' while the job queued beside it still waits.
  SELECT j.id INTO waiting FROM public.jobs j
   WHERE j.kind = 'order_confirmation' AND j.payload->>'order_id' = p_order::text AND j.run_at <> 'infinity'
     AND (j.locked_until IS NULL OR j.locked_until < now())
   ORDER BY j.run_at LIMIT 1 FOR UPDATE;
  IF FOUND AND o.mail_status IS DISTINCT FROM 'failing' THEN
    RETURN 'on_its_way';
  END IF;
  IF FOUND THEN
    UPDATE public.jobs SET run_at = statement_timestamp(), attempts = 0, locked_until = NULL WHERE id = waiting;
    UPDATE public.orders SET mail_status = 'queued', mail_attempts = 0, mail_problem = NULL,
      mail_requested_at = statement_timestamp(), mail_updated_at = statement_timestamp()
     WHERE id = p_order;
    RETURN 'queued';
  END IF;
  IF o.status NOT IN ('paid', 'partially_refunded')
     OR NOT EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = p_order AND t.status = 'valid' AND t.blocked_at IS NULL) THEN
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
REVOKE ALL ON FUNCTION force_order_email(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION force_order_email(uuid) TO ts_app;

-- queue_order_email, as in 00038, with a blocked ticket counting as nothing
-- to send.
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
     OR NOT EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = p_order AND t.status = 'valid' AND t.blocked_at IS NULL) THEN
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

-- order_by_link, as in 00049, saying which tickets are blocked and giving
-- no QR for them.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, names_until timestamptz, source text, manual_method text, manual_cents bigint, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text, e.status::text,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status <> 'failed'), 0)::bigint,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status = 'failed' AND r.returned_by_bank), 0)::bigint,
         CASE WHEN o.status IN ('paid', 'partially_refunded') AND e.status = 'live' AND NOT public.organizer_suspended(o.organizer_id)
                   AND statement_timestamp() < least(coalesce(e.names_locked_at, e.starts_at), e.starts_at)
              THEN least(coalesce(e.names_locked_at, e.starts_at), e.starts_at) END,
         o.source, o.manual_method,
         CASE WHEN o.source <> 'manual' THEN 0
              WHEN EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              THEN (SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              ELSE coalesce((SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id
                              AND t.released_at = (SELECT max(l.released_at) FROM public.tickets l WHERE l.order_id = o.id)), 0)
         END::bigint,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'refunded', t.refund_id IS NOT NULL, 'name_changes', t.name_changes,
                     'blocked', t.blocked_at IS NOT NULL,
                     'secret', CASE WHEN t.status = 'valid' AND t.blocked_at IS NULL THEN pg_catalog.encode(t.secret, 'hex') END) ORDER BY t.id)
                   FROM public.tickets t JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
                  WHERE t.order_id = o.id), '[]'::jsonb)
    FROM public.orders o
    JOIN public.events e ON e.id = o.event_id
    JOIN public.organizers g ON g.id = o.organizer_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_by_link(text, bytea) TO ts_app;

-- +goose Down
DROP FUNCTION force_order_email(uuid);
DROP FUNCTION set_ticket_secret(uuid, bytea);
DROP FUNCTION set_ticket_type(uuid, uuid);
-- order_by_link back to 00049.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, names_until timestamptz, source text, manual_method text, manual_cents bigint, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text, e.status::text,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status <> 'failed'), 0)::bigint,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status = 'failed' AND r.returned_by_bank), 0)::bigint,
         CASE WHEN o.status IN ('paid', 'partially_refunded') AND e.status = 'live' AND NOT public.organizer_suspended(o.organizer_id)
                   AND statement_timestamp() < least(coalesce(e.names_locked_at, e.starts_at), e.starts_at)
              THEN least(coalesce(e.names_locked_at, e.starts_at), e.starts_at) END,
         o.source, o.manual_method,
         CASE WHEN o.source <> 'manual' THEN 0
              WHEN EXISTS (SELECT 1 FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              THEN (SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id AND t.status = 'valid')
              ELSE coalesce((SELECT sum(t.charge_cents) FROM public.tickets t WHERE t.order_id = o.id
                              AND t.released_at = (SELECT max(l.released_at) FROM public.tickets l WHERE l.order_id = o.id)), 0)
         END::bigint,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'refunded', t.refund_id IS NOT NULL, 'name_changes', t.name_changes,
                     'secret', CASE WHEN t.status = 'valid' THEN pg_catalog.encode(t.secret, 'hex') END) ORDER BY t.id)
                   FROM public.tickets t JOIN public.ticket_types tt ON tt.id = t.ticket_type_id
                  WHERE t.order_id = o.id), '[]'::jsonb)
    FROM public.orders o
    JOIN public.events e ON e.id = o.event_id
    JOIN public.organizers g ON g.id = o.organizer_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_by_link(text, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_by_link(text, bytea) TO ts_app;
-- queue_order_email back to 00038.
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
-- tickets_app_one_way back to 00039.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tickets_app_one_way() RETURNS trigger LANGUAGE plpgsql
  SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF current_user = 'ts_app' AND (
       (NEW.status IS DISTINCT FROM OLD.status AND NOT (OLD.status = 'valid' AND NEW.status = 'void'))
    OR (NEW.refund_id IS DISTINCT FROM OLD.refund_id AND OLD.refund_id IS NOT NULL)
    OR (NEW.released_at IS DISTINCT FROM OLD.released_at AND OLD.released_at IS NOT NULL)
    OR ((NEW.refund_id IS DISTINCT FROM OLD.refund_id OR NEW.released_at IS DISTINCT FROM OLD.released_at)
        AND NEW.status IS DISTINCT FROM 'void')
    OR (NEW.checked_in_at IS DISTINCT FROM OLD.checked_in_at
        AND (OLD.checked_in_at IS NOT NULL OR NEW.checked_in_at IS NULL
             OR NEW.checked_in_at <> statement_timestamp() OR OLD.status <> 'valid'))) THEN
    RAISE EXCEPTION 'tickets: one way only: valid to void, checked in once' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
-- Fails while a 'blocked' check-in row exists: those are kept, not deleted.
-- To go down anyway, remap them by hand first:
--   UPDATE checkins SET result = 'invalid' WHERE result = 'blocked';
ALTER TABLE checkins DROP CONSTRAINT checkins_result_check,
  ADD CONSTRAINT checkins_result_check CHECK (result IN ('accepted', 'duplicate', 'invalid', 'void'));
ALTER TABLE tickets DROP CONSTRAINT tickets_block_reason, DROP COLUMN blocked_reason, DROP COLUMN blocked_at;
