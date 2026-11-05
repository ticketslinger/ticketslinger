-- +goose Up
-- Attendee names the buyer can fix from the order link, until the event
-- starts or the organizer locks names (names_locked_at), whichever is first.
-- A rename is a correction, not a transfer: the ticket's secret stays.
-- name_changes caps the buyer at five per ticket; the organizer has no cap
-- and renames as ts_app under tickets_scope.
ALTER TABLE events ADD COLUMN names_locked_at timestamptz;
ALTER TABLE tickets ADD COLUMN name_changes int NOT NULL DEFAULT 0
  CHECK (name_changes BETWEEN 0 AND 5);
GRANT UPDATE (attendee_name) ON tickets TO ts_app;   -- organizer path, under tickets_scope RLS

-- The buyer's rename, for the order whose code and link token match, in one
-- statement as order_by_link does: NULL for an unknown code, a wrong token
-- or a ticket of another order, so none of them tells the others apart.
-- The check here is a backstop for length, edge whitespace and controls;
-- the API's cleanName runs first and also refuses format characters.
-- +goose StatementBegin
CREATE FUNCTION rename_ticket_by_link(p_code text, p_token_hash bytea, p_ticket uuid, p_name text)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  t record;
BEGIN
  IF p_name IS NULL OR char_length(p_name) > 120 OR p_name <> pg_catalog.btrim(p_name)
     OR p_name ~ '[[:cntrl:]]' THEN
    RAISE EXCEPTION 'bad_attendee' USING ERRCODE = '22023';
  END IF;
  SELECT tk.id, tk.organizer_id, tk.attendee_name, tk.name_changes, tk.status,
         o.status AS order_status, e.status AS event_status,
         least(coalesce(e.names_locked_at, e.starts_at), e.starts_at) AS lock_at
    INTO t
    FROM public.tickets tk
    JOIN public.orders o ON o.id = tk.order_id
    JOIN public.events e ON e.id = tk.event_id
   WHERE o.code = pg_catalog.upper(p_code) AND o.magic_token_hash = p_token_hash AND tk.id = p_ticket
   FOR UPDATE OF tk;
  IF NOT FOUND THEN
    RETURN NULL;                                  -- same answer as a wrong token
  END IF;
  IF t.status <> 'valid' OR t.order_status NOT IN ('paid', 'partially_refunded')
     OR t.event_status <> 'live' OR statement_timestamp() >= t.lock_at
     OR public.organizer_suspended(t.organizer_id) THEN
    RETURN 'locked';
  END IF;
  IF t.attendee_name = p_name THEN
    RETURN 'unchanged';
  END IF;
  IF t.name_changes >= 5 THEN
    RETURN 'limit';
  END IF;
  UPDATE public.tickets SET attendee_name = p_name, name_changes = name_changes + 1 WHERE id = t.id;
  INSERT INTO public.audit_log (organizer_id, actor_system, action, entity_type, entity_id, before, after)
  VALUES (t.organizer_id, true, 'ticket.renamed', 'ticket', t.id,
          pg_catalog.jsonb_build_object('attendee_name', t.attendee_name),
          pg_catalog.jsonb_build_object('attendee_name', p_name, 'via', 'order_link'));
  RETURN 'renamed';
END $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION rename_ticket_by_link(text, bytea, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION rename_ticket_by_link(text, bytea, uuid, text) TO ts_public;

-- order_by_link, as in 00033, now also saying until when the buyer may
-- rename (NULL when they can't now, by rename_ticket_by_link's rules) and
-- how many of the five changes each ticket has used.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, names_until timestamptz, tickets jsonb)
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

-- +goose Down
-- order_by_link back to 00033.
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, event_status text, refunded_cents bigint,
  returned_cents bigint, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text, e.status::text,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status <> 'failed'), 0)::bigint,
         coalesce((SELECT sum(r.amount_cents) FROM public.refunds r JOIN public.payments p ON p.id = r.payment_id
                    WHERE p.order_id = o.id AND r.status = 'failed' AND r.returned_by_bank), 0)::bigint,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'refunded', t.refund_id IS NOT NULL,
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
DROP FUNCTION rename_ticket_by_link(text, bytea, uuid, text);
REVOKE UPDATE (attendee_name) ON tickets FROM ts_app;
ALTER TABLE tickets DROP COLUMN name_changes;
ALTER TABLE events DROP COLUMN names_locked_at;
