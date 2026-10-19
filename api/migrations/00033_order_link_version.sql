-- +goose Up
-- A buyer who lost their order link can ask for a new one. The token stays
-- an HMAC that's never stored; link_version goes into it, so a new link
-- replaces the old one (order_by_link compares the hash). Version 0 is what
-- every link sent before this hashes, so those keep working. link_sent_at
-- is when the last new link went out, so a flood of requests can't keep
-- killing a working one.
ALTER TABLE orders ADD COLUMN link_version int NOT NULL DEFAULT 0 CHECK (link_version >= 0),
  ADD COLUMN link_sent_at timestamptz;

-- A refund that went through and then came back: the buyer's bank sent the
-- money back to the organizer's Stripe balance.
ALTER TABLE refunds ADD COLUMN returned_by_bank boolean NOT NULL DEFAULT false;

-- The organizer and event behind a storefront address, for the link
-- request job (which runs with no organizer yet). Drafts aren't for sale,
-- so they have no buyers to send links to.
-- +goose StatementBegin
CREATE FUNCTION order_link_scope(p_organizer text, p_event text) RETURNS TABLE(organizer_id uuid, event_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT e.organizer_id, e.id FROM public.events e JOIN public.organizers g ON g.id = e.organizer_id
   WHERE g.slug = p_organizer AND e.slug = p_event AND e.status <> 'draft'
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION order_link_scope(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION order_link_scope(text, text) TO ts_app;

-- order_by_link, as in 00023, now also saying what was refunded (refunds
-- that haven't failed), what the buyer's bank sent back, and whether the
-- event was cancelled. A void ticket's secret stays in the database: it
-- opens nothing, so the page has no QR to show for it.
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

-- +goose Down
DROP FUNCTION order_by_link(text, bytea);
-- +goose StatementBegin
CREATE FUNCTION order_by_link(p_code text, p_token_hash bytea)
RETURNS TABLE(code text, status text, email text, subtotal_cents bigint, discount_cents bigint, tax_cents bigint,
  fee_cents bigint, total_cents bigint, event_name text, timezone text, venue_name text, venue_address text,
  starts_at timestamptz, ends_at timestamptz, organizer_name text, tickets jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT o.code, o.status, o.email::text, o.subtotal_cents, o.discount_cents, o.tax_cents, o.fee_cents, o.total_cents,
         e.name::text, e.timezone::text, e.venue_name, e.venue_address, e.starts_at, e.ends_at, g.name::text,
         coalesce((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', t.id, 'ticket_type_name', tt.name,
                     'attendee_name', t.attendee_name, 'status', t.status, 'secret', pg_catalog.encode(t.secret, 'hex')) ORDER BY t.id)
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
DROP FUNCTION order_link_scope(text, text);
ALTER TABLE refunds DROP COLUMN returned_by_bank;
ALTER TABLE orders DROP COLUMN link_sent_at, DROP COLUMN link_version;
