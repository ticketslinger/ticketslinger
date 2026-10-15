-- +goose Up
-- Each refund is its own row with its own Stripe idempotency key
-- (refund-<id>-<attempt>, appfee-<id>-<attempt>). attempt moves on only after
-- Stripe refuses outright, since Stripe replays a refusal for 24 hours.
-- fee_cents is our application fee that goes back with it (0 when it doesn't);
-- fee_refunded_at says it went back, and fee_refund_id is Stripe's id for
-- that when we have one (past Stripe's idempotency window we may not).
ALTER TABLE refunds
  ADD COLUMN fee_cents     bigint NOT NULL DEFAULT 0 CHECK (fee_cents >= 0),
  ADD COLUMN fee_refund_id text UNIQUE CHECK (fee_refund_id IS NULL OR fee_refund_id LIKE 'fr\_%'),
  ADD COLUMN fee_refunded_at timestamptz,
  ADD COLUMN attempt       int NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  ADD COLUMN failure       text CHECK (failure IN ('stripe_failed', 'no_access', 'gave_up')),
  ADD CONSTRAINT refunds_failure_when_failed CHECK ((status = 'failed') = (failure IS NOT NULL)),
  ADD CONSTRAINT refunds_id_organizer_key UNIQUE (id, organizer_id);
-- No production data yet (ruling L71); late refunds written before this gave the whole fee back.
UPDATE refunds r SET fee_cents = p.application_fee_cents, fee_refunded_at = r.updated_at FROM payments p WHERE p.id = r.payment_id AND r.fee_refunded;
CREATE INDEX ON refunds (payment_id);

-- A void ticket keeps its seat until its refund succeeds (released_at), so
-- money still on its way back never frees a seat for someone else.
ALTER TABLE tickets
  ADD COLUMN refund_id   uuid,
  ADD COLUMN released_at timestamptz,
  ADD CONSTRAINT tickets_released_only_void CHECK (released_at IS NULL OR status = 'void'),
  ADD CONSTRAINT tickets_refund_fkey FOREIGN KEY (refund_id, organizer_id) REFERENCES refunds (id, organizer_id);
CREATE INDEX ON tickets (refund_id) WHERE refund_id IS NOT NULL;
DROP INDEX tickets_ticket_type_id_idx;
CREATE INDEX tickets_holding_idx ON tickets (ticket_type_id) WHERE released_at IS NULL;

CREATE OR REPLACE FUNCTION quota_used(p_quota uuid, p_except uuid) RETURNS bigint LANGUAGE sql STABLE
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT (SELECT count(*) FROM public.tickets t
            JOIN public.quota_ticket_types x ON x.ticket_type_id = t.ticket_type_id
           WHERE x.quota_id = p_quota AND t.released_at IS NULL)
       + (SELECT coalesce(sum(h.quantity), 0) FROM public.holds h
            JOIN public.checkouts c ON c.id = h.checkout_id
            JOIN public.quota_ticket_types x ON x.ticket_type_id = h.ticket_type_id
           WHERE x.quota_id = p_quota AND c.status IN ('open', 'paying') AND c.expires_at > now()
             AND c.id IS DISTINCT FROM p_except) $$;

-- +goose Down
CREATE OR REPLACE FUNCTION quota_used(p_quota uuid, p_except uuid) RETURNS bigint LANGUAGE sql STABLE
  SET search_path = pg_catalog, public, pg_temp AS $$
  SELECT (SELECT count(*) FROM public.tickets t
            JOIN public.quota_ticket_types x ON x.ticket_type_id = t.ticket_type_id
           WHERE x.quota_id = p_quota AND t.status = 'valid')
       + (SELECT coalesce(sum(h.quantity), 0) FROM public.holds h
            JOIN public.checkouts c ON c.id = h.checkout_id
            JOIN public.quota_ticket_types x ON x.ticket_type_id = h.ticket_type_id
           WHERE x.quota_id = p_quota AND c.status IN ('open', 'paying') AND c.expires_at > now()
             AND c.id IS DISTINCT FROM p_except) $$;
DROP INDEX tickets_holding_idx;
CREATE INDEX tickets_ticket_type_id_idx ON tickets (ticket_type_id) WHERE status = 'valid';
ALTER TABLE tickets DROP CONSTRAINT tickets_refund_fkey, DROP CONSTRAINT tickets_released_only_void,
  DROP COLUMN released_at, DROP COLUMN refund_id;
DROP INDEX refunds_payment_id_idx;
ALTER TABLE refunds DROP CONSTRAINT refunds_id_organizer_key, DROP CONSTRAINT refunds_failure_when_failed,
  DROP COLUMN failure, DROP COLUMN attempt, DROP COLUMN fee_refunded_at, DROP COLUMN fee_refund_id, DROP COLUMN fee_cents;
